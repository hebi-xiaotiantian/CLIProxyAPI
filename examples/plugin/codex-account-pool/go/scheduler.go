package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxAffinityEntries = 10000

type selector struct {
	now            func() time.Time
	mu             sync.Mutex
	weightRevision uint64
	weights        map[string]map[string]int
	affinity       map[string]affinityEntry
}

type affinityEntry struct {
	AuthID    string
	Revision  uint64
	ExpiresAt time.Time
}

type selectionCandidate struct {
	ID       string
	Policy   AccountPolicy
	Plan     PlanKind
	Backup   bool
	Tier     int
	Priority int
}

func newSelector(now func() time.Time) *selector {
	if now == nil {
		now = time.Now
	}
	return &selector{
		now:      now,
		weights:  make(map[string]map[string]int),
		affinity: make(map[string]affinityEntry),
	}
}

func (s *selector) clone() *selector {
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := newSelector(s.now)
	cloned.weightRevision = s.weightRevision
	for key, weights := range s.weights {
		copyWeights := make(map[string]int, len(weights))
		for authID, value := range weights {
			copyWeights[authID] = value
		}
		cloned.weights[key] = copyWeights
	}
	for sessionKey, entry := range s.affinity {
		cloned.affinity[sessionKey] = entry
	}
	return cloned
}

func (s *selector) pick(req pluginapi.SchedulerPickRequest, policy PolicyDocument, quota QuotaDocument, cfg Config) (pluginapi.SchedulerPickResponse, error) {
	codexCandidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if strings.EqualFold(strings.TrimSpace(candidate.Provider), "codex") {
			codexCandidates = append(codexCandidates, candidate)
		}
	}
	if len(codexCandidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	now := s.now()
	profile := effectiveProfile(policy, now)
	eligible := make([]selectionCandidate, 0, len(codexCandidates))
	for _, candidate := range codexCandidates {
		accountPolicy, ok := policy.Accounts[candidate.ID]
		if !ok {
			accountPolicy = defaultAccountPolicy(candidate.Priority)
		}
		item, ok := classifyCandidate(candidate, accountPolicy, quota.Accounts[candidate.ID], profile, policy.CustomPlanOrder, cfg, now)
		if ok {
			eligible = append(eligible, item)
		}
	}
	if len(eligible) == 0 {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("codex account pool has no eligible account")
	}

	group := strictGroup(eligible)
	if len(group) == 0 {
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("codex account pool has no selectable account")
	}
	sessionKey := schedulerSessionKey(req.Options.Headers, req.Options.Metadata)
	if sessionKey != "" {
		if authID := s.affinityHit(sessionKey, policy.Revision, group, now, cfg.AffinityTTL); authID != "" {
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: authID}, nil
		}
	}
	authID := s.weightedPick(policy.Revision, profile, group)
	if sessionKey != "" {
		s.bindAffinity(sessionKey, authID, policy.Revision, now.Add(cfg.AffinityTTL))
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: authID}, nil
}

func classifyCandidate(candidate pluginapi.SchedulerAuthCandidate, policy AccountPolicy, snapshot QuotaSnapshot, profile RouteProfile, customOrder []PlanKind, cfg Config, now time.Time) (selectionCandidate, bool) {
	item, ok, _ := classifyCandidateWithReason(candidate, policy, snapshot, profile, customOrder, cfg, now)
	return item, ok
}

func classifyCandidateWithReason(candidate pluginapi.SchedulerAuthCandidate, policy AccountPolicy, snapshot QuotaSnapshot, profile RouteProfile, customOrder []PlanKind, cfg Config, now time.Time) (selectionCandidate, bool, string) {
	if !policy.Enabled {
		return selectionCandidate{}, false, "plugin_disabled"
	}
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	if status == "disabled" || status == "unavailable" || status == "error" {
		return selectionCandidate{}, false, "host_" + status
	}

	plan := policy.PlanOverride
	if plan == "" {
		plan = snapshot.Plan
	}
	if !validPlan(plan) {
		plan = PlanUnknown
	}
	hasQuotaWindow := snapshot.FiveHourWindowPresent || snapshot.WeeklyWindowPresent
	fresh := hasQuotaWindow && !snapshot.RefreshedAt.IsZero() && now.Sub(snapshot.RefreshedAt) <= cfg.SnapshotMaxAge
	if !fresh && cfg.StalePolicy == StaleExclude {
		return selectionCandidate{}, false, "quota_stale"
	}
	if fresh {
		if snapshot.FiveHourWindowPresent && snapshot.FiveHourRemaining < policy.FiveHourReserve {
			return selectionCandidate{}, false, "five_hour_reserve"
		}
		if snapshot.WeeklyWindowPresent && snapshot.WeeklyRemaining < policy.WeeklyReserve {
			return selectionCandidate{}, false, "weekly_reserve"
		}
	}

	tier, ok := profileTier(profile, plan, policy.Backup, customOrder)
	if !ok {
		return selectionCandidate{}, false, "profile_excluded"
	}
	return selectionCandidate{
		ID:       candidate.ID,
		Policy:   policy,
		Plan:     plan,
		Backup:   policy.Backup,
		Tier:     tier,
		Priority: policy.Priority,
	}, true, ""
}

func profileTier(profile RouteProfile, plan PlanKind, backup bool, customOrder []PlanKind) (int, bool) {
	if profile == ProfileFreeOnly && backup {
		return 1, true
	}
	switch profile {
	case ProfilePaidFirst:
		switch plan {
		case PlanPaid:
			return 3, true
		case PlanFree:
			return 2, true
		default:
			return 1, true
		}
	case ProfileFreeFirst:
		switch plan {
		case PlanFree:
			return 3, true
		case PlanPaid:
			return 2, true
		default:
			return 1, true
		}
	case ProfileFreeOnly:
		if plan == PlanFree {
			return 1, true
		}
		return 0, false
	case ProfileCustom:
		for index, item := range customOrder {
			if item == plan {
				return len(customOrder) - index, true
			}
		}
		return 0, false
	default:
		return 0, false
	}
}

func strictGroup(candidates []selectionCandidate) []selectionCandidate {
	hasRegular := false
	for _, candidate := range candidates {
		if !candidate.Backup {
			hasRegular = true
			break
		}
	}
	layer := candidates[:0]
	for _, candidate := range candidates {
		if candidate.Backup == !hasRegular {
			layer = append(layer, candidate)
		}
	}
	maxTier := layer[0].Tier
	for _, candidate := range layer[1:] {
		if candidate.Tier > maxTier {
			maxTier = candidate.Tier
		}
	}
	tier := layer[:0]
	for _, candidate := range layer {
		if candidate.Tier == maxTier {
			tier = append(tier, candidate)
		}
	}
	maxPriority := tier[0].Priority
	for _, candidate := range tier[1:] {
		if candidate.Priority > maxPriority {
			maxPriority = candidate.Priority
		}
	}
	group := tier[:0]
	for _, candidate := range tier {
		if candidate.Priority == maxPriority {
			group = append(group, candidate)
		}
	}
	sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
	return group
}

func (s *selector) weightedPick(revision uint64, profile RouteProfile, group []selectionCandidate) string {
	keyBuilder := strings.Builder{}
	keyBuilder.WriteString(strconv.FormatUint(revision, 10))
	keyBuilder.WriteByte('|')
	keyBuilder.WriteString(string(profile))
	for _, candidate := range group {
		keyBuilder.WriteByte('|')
		keyBuilder.WriteString(candidate.ID)
		keyBuilder.WriteByte(':')
		keyBuilder.WriteString(strconv.Itoa(candidate.Policy.Weight))
	}
	key := keyBuilder.String()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.weightRevision != revision {
		s.weights = make(map[string]map[string]int)
		s.weightRevision = revision
	}
	current := s.weights[key]
	if current == nil {
		current = make(map[string]int, len(group))
		s.weights[key] = current
	}
	total := 0
	selected := group[0]
	selectedCurrent := 0
	for index, candidate := range group {
		total += candidate.Policy.Weight
		current[candidate.ID] += candidate.Policy.Weight
		if index == 0 || current[candidate.ID] > selectedCurrent {
			selected = candidate
			selectedCurrent = current[candidate.ID]
		}
	}
	current[selected.ID] -= total
	return selected.ID
}

func (s *selector) affinityHit(sessionKey string, revision uint64, group []selectionCandidate, now time.Time, ttl time.Duration) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.affinity[sessionKey]
	if !ok {
		return ""
	}
	if entry.Revision != revision || !now.Before(entry.ExpiresAt) {
		delete(s.affinity, sessionKey)
		return ""
	}
	for _, candidate := range group {
		if candidate.ID == entry.AuthID {
			entry.ExpiresAt = now.Add(ttl)
			s.affinity[sessionKey] = entry
			return entry.AuthID
		}
	}
	delete(s.affinity, sessionKey)
	return ""
}

func (s *selector) bindAffinity(sessionKey, authID string, revision uint64, expiresAt time.Time) {
	s.mu.Lock()
	now := s.now()
	for key, entry := range s.affinity {
		if entry.Revision != revision || !now.Before(entry.ExpiresAt) {
			delete(s.affinity, key)
		}
	}
	if _, exists := s.affinity[sessionKey]; !exists && len(s.affinity) >= maxAffinityEntries {
		oldestKey := ""
		var oldestExpiry time.Time
		for key, entry := range s.affinity {
			if oldestKey == "" || entry.ExpiresAt.Before(oldestExpiry) {
				oldestKey = key
				oldestExpiry = entry.ExpiresAt
			}
		}
		delete(s.affinity, oldestKey)
	}
	s.affinity[sessionKey] = affinityEntry{AuthID: authID, Revision: revision, ExpiresAt: expiresAt}
	s.mu.Unlock()
}

func schedulerSessionKey(headers map[string][]string, metadata map[string]any) string {
	httpHeaders := http.Header(headers)
	for _, name := range []string{"X-Session-ID", "Session-Id", "Session_id", "X-Client-Request-Id"} {
		if value := strings.TrimSpace(httpHeaders.Get(name)); value != "" {
			return strings.ToLower(name) + ":" + value
		}
	}
	for _, key := range []string{"session_id", "sessionId", "user_id", "conversation_id", "execution_session_id", "derived_session_id"} {
		if value, ok := metadata[key].(string); ok && strings.TrimSpace(value) != "" {
			return key + ":" + strings.TrimSpace(value)
		}
	}
	return ""
}
