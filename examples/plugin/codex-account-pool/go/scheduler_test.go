package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNormalizePolicyDocumentAcceptsAutomaticProfiles(t *testing.T) {
	profiles := []RouteProfile{
		ProfileAuto,
		ProfileQuotaHighFirst,
		ProfileQuotaLowFirst,
		ProfilePlanHighFirst,
		ProfilePlanLowFirst,
	}
	for _, profile := range profiles {
		doc := defaultPolicyDocument()
		doc.ActiveProfile = profile
		if _, err := normalizePolicyDocument(doc); err != nil {
			t.Fatalf("normalizePolicyDocument(%q) error = %v", profile, err)
		}
	}
}

func TestPlanRank(t *testing.T) {
	tests := []struct {
		raw  string
		rank int
		ok   bool
	}{
		{raw: "free", rank: 100, ok: true},
		{raw: "Go", rank: 200, ok: true},
		{raw: "plus", rank: 300, ok: true},
		{raw: "team", rank: 300, ok: true},
		{raw: "business", rank: 300, ok: true},
		{raw: "pro", rank: 400, ok: true},
		{raw: "pro_lite", rank: 400, ok: true},
		{raw: "pro-max", rank: 500, ok: true},
		{raw: "enterprise", rank: 600, ok: true},
		{raw: "edu-enterprise", rank: 600, ok: true},
		{raw: "mystery", rank: 0, ok: false},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			rank, ok := planRank(test.raw)
			if rank != test.rank || ok != test.ok {
				t.Fatalf("planRank(%q) = %d, %v; want %d, %v", test.raw, rank, ok, test.rank, test.ok)
			}
		})
	}
}

func TestQuotaScoreUsesMinimumPresentWindow(t *testing.T) {
	tests := []struct {
		name     string
		snapshot QuotaSnapshot
		score    int
		ok       bool
	}{
		{
			name: "both",
			snapshot: QuotaSnapshot{
				FiveHourRemaining:     70,
				WeeklyRemaining:       40,
				FiveHourWindowPresent: true,
				WeeklyWindowPresent:   true,
			},
			score: 40,
			ok:    true,
		},
		{
			name: "five-hour-only",
			snapshot: QuotaSnapshot{
				FiveHourRemaining:     65,
				FiveHourWindowPresent: true,
			},
			score: 65,
			ok:    true,
		},
		{
			name: "weekly-only",
			snapshot: QuotaSnapshot{
				WeeklyRemaining:     55,
				WeeklyWindowPresent: true,
			},
			score: 55,
			ok:    true,
		},
		{
			name:     "missing",
			snapshot: QuotaSnapshot{},
			score:    0,
			ok:       false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			score, ok := quotaScore(test.snapshot)
			if score != test.score || ok != test.ok {
				t.Fatalf("quotaScore() = %d, %v; want %d, %v", score, ok, test.score, test.ok)
			}
		})
	}
}

func TestSchedulerLeavesNonCodexRequestUnhandled(t *testing.T) {
	selector := newSelector(time.Now)
	resp, err := selector.pick(pluginapi.SchedulerPickRequest{
		Provider: "gemini",
		Candidates: []pluginapi.SchedulerAuthCandidate{{
			ID: "gemini-a", Provider: "gemini", Priority: 10, Status: "active",
		}},
	}, defaultPolicyDocument(), QuotaDocument{}, defaultConfig())
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.Handled {
		t.Fatalf("pick() = %#v, want unhandled", resp)
	}
}

func TestSchedulerKeepsBackupAfterRegular(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := defaultPolicyDocument()
	policy.Accounts = map[string]AccountPolicy{
		"regular": {Enabled: true, Priority: 10, Weight: 1},
		"backup":  {Enabled: true, Priority: 999, Weight: 1, Backup: true},
	}
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "regular", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
		QuotaSnapshot{AuthID: "backup", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
	)
	resp, err := newSelector(func() time.Time { return now }).pick(codexRequest("regular", "backup"), policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "regular" {
		t.Fatalf("pick() auth = %q, want regular", resp.AuthID)
	}
}

func TestSchedulerFreeFirstOverridesBasePriority(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := defaultPolicyDocument()
	policy.ActiveProfile = ProfileFreeFirst
	policy.Accounts = map[string]AccountPolicy{
		"free": {Enabled: true, Priority: 10, Weight: 1},
		"paid": {Enabled: true, Priority: 999, Weight: 1},
	}
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "free", Plan: PlanFree, FiveHourRemaining: 80, WeeklyRemaining: 80},
		QuotaSnapshot{AuthID: "paid", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
	)
	resp, err := newSelector(func() time.Time { return now }).pick(codexRequest("free", "paid"), policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "free" {
		t.Fatalf("pick() auth = %q, want free", resp.AuthID)
	}
}

func TestSchedulerAutoUsesPlanThenQuota(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfileAuto,
		map[string]AccountPolicy{
			"free-high-quota": {Enabled: true, Priority: 999, Weight: 9},
			"pro-low-quota":   {Enabled: true, Priority: 1, Weight: 1},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "free-high-quota", Plan: PlanFree, PlanType: "free", FiveHourRemaining: 95, WeeklyRemaining: 95},
		QuotaSnapshot{AuthID: "pro-low-quota", Plan: PlanPaid, PlanType: "pro", FiveHourRemaining: 30, WeeklyRemaining: 30},
	)

	resp, err := newSelector(func() time.Time { return now }).pick(
		codexRequest("free-high-quota", "pro-low-quota"),
		policy,
		quota,
		defaultConfig(),
	)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "pro-low-quota" {
		t.Fatalf("pick() auth = %q, want pro-low-quota", resp.AuthID)
	}
}

func TestSchedulerQuotaHighFirstUsesQuotaThenPlan(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfileQuotaHighFirst,
		map[string]AccountPolicy{
			"free-high": {Enabled: true, Priority: 1, Weight: 1},
			"pro-low":   {Enabled: true, Priority: 999, Weight: 9},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "free-high", Plan: PlanFree, PlanType: "free", FiveHourRemaining: 90, WeeklyRemaining: 90},
		QuotaSnapshot{AuthID: "pro-low", Plan: PlanPaid, PlanType: "pro", FiveHourRemaining: 70, WeeklyRemaining: 70},
	)

	resp, err := newSelector(func() time.Time { return now }).pick(
		codexRequest("free-high", "pro-low"),
		policy,
		quota,
		defaultConfig(),
	)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "free-high" {
		t.Fatalf("pick() auth = %q, want free-high", resp.AuthID)
	}
}

func TestSchedulerQuotaLowFirstUsesLowestEligibleQuota(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfileQuotaLowFirst,
		map[string]AccountPolicy{
			"free-low": {Enabled: true, Priority: 1, Weight: 1},
			"pro-high": {Enabled: true, Priority: 999, Weight: 9},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "free-low", Plan: PlanFree, PlanType: "free", FiveHourRemaining: 20, WeeklyRemaining: 20},
		QuotaSnapshot{AuthID: "pro-high", Plan: PlanPaid, PlanType: "pro", FiveHourRemaining: 80, WeeklyRemaining: 80},
	)

	resp, err := newSelector(func() time.Time { return now }).pick(
		codexRequest("free-low", "pro-high"),
		policy,
		quota,
		defaultConfig(),
	)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "free-low" {
		t.Fatalf("pick() auth = %q, want free-low", resp.AuthID)
	}
}

func TestSchedulerPlanHighFirstUsesHighestPlanThenQuota(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfilePlanHighFirst,
		map[string]AccountPolicy{
			"enterprise-low": {Enabled: true, Priority: 1, Weight: 1},
			"pro-high":       {Enabled: true, Priority: 999, Weight: 9},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "enterprise-low", Plan: PlanPaid, PlanType: "enterprise", FiveHourRemaining: 20, WeeklyRemaining: 20},
		QuotaSnapshot{AuthID: "pro-high", Plan: PlanPaid, PlanType: "pro", FiveHourRemaining: 90, WeeklyRemaining: 90},
	)

	resp, err := newSelector(func() time.Time { return now }).pick(
		codexRequest("enterprise-low", "pro-high"),
		policy,
		quota,
		defaultConfig(),
	)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "enterprise-low" {
		t.Fatalf("pick() auth = %q, want enterprise-low", resp.AuthID)
	}
}

func TestSchedulerPlanLowFirstUsesLowestPlanThenQuota(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfilePlanLowFirst,
		map[string]AccountPolicy{
			"free-low":  {Enabled: true, Priority: 1, Weight: 1},
			"plus-high": {Enabled: true, Priority: 999, Weight: 9},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "free-low", Plan: PlanFree, PlanType: "free", FiveHourRemaining: 20, WeeklyRemaining: 20},
		QuotaSnapshot{AuthID: "plus-high", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 90, WeeklyRemaining: 90},
	)

	resp, err := newSelector(func() time.Time { return now }).pick(
		codexRequest("free-low", "plus-high"),
		policy,
		quota,
		defaultConfig(),
	)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "free-low" {
		t.Fatalf("pick() auth = %q, want free-low", resp.AuthID)
	}
}

func TestSchedulerAutomaticUnknownMetricsSortLast(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	t.Run("unknown quota", func(t *testing.T) {
		policy := automaticPolicy(ProfileQuotaHighFirst,
			map[string]AccountPolicy{
				"known-quota":   {Enabled: true, Priority: 1, Weight: 1},
				"unknown-quota": {Enabled: true, Priority: 999, Weight: 9},
			},
		)
		quota := freshQuota(now,
			QuotaSnapshot{AuthID: "known-quota", Plan: PlanFree, PlanType: "free", FiveHourRemaining: 10, WeeklyRemaining: 10},
		)
		quota.Accounts["unknown-quota"] = QuotaSnapshot{
			AuthID:      "unknown-quota",
			Plan:        PlanPaid,
			PlanType:    "enterprise",
			RefreshedAt: now,
		}
		cfg := defaultConfig()
		cfg.StalePolicy = StaleAllow

		resp, err := newSelector(func() time.Time { return now }).pick(
			codexRequest("known-quota", "unknown-quota"),
			policy,
			quota,
			cfg,
		)
		if err != nil {
			t.Fatalf("pick() error = %v", err)
		}
		if resp.AuthID != "known-quota" {
			t.Fatalf("pick() auth = %q, want known-quota", resp.AuthID)
		}
	})

	t.Run("unknown plan", func(t *testing.T) {
		policy := automaticPolicy(ProfilePlanHighFirst,
			map[string]AccountPolicy{
				"known-plan":   {Enabled: true, Priority: 1, Weight: 1},
				"unknown-plan": {Enabled: true, Priority: 999, Weight: 9},
			},
		)
		quota := freshQuota(now,
			QuotaSnapshot{AuthID: "known-plan", Plan: PlanFree, PlanType: "free", FiveHourRemaining: 10, WeeklyRemaining: 10},
			QuotaSnapshot{AuthID: "unknown-plan", Plan: PlanPaid, PlanType: "mystery", FiveHourRemaining: 99, WeeklyRemaining: 99},
		)

		resp, err := newSelector(func() time.Time { return now }).pick(
			codexRequest("known-plan", "unknown-plan"),
			policy,
			quota,
			defaultConfig(),
		)
		if err != nil {
			t.Fatalf("pick() error = %v", err)
		}
		if resp.AuthID != "known-plan" {
			t.Fatalf("pick() auth = %q, want known-plan", resp.AuthID)
		}
	})
}

func TestSchedulerAutomaticReserveFilteringPrecedesOrdering(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfileQuotaLowFirst,
		map[string]AccountPolicy{
			"below-reserve": {
				Enabled: true, Priority: 999, Weight: 9,
				FiveHourReserve: 10, WeeklyReserve: 10,
			},
			"eligible": {
				Enabled: true, Priority: 1, Weight: 1,
				FiveHourReserve: 10, WeeklyReserve: 10,
			},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "below-reserve", Plan: PlanPaid, PlanType: "pro", FiveHourRemaining: 5, WeeklyRemaining: 50},
		QuotaSnapshot{AuthID: "eligible", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 30, WeeklyRemaining: 30},
	)

	resp, err := newSelector(func() time.Time { return now }).pick(
		codexRequest("below-reserve", "eligible"),
		policy,
		quota,
		defaultConfig(),
	)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "eligible" {
		t.Fatalf("pick() auth = %q, want eligible", resp.AuthID)
	}
}

func TestSchedulerAutomaticKeepsBackupIndependent(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfileQuotaLowFirst,
		map[string]AccountPolicy{
			"regular": {Enabled: true, Priority: 1, Weight: 1},
			"backup":  {Enabled: true, Priority: 999, Weight: 9, Backup: true},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "regular", Plan: PlanPaid, PlanType: "pro", FiveHourRemaining: 90, WeeklyRemaining: 90},
		QuotaSnapshot{AuthID: "backup", Plan: PlanFree, PlanType: "free", FiveHourRemaining: 10, WeeklyRemaining: 10},
	)

	resp, err := newSelector(func() time.Time { return now }).pick(
		codexRequest("regular", "backup"),
		policy,
		quota,
		defaultConfig(),
	)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "regular" {
		t.Fatalf("pick() auth = %q, want regular", resp.AuthID)
	}
}

func TestSchedulerUsesHighestPriorityWithinTier(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := defaultPolicyDocument()
	policy.Accounts = map[string]AccountPolicy{
		"high": {Enabled: true, Priority: 100, Weight: 1},
		"low":  {Enabled: true, Priority: 80, Weight: 100},
	}
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "high", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
		QuotaSnapshot{AuthID: "low", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
	)
	resp, err := newSelector(func() time.Time { return now }).pick(codexRequest("low", "high"), policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "high" {
		t.Fatalf("pick() auth = %q, want high", resp.AuthID)
	}
}

func TestSchedulerSmoothWeightedRoundRobin(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := defaultPolicyDocument()
	policy.Accounts = map[string]AccountPolicy{
		"heavy": {Enabled: true, Priority: 100, Weight: 3},
		"light": {Enabled: true, Priority: 100, Weight: 1},
	}
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "heavy", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
		QuotaSnapshot{AuthID: "light", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
	)
	selector := newSelector(func() time.Time { return now })
	counts := map[string]int{}
	for range 40 {
		resp, err := selector.pick(codexRequest("heavy", "light"), policy, quota, defaultConfig())
		if err != nil {
			t.Fatalf("pick() error = %v", err)
		}
		counts[resp.AuthID]++
	}
	if counts["heavy"] != 30 || counts["light"] != 10 {
		t.Fatalf("weighted counts = %#v, want 30/10", counts)
	}
}

func TestSchedulerAutomaticExactTieRotatesEquallyAndIgnoresWeight(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfileQuotaHighFirst,
		map[string]AccountPolicy{
			"heavy": {Enabled: true, Priority: 999, Weight: 9},
			"light": {Enabled: true, Priority: 1, Weight: 1},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "heavy", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 80, WeeklyRemaining: 70},
		QuotaSnapshot{AuthID: "light", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 70, WeeklyRemaining: 80},
	)
	selector := newSelector(func() time.Time { return now })
	counts := map[string]int{}
	for range 20 {
		resp, err := selector.pick(codexRequest("heavy", "light"), policy, quota, defaultConfig())
		if err != nil {
			t.Fatalf("pick() error = %v", err)
		}
		counts[resp.AuthID]++
	}
	if counts["heavy"] != 10 || counts["light"] != 10 {
		t.Fatalf("automatic tie counts = %#v, want 10/10", counts)
	}
}

func TestSchedulerAutomaticScoreChangeInvalidatesAffinity(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := automaticPolicy(ProfileQuotaLowFirst,
		map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 100, Weight: 1},
			"b": {Enabled: true, Priority: 100, Weight: 1},
		},
	)
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "a", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 20, WeeklyRemaining: 20},
		QuotaSnapshot{AuthID: "b", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 40, WeeklyRemaining: 40},
	)
	selector := newSelector(func() time.Time { return now })
	req := codexRequest("a", "b")
	headers := http.Header{}
	headers.Set("X-Session-ID", "automatic-session")
	req.Options.Headers = headers

	first, err := selector.pick(req, policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("first pick() error = %v", err)
	}
	if first.AuthID != "a" {
		t.Fatalf("first auth = %q, want a", first.AuthID)
	}

	quota = freshQuota(now,
		QuotaSnapshot{AuthID: "a", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 60, WeeklyRemaining: 60},
		QuotaSnapshot{AuthID: "b", Plan: PlanPaid, PlanType: "plus", FiveHourRemaining: 10, WeeklyRemaining: 10},
	)
	second, err := selector.pick(req, policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("second pick() error = %v", err)
	}
	if second.AuthID != "b" {
		t.Fatalf("second auth = %q, want b", second.AuthID)
	}
}

func TestSchedulerDropsWeightedStateAfterPolicyRevision(t *testing.T) {
	selector := newSelector(time.Now)
	group := []selectionCandidate{
		{ID: "a", Policy: AccountPolicy{Weight: 1}},
		{ID: "b", Policy: AccountPolicy{Weight: 1}},
	}

	selector.weightedPick(1, ProfilePaidFirst, group)
	selector.weightedPick(2, ProfilePaidFirst, group)

	if len(selector.weights) != 1 {
		t.Fatalf("weighted groups = %d, want 1 active revision", len(selector.weights))
	}
}

func TestSelectorPrunesAndBoundsAffinity(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	selector := newSelector(func() time.Time { return now })
	selector.affinity["expired"] = affinityEntry{AuthID: "a", Revision: 1, ExpiresAt: now.Add(-time.Minute)}
	for index := range 10000 {
		key := fmt.Sprintf("session-%05d", index)
		selector.affinity[key] = affinityEntry{
			AuthID:    "a",
			Revision:  2,
			ExpiresAt: now.Add(time.Duration(index+1) * time.Second),
		}
	}

	selector.bindAffinity("latest", "c", 2, now.Add(3*time.Hour))

	if _, ok := selector.affinity["expired"]; ok {
		t.Fatal("expired affinity entry was not removed")
	}
	if _, ok := selector.affinity["session-00000"]; ok {
		t.Fatal("oldest affinity entry was not evicted")
	}
	if len(selector.affinity) != 10000 {
		t.Fatalf("affinity entries = %d, want 10000", len(selector.affinity))
	}
}

func TestSchedulerAffinityCannotEscapeStrictTier(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := defaultPolicyDocument()
	policy.Revision = 1
	policy.ActiveProfile = ProfilePaidFirst
	policy.Accounts = map[string]AccountPolicy{
		"free": {Enabled: true, Priority: 100, Weight: 1},
		"paid": {Enabled: true, Priority: 100, Weight: 1},
	}
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "free", Plan: PlanFree, FiveHourRemaining: 80, WeeklyRemaining: 80},
		QuotaSnapshot{AuthID: "paid", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
	)
	selector := newSelector(func() time.Time { return now })
	req := codexRequest("free", "paid")
	headers := http.Header{}
	headers.Set("X-Session-ID", "session-a")
	req.Options.Headers = headers

	first, err := selector.pick(req, policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("first pick() error = %v", err)
	}
	if first.AuthID != "paid" {
		t.Fatalf("first auth = %q, want paid", first.AuthID)
	}

	policy.Revision = 2
	policy.ActiveProfile = ProfileFreeFirst
	second, err := selector.pick(req, policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("second pick() error = %v", err)
	}
	if second.AuthID != "free" {
		t.Fatalf("second auth = %q, want free", second.AuthID)
	}
}

func TestSchedulerSessionKeyUsesDerivedSessionID(t *testing.T) {
	got := schedulerSessionKey(nil, map[string]any{
		"derived_session_id": "ctx:v1:conversation-root",
	})
	if got != "derived_session_id:ctx:v1:conversation-root" {
		t.Fatalf("schedulerSessionKey() = %q", got)
	}
}

func TestSchedulerAffinityExtendsTTLOnHit(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	current := now
	policy := defaultPolicyDocument()
	policy.Accounts = map[string]AccountPolicy{
		"a": {Enabled: true, Priority: 100, Weight: 1},
		"b": {Enabled: true, Priority: 100, Weight: 1},
	}
	quota := freshQuota(now,
		QuotaSnapshot{AuthID: "a", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
		QuotaSnapshot{AuthID: "b", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80},
	)
	cfg := defaultConfig()
	cfg.AffinityTTL = 30 * time.Minute
	cfg.SnapshotMaxAge = time.Hour
	selector := newSelector(func() time.Time { return current })
	req := codexRequest("a", "b")
	headers := http.Header{}
	headers.Set("X-Session-ID", "session-a")
	req.Options.Headers = headers

	first, err := selector.pick(req, policy, quota, cfg)
	if err != nil {
		t.Fatalf("first pick() error = %v", err)
	}
	current = now.Add(20 * time.Minute)
	second, err := selector.pick(req, policy, quota, cfg)
	if err != nil {
		t.Fatalf("second pick() error = %v", err)
	}
	current = now.Add(35 * time.Minute)
	third, err := selector.pick(req, policy, quota, cfg)
	if err != nil {
		t.Fatalf("third pick() error = %v", err)
	}
	if first.AuthID != "a" || second.AuthID != "a" || third.AuthID != "a" {
		t.Fatalf("affinity picks = %q/%q/%q, want a/a/a", first.AuthID, second.AuthID, third.AuthID)
	}
}

func TestSchedulerExcludesStaleQuota(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := defaultPolicyDocument()
	policy.Accounts["stale"] = AccountPolicy{Enabled: true, Priority: 100, Weight: 1}
	quota := QuotaDocument{Accounts: map[string]QuotaSnapshot{
		"stale": {
			AuthID:            "stale",
			Plan:              PlanFree,
			FiveHourRemaining: 80,
			WeeklyRemaining:   80,
			RefreshedAt:       now.Add(-time.Hour),
		},
	}}
	_, err := newSelector(func() time.Time { return now }).pick(codexRequest("stale"), policy, quota, defaultConfig())
	if err == nil {
		t.Fatal("pick() error = nil, want no eligible account")
	}
}

func TestSchedulerUsesReserveOnlyForPresentQuotaWindows(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	policy := defaultPolicyDocument()
	policy.Accounts["free"] = AccountPolicy{
		Enabled:         true,
		Priority:        100,
		Weight:          1,
		FiveHourReserve: 90,
		WeeklyReserve:   20,
	}
	quota := QuotaDocument{Accounts: map[string]QuotaSnapshot{
		"free": {
			AuthID:              "free",
			Plan:                PlanFree,
			WeeklyRemaining:     30,
			WeeklyWindowPresent: true,
			RefreshedAt:         now,
		},
	}}
	selector := newSelector(func() time.Time { return now })

	resp, err := selector.pick(codexRequest("free"), policy, quota, defaultConfig())
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if resp.AuthID != "free" {
		t.Fatalf("pick() auth = %q, want free", resp.AuthID)
	}

	accountPolicy := policy.Accounts["free"]
	accountPolicy.WeeklyReserve = 40
	policy.Accounts["free"] = accountPolicy
	if _, err = selector.pick(codexRequest("free"), policy, quota, defaultConfig()); err == nil {
		t.Fatal("pick() error = nil, want weekly reserve exclusion")
	}
}

func codexRequest(ids ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{Provider: "codex"}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{
			ID: id, Provider: "codex", Priority: 10, Status: "active",
		})
	}
	return req
}

func freshQuota(now time.Time, snapshots ...QuotaSnapshot) QuotaDocument {
	doc := QuotaDocument{Accounts: make(map[string]QuotaSnapshot)}
	for _, snapshot := range snapshots {
		snapshot.RefreshedAt = now
		if !snapshot.FiveHourWindowPresent && !snapshot.WeeklyWindowPresent {
			snapshot.FiveHourWindowPresent = true
			snapshot.WeeklyWindowPresent = true
		}
		doc.Accounts[snapshot.AuthID] = snapshot
	}
	return doc
}

func automaticPolicy(profile RouteProfile, accounts map[string]AccountPolicy) PolicyDocument {
	doc := defaultPolicyDocument()
	doc.ActiveProfile = profile
	doc.Accounts = accounts
	return doc
}
