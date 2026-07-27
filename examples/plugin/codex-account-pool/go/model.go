package main

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const stateVersion = 1

type RouteProfile string

const (
	ProfileAuto           RouteProfile = "auto"
	ProfileQuotaHighFirst RouteProfile = "quota-high-first"
	ProfileQuotaLowFirst  RouteProfile = "quota-low-first"
	ProfilePlanHighFirst  RouteProfile = "plan-high-first"
	ProfilePlanLowFirst   RouteProfile = "plan-low-first"
	ProfilePaidFirst      RouteProfile = "paid-first"
	ProfileFreeFirst      RouteProfile = "free-first"
	ProfileFreeOnly       RouteProfile = "free-only"
	ProfileCustom         RouteProfile = "custom"
)

type PlanKind string

const (
	PlanUnknown PlanKind = "unknown"
	PlanFree    PlanKind = "free"
	PlanPaid    PlanKind = "paid"
)

type StalePolicy string

const (
	StaleExclude StalePolicy = "exclude"
	StaleAllow   StalePolicy = "allow"
)

type Config struct {
	StateDir              string
	RefreshInterval       time.Duration
	RefreshConcurrency    int
	SnapshotMaxAge        time.Duration
	StalePolicy           StalePolicy
	AffinityTTL           time.Duration
	QuotaURL              string
	CustomPlanOrder       []PlanKind
	FreeReserveDefault    int
	PaidReserveDefault    int
	UnknownReserveDefault int
}

type AccountPolicy struct {
	Enabled                bool     `json:"enabled"`
	Priority               int      `json:"priority"`
	Weight                 int      `json:"weight"`
	Backup                 bool     `json:"backup,omitempty"`
	PlanOverride           PlanKind `json:"plan_override,omitempty"`
	FiveHourReserve        int      `json:"five_hour_reserve"`
	WeeklyReserve          int      `json:"weekly_reserve"`
	ReserveDefaultsForPlan PlanKind `json:"reserve_defaults_for_plan,omitempty"`
	Tags                   []string `json:"tags,omitempty"`
}

type ProfileOverride struct {
	Profile   RouteProfile `json:"profile"`
	ExpiresAt time.Time    `json:"expires_at"`
}

type PolicyDocument struct {
	Version           int                      `json:"version"`
	Revision          uint64                   `json:"revision"`
	ActiveProfile     RouteProfile             `json:"active_profile"`
	TemporaryOverride *ProfileOverride         `json:"temporary_override,omitempty"`
	CustomPlanOrder   []PlanKind               `json:"custom_plan_order,omitempty"`
	Accounts          map[string]AccountPolicy `json:"accounts"`
}

type RefreshStatus struct {
	State       string    `json:"state,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	FailedAt    time.Time `json:"failed_at,omitempty"`
	NextRefresh time.Time `json:"next_refresh,omitempty"`
}

type QuotaSnapshot struct {
	AuthID                string        `json:"auth_id"`
	Plan                  PlanKind      `json:"plan"`
	PlanType              string        `json:"plan_type,omitempty"`
	FiveHourRemaining     int           `json:"five_hour_remaining"`
	WeeklyRemaining       int           `json:"weekly_remaining"`
	FiveHourResetAt       time.Time     `json:"five_hour_reset_at,omitempty"`
	WeeklyResetAt         time.Time     `json:"weekly_reset_at,omitempty"`
	FiveHourWindowMinutes int           `json:"five_hour_window_minutes,omitempty"`
	WeeklyWindowMinutes   int           `json:"weekly_window_minutes,omitempty"`
	FiveHourWindowPresent bool          `json:"five_hour_window_present"`
	WeeklyWindowPresent   bool          `json:"weekly_window_present"`
	RefreshedAt           time.Time     `json:"refreshed_at"`
	Refresh               RefreshStatus `json:"refresh,omitempty"`
}

type QuotaDocument struct {
	Version  int                      `json:"version"`
	Accounts map[string]QuotaSnapshot `json:"accounts"`
}

type AccountUsage struct {
	Requests            int64     `json:"requests"`
	FailedRequests      int64     `json:"failed_requests"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	ReasoningTokens     int64     `json:"reasoning_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	TotalTokens         int64     `json:"total_tokens"`
	UpdatedAt           time.Time `json:"updated_at,omitempty"`
}

type UsageDocument struct {
	Version  int                     `json:"version"`
	Accounts map[string]AccountUsage `json:"accounts"`
}

func defaultConfig() Config {
	return Config{
		StateDir:              "codex-account-pool-data",
		RefreshInterval:       10 * time.Minute,
		RefreshConcurrency:    3,
		SnapshotMaxAge:        20 * time.Minute,
		StalePolicy:           StaleExclude,
		AffinityTTL:           30 * time.Minute,
		QuotaURL:              "https://chatgpt.com/backend-api/wham/usage",
		CustomPlanOrder:       []PlanKind{PlanPaid, PlanFree, PlanUnknown},
		FreeReserveDefault:    0,
		PaidReserveDefault:    10,
		UnknownReserveDefault: 10,
	}
}

func defaultPolicyDocument() PolicyDocument {
	return PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts:      make(map[string]AccountPolicy),
	}
}

func defaultQuotaDocument() QuotaDocument {
	return QuotaDocument{
		Version:  stateVersion,
		Accounts: make(map[string]QuotaSnapshot),
	}
}

func defaultUsageDocument() UsageDocument {
	return UsageDocument{
		Version:  stateVersion,
		Accounts: make(map[string]AccountUsage),
	}
}

func nonNegativeCounter(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func saturatingAdd(current, delta int64) int64 {
	current = nonNegativeCounter(current)
	delta = nonNegativeCounter(delta)
	if current > math.MaxInt64-delta {
		return math.MaxInt64
	}
	return current + delta
}

func defaultAccountPolicy(priority int) AccountPolicy {
	return AccountPolicy{
		Enabled:  true,
		Priority: priority,
		Weight:   1,
	}
}

func normalizePolicyDocument(doc PolicyDocument) (PolicyDocument, error) {
	if doc.Version == 0 {
		doc.Version = stateVersion
	}
	if doc.Version != stateVersion {
		return PolicyDocument{}, fmt.Errorf("unsupported policy version %d", doc.Version)
	}
	if doc.Revision == 0 {
		doc.Revision = 1
	}
	if doc.ActiveProfile == "" {
		doc.ActiveProfile = ProfilePaidFirst
	}
	if !validProfile(doc.ActiveProfile) {
		return PolicyDocument{}, fmt.Errorf("invalid active profile %q", doc.ActiveProfile)
	}
	if doc.TemporaryOverride != nil {
		if !validProfile(doc.TemporaryOverride.Profile) {
			return PolicyDocument{}, fmt.Errorf("invalid temporary profile %q", doc.TemporaryOverride.Profile)
		}
		if doc.TemporaryOverride.ExpiresAt.IsZero() {
			return PolicyDocument{}, fmt.Errorf("temporary profile expiration is required")
		}
	}
	var errPlanOrder error
	doc.CustomPlanOrder, errPlanOrder = normalizePlanOrder(doc.CustomPlanOrder)
	if errPlanOrder != nil {
		return PolicyDocument{}, errPlanOrder
	}
	normalized := make(map[string]AccountPolicy, len(doc.Accounts))
	for rawID, policy := range doc.Accounts {
		authID := strings.TrimSpace(rawID)
		if authID == "" {
			return PolicyDocument{}, fmt.Errorf("account id is required")
		}
		if policy.Weight < 1 {
			return PolicyDocument{}, fmt.Errorf("account %s weight must be at least 1", authID)
		}
		if policy.FiveHourReserve < 0 || policy.FiveHourReserve > 100 {
			return PolicyDocument{}, fmt.Errorf("account %s five-hour reserve must be between 0 and 100", authID)
		}
		if policy.WeeklyReserve < 0 || policy.WeeklyReserve > 100 {
			return PolicyDocument{}, fmt.Errorf("account %s weekly reserve must be between 0 and 100", authID)
		}
		if policy.PlanOverride != "" && !validPlan(policy.PlanOverride) {
			return PolicyDocument{}, fmt.Errorf("account %s has invalid plan override %q", authID, policy.PlanOverride)
		}
		if policy.ReserveDefaultsForPlan != "" && !validPlan(policy.ReserveDefaultsForPlan) {
			return PolicyDocument{}, fmt.Errorf("account %s has invalid reserve default plan %q", authID, policy.ReserveDefaultsForPlan)
		}
		policy.Tags = normalizeTags(policy.Tags)
		normalized[authID] = policy
	}
	doc.Accounts = normalized
	return doc, nil
}

func normalizePlanOrder(order []PlanKind) ([]PlanKind, error) {
	if len(order) == 0 {
		return []PlanKind{PlanPaid, PlanFree, PlanUnknown}, nil
	}
	normalized := make([]PlanKind, 0, len(order))
	seen := make(map[PlanKind]struct{}, len(order))
	for _, plan := range order {
		if !validPlan(plan) {
			return nil, fmt.Errorf("invalid custom plan %q", plan)
		}
		if _, exists := seen[plan]; exists {
			return nil, fmt.Errorf("duplicate custom plan %q", plan)
		}
		seen[plan] = struct{}{}
		normalized = append(normalized, plan)
	}
	return normalized, nil
}

func normalizeQuotaDocument(doc QuotaDocument) (QuotaDocument, error) {
	if doc.Version == 0 {
		doc.Version = stateVersion
	}
	if doc.Version != stateVersion {
		return QuotaDocument{}, fmt.Errorf("unsupported quota version %d", doc.Version)
	}
	if doc.Accounts == nil {
		doc.Accounts = make(map[string]QuotaSnapshot)
	}
	for authID, snapshot := range doc.Accounts {
		key := strings.TrimSpace(authID)
		if key == "" {
			return QuotaDocument{}, fmt.Errorf("quota account id is required")
		}
		snapshot.AuthID = key
		snapshot.FiveHourRemaining = clampPercent(snapshot.FiveHourRemaining)
		snapshot.WeeklyRemaining = clampPercent(snapshot.WeeklyRemaining)
		if !validPlan(snapshot.Plan) {
			snapshot.Plan = normalizePlan(snapshot.PlanType)
		}
		if key != authID {
			delete(doc.Accounts, authID)
		}
		doc.Accounts[key] = snapshot
	}
	return doc, nil
}

func normalizeUsageDocument(doc UsageDocument) (UsageDocument, error) {
	if doc.Version == 0 {
		doc.Version = stateVersion
	}
	if doc.Version != stateVersion {
		return UsageDocument{}, fmt.Errorf("unsupported usage version %d", doc.Version)
	}
	if doc.Accounts == nil {
		doc.Accounts = make(map[string]AccountUsage)
	}
	normalized := make(map[string]AccountUsage, len(doc.Accounts))
	for rawID, account := range doc.Accounts {
		authID := strings.TrimSpace(rawID)
		if authID == "" {
			return UsageDocument{}, fmt.Errorf("usage account id is required")
		}
		account.Requests = nonNegativeCounter(account.Requests)
		account.FailedRequests = nonNegativeCounter(account.FailedRequests)
		account.InputTokens = nonNegativeCounter(account.InputTokens)
		account.OutputTokens = nonNegativeCounter(account.OutputTokens)
		account.ReasoningTokens = nonNegativeCounter(account.ReasoningTokens)
		account.CacheReadTokens = nonNegativeCounter(account.CacheReadTokens)
		account.CacheCreationTokens = nonNegativeCounter(account.CacheCreationTokens)
		account.TotalTokens = nonNegativeCounter(account.TotalTokens)
		if !account.UpdatedAt.IsZero() {
			account.UpdatedAt = account.UpdatedAt.UTC()
		}
		normalized[authID] = account
	}
	doc.Accounts = normalized
	return doc, nil
}

func effectiveProfile(doc PolicyDocument, now time.Time) RouteProfile {
	if override := doc.TemporaryOverride; override != nil && now.Before(override.ExpiresAt) {
		return override.Profile
	}
	if validProfile(doc.ActiveProfile) {
		return doc.ActiveProfile
	}
	return ProfilePaidFirst
}

func validProfile(profile RouteProfile) bool {
	switch profile {
	case ProfileAuto,
		ProfileQuotaHighFirst,
		ProfileQuotaLowFirst,
		ProfilePlanHighFirst,
		ProfilePlanLowFirst,
		ProfilePaidFirst,
		ProfileFreeFirst,
		ProfileFreeOnly,
		ProfileCustom:
		return true
	default:
		return false
	}
}

func isAutomaticProfile(profile RouteProfile) bool {
	switch profile {
	case ProfileAuto, ProfileQuotaHighFirst, ProfileQuotaLowFirst, ProfilePlanHighFirst, ProfilePlanLowFirst:
		return true
	default:
		return false
	}
}

func validPlan(plan PlanKind) bool {
	switch plan {
	case PlanUnknown, PlanFree, PlanPaid:
		return true
	default:
		return false
	}
}

func normalizePlan(raw string) PlanKind {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" || value == string(PlanUnknown) {
		return PlanUnknown
	}
	if strings.Contains(value, "free") {
		return PlanFree
	}
	return PlanPaid
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func clampPercent(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
