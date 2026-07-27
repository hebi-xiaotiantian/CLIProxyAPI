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
	ID         string
	Policy     AccountPolicy
	Plan       PlanKind
	Backup     bool
	Tier       int
	Priority   int
	PlanRank   int
	PlanKnown  bool
	QuotaScore int
	QuotaKnown bool
}

type analyzedCandidate struct {
	Candidate     pluginapi.SchedulerAuthCandidate
	Policy        AccountPolicy
	Snapshot      QuotaSnapshot
	Selection     selectionCandidate
	Eligible      bool
	Reason        string
	HostAvailable bool
	QuotaEligible bool
	InActiveLayer bool
	InActiveGroup bool
}

type selectionAnalysis struct {
	Timestamp      time.Time
	Profile        RouteProfile
	CandidateCount int
	HostAvailable  int
	QuotaEligible  int
	Candidates     []analyzedCandidate
	Eligible       []selectionCandidate
	Layer          []selectionCandidate
	Group          []selectionCandidate
	Excluded       map[string]int
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
	response, _, err := s.pickWithAnalysis(req, policy, quota, cfg)
	return response, err
}

func (s *selector) pickWithAnalysis(req pluginapi.SchedulerPickRequest, policy PolicyDocument, quota QuotaDocument, cfg Config) (pluginapi.SchedulerPickResponse, selectionAnalysis, error) {
	now := s.now()
	analysis := analyzeSelection(req, policy, quota, cfg, now)
	if analysis.CandidateCount == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, analysis, nil
	}
	if len(analysis.Eligible) == 0 {
		return pluginapi.SchedulerPickResponse{}, analysis, fmt.Errorf("codex account pool has no eligible account")
	}
	group := analysis.Group
	if len(group) == 0 {
		return pluginapi.SchedulerPickResponse{}, analysis, fmt.Errorf("codex account pool has no selectable account")
	}
	sessionKey := schedulerSessionKey(req.Options.Headers, req.Options.Metadata)
	if sessionKey != "" {
		if authID := s.affinityHit(sessionKey, policy.Revision, group, now, cfg.AffinityTTL); authID != "" {
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: authID}, analysis, nil
		}
	}
	authID := ""
	if isAutomaticProfile(analysis.Profile) {
		authID = s.equalPick(policy.Revision, analysis.Profile, group)
	} else {
		authID = s.weightedPick(policy.Revision, analysis.Profile, group)
	}
	if sessionKey != "" {
		s.bindAffinity(sessionKey, authID, policy.Revision, now.Add(cfg.AffinityTTL))
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: authID}, analysis, nil
}

func analyzeSelection(req pluginapi.SchedulerPickRequest, policy PolicyDocument, quota QuotaDocument, cfg Config, now time.Time) selectionAnalysis {
	analysis := selectionAnalysis{
		Timestamp: now.UTC(),
		Profile:   effectiveProfile(policy, now),
		Excluded:  make(map[string]int),
	}
	for _, candidate := range req.Candidates {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "codex") {
			continue
		}
		analysis.CandidateCount++
		accountPolicy, ok := policy.Accounts[candidate.ID]
		if !ok {
			accountPolicy = defaultAccountPolicy(candidate.Priority)
		}
		analyzed := classifyCandidateDetailed(
			candidate,
			accountPolicy,
			quota.Accounts[candidate.ID],
			analysis.Profile,
			policy.CustomPlanOrder,
			cfg,
			now,
		)
		if analyzed.HostAvailable {
			analysis.HostAvailable++
		}
		if analyzed.QuotaEligible {
			analysis.QuotaEligible++
		}
		if analyzed.Eligible {
			analysis.Eligible = append(analysis.Eligible, analyzed.Selection)
		} else if analyzed.Reason != "" {
			analysis.Excluded[analyzed.Reason]++
		}
		analysis.Candidates = append(analysis.Candidates, analyzed)
	}
	if len(analysis.Eligible) == 0 {
		return analysis
	}
	analysis.Layer = activeLayer(analysis.Eligible)
	analysis.Group = selectionGroup(analysis.Profile, analysis.Eligible)
	layerIDs := selectionIDSet(analysis.Layer)
	groupIDs := selectionIDSet(analysis.Group)
	for index := range analysis.Candidates {
		analysis.Candidates[index].InActiveLayer = layerIDs[analysis.Candidates[index].Candidate.ID]
		analysis.Candidates[index].InActiveGroup = groupIDs[analysis.Candidates[index].Candidate.ID]
	}
	return analysis
}

func selectionIDSet(candidates []selectionCandidate) map[string]bool {
	ids := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		ids[candidate.ID] = true
	}
	return ids
}

func classifyCandidate(candidate pluginapi.SchedulerAuthCandidate, policy AccountPolicy, snapshot QuotaSnapshot, profile RouteProfile, customOrder []PlanKind, cfg Config, now time.Time) (selectionCandidate, bool) {
	item, ok, _ := classifyCandidateWithReason(candidate, policy, snapshot, profile, customOrder, cfg, now)
	return item, ok
}

func classifyCandidateWithReason(candidate pluginapi.SchedulerAuthCandidate, policy AccountPolicy, snapshot QuotaSnapshot, profile RouteProfile, customOrder []PlanKind, cfg Config, now time.Time) (selectionCandidate, bool, string) {
	analyzed := classifyCandidateDetailed(candidate, policy, snapshot, profile, customOrder, cfg, now)
	return analyzed.Selection, analyzed.Eligible, analyzed.Reason
}

func classifyCandidateDetailed(candidate pluginapi.SchedulerAuthCandidate, policy AccountPolicy, snapshot QuotaSnapshot, profile RouteProfile, customOrder []PlanKind, cfg Config, now time.Time) analyzedCandidate {
	plan := policy.PlanOverride
	if plan == "" {
		plan = snapshot.Plan
	}
	if !validPlan(plan) {
		plan = PlanUnknown
	}
	rank, rankKnown := planRank(snapshot.PlanType)
	if !rankKnown && plan == PlanFree {
		rank, rankKnown = planRank(string(PlanFree))
	}
	score, scoreKnown := quotaScore(snapshot)
	analyzed := analyzedCandidate{
		Candidate:     candidate,
		Policy:        policy,
		Snapshot:      snapshot,
		HostAvailable: hostCandidateAvailable(candidate),
		Selection: selectionCandidate{
			ID:         candidate.ID,
			Policy:     policy,
			Plan:       plan,
			Backup:     policy.Backup,
			Priority:   policy.Priority,
			PlanRank:   rank,
			PlanKnown:  rankKnown,
			QuotaScore: score,
			QuotaKnown: scoreKnown,
		},
	}
	if !policy.Enabled {
		analyzed.Reason = "plugin_disabled"
		return analyzed
	}
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	if status == "disabled" || status == "unavailable" || status == "error" {
		analyzed.Reason = "host_" + status
		return analyzed
	}

	hasQuotaWindow := snapshot.FiveHourWindowPresent || snapshot.WeeklyWindowPresent
	fresh := hasQuotaWindow && !snapshot.RefreshedAt.IsZero() && now.Sub(snapshot.RefreshedAt) <= cfg.SnapshotMaxAge
	if !fresh && cfg.StalePolicy == StaleExclude {
		analyzed.Reason = "quota_stale"
		return analyzed
	}
	if fresh {
		if snapshot.FiveHourWindowPresent && snapshot.FiveHourRemaining < policy.FiveHourReserve {
			analyzed.Reason = "five_hour_reserve"
			return analyzed
		}
		if snapshot.WeeklyWindowPresent && snapshot.WeeklyRemaining < policy.WeeklyReserve {
			analyzed.Reason = "weekly_reserve"
			return analyzed
		}
	}
	analyzed.QuotaEligible = true

	tier := 0
	if !isAutomaticProfile(profile) {
		var ok bool
		tier, ok = profileTier(profile, plan, policy.Backup, customOrder)
		if !ok {
			analyzed.Reason = "profile_excluded"
			return analyzed
		}
	}
	analyzed.Selection.Tier = tier
	analyzed.Eligible = true
	return analyzed
}

func hostCandidateAvailable(candidate pluginapi.SchedulerAuthCandidate) bool {
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	return status != "disabled" && status != "unavailable" && status != "error"
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

func planRank(raw string) (int, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	compact := strings.NewReplacer("-", "", "_", "", " ", "").Replace(normalized)
	switch {
	case strings.Contains(compact, "enterprise"),
		strings.Contains(compact, "health"),
		strings.Contains(compact, "gov"),
		strings.Contains(compact, "teacher"),
		strings.Contains(compact, "edu"):
		return 600, true
	case strings.Contains(compact, "promax"):
		return 500, true
	case strings.Contains(compact, "prolite"), strings.Contains(compact, "pro"):
		return 400, true
	case strings.Contains(compact, "business"),
		strings.Contains(compact, "team"),
		strings.Contains(compact, "plus"):
		return 300, true
	case compact == "go" || strings.HasSuffix(compact, "go"):
		return 200, true
	case strings.Contains(compact, "free"):
		return 100, true
	default:
		return 0, false
	}
}

func quotaScore(snapshot QuotaSnapshot) (int, bool) {
	switch {
	case snapshot.FiveHourWindowPresent && snapshot.WeeklyWindowPresent:
		return min(snapshot.FiveHourRemaining, snapshot.WeeklyRemaining), true
	case snapshot.FiveHourWindowPresent:
		return snapshot.FiveHourRemaining, true
	case snapshot.WeeklyWindowPresent:
		return snapshot.WeeklyRemaining, true
	default:
		return 0, false
	}
}

func selectionGroup(profile RouteProfile, candidates []selectionCandidate) []selectionCandidate {
	layer := activeLayer(candidates)
	if isAutomaticProfile(profile) {
		return automaticProfileGroup(profile, layer)
	}
	return strictProfileGroup(layer)
}

func activeLayer(candidates []selectionCandidate) []selectionCandidate {
	hasRegular := false
	for _, candidate := range candidates {
		if !candidate.Backup {
			hasRegular = true
			break
		}
	}
	layer := make([]selectionCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Backup == !hasRegular {
			layer = append(layer, candidate)
		}
	}
	return layer
}

func strictProfileGroup(layer []selectionCandidate) []selectionCandidate {
	maxTier := layer[0].Tier
	for _, candidate := range layer[1:] {
		if candidate.Tier > maxTier {
			maxTier = candidate.Tier
		}
	}
	tier := make([]selectionCandidate, 0, len(layer))
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
	group := make([]selectionCandidate, 0, len(tier))
	for _, candidate := range tier {
		if candidate.Priority == maxPriority {
			group = append(group, candidate)
		}
	}
	sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
	return group
}

func strictGroup(candidates []selectionCandidate) []selectionCandidate {
	return strictProfileGroup(activeLayer(candidates))
}

func automaticProfileGroup(profile RouteProfile, layer []selectionCandidate) []selectionCandidate {
	best := layer[0]
	for _, candidate := range layer[1:] {
		if compareAutomaticCandidate(profile, candidate, best) < 0 {
			best = candidate
		}
	}
	group := make([]selectionCandidate, 0, len(layer))
	for _, candidate := range layer {
		if compareAutomaticCandidate(profile, candidate, best) == 0 {
			group = append(group, candidate)
		}
	}
	sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
	return group
}

func compareAutomaticCandidate(profile RouteProfile, left, right selectionCandidate) int {
	switch profile {
	case ProfileAuto, ProfilePlanHighFirst:
		if compared := compareKnownMetric(left.PlanKnown, left.PlanRank, right.PlanKnown, right.PlanRank, false); compared != 0 {
			return compared
		}
		return compareKnownMetric(left.QuotaKnown, left.QuotaScore, right.QuotaKnown, right.QuotaScore, false)
	case ProfilePlanLowFirst:
		if compared := compareKnownMetric(left.PlanKnown, left.PlanRank, right.PlanKnown, right.PlanRank, true); compared != 0 {
			return compared
		}
		return compareKnownMetric(left.QuotaKnown, left.QuotaScore, right.QuotaKnown, right.QuotaScore, false)
	case ProfileQuotaHighFirst:
		if compared := compareKnownMetric(left.QuotaKnown, left.QuotaScore, right.QuotaKnown, right.QuotaScore, false); compared != 0 {
			return compared
		}
		return compareKnownMetric(left.PlanKnown, left.PlanRank, right.PlanKnown, right.PlanRank, false)
	case ProfileQuotaLowFirst:
		if compared := compareKnownMetric(left.QuotaKnown, left.QuotaScore, right.QuotaKnown, right.QuotaScore, true); compared != 0 {
			return compared
		}
		return compareKnownMetric(left.PlanKnown, left.PlanRank, right.PlanKnown, right.PlanRank, false)
	default:
		return 0
	}
}

func compareKnownMetric(leftKnown bool, left int, rightKnown bool, right int, ascending bool) int {
	if leftKnown != rightKnown {
		if leftKnown {
			return -1
		}
		return 1
	}
	if !leftKnown || left == right {
		return 0
	}
	if ascending {
		if left < right {
			return -1
		}
		return 1
	}
	if left > right {
		return -1
	}
	return 1
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

func (s *selector) equalPick(revision uint64, profile RouteProfile, group []selectionCandidate) string {
	equalGroup := make([]selectionCandidate, len(group))
	copy(equalGroup, group)
	for index := range equalGroup {
		equalGroup[index].Policy.Weight = 1
	}
	return s.weightedPick(revision, profile, equalGroup)
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
