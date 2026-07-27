package main

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
