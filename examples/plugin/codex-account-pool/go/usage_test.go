package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUsageTrackerObservesCodexRecords(t *testing.T) {
	now := time.Date(2026, time.July, 27, 12, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	tracker := newUsageTracker(func() time.Time { return now }, time.Hour)
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})
	if err := tracker.configureStore(newStateStore(t.TempDir())); err != nil {
		t.Fatalf("configure store: %v", err)
	}

	tracker.observe(pluginapi.UsageRecord{
		Provider: "openai",
		AuthID:   "ignored-provider",
		Detail:   pluginapi.UsageDetail{TotalTokens: 99},
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   " ",
		Detail:   pluginapi.UsageDetail{TotalTokens: 99},
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: " CoDeX ",
		AuthID:   " auth-a ",
		Failed:   true,
		Detail: pluginapi.UsageDetail{
			InputTokens:         10,
			OutputTokens:        4,
			ReasoningTokens:     3,
			CachedTokens:        6,
			CacheReadTokens:     4,
			CacheCreationTokens: 2,
			TotalTokens:         100,
		},
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail: pluginapi.UsageDetail{
			InputTokens:         7,
			OutputTokens:        5,
			ReasoningTokens:     2,
			CachedTokens:        3,
			CacheReadTokens:     9,
			CacheCreationTokens: 1,
		},
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-b",
		Failed:   true,
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-c",
		Detail: pluginapi.UsageDetail{
			InputTokens:         -1,
			OutputTokens:        -2,
			ReasoningTokens:     -3,
			CachedTokens:        -4,
			CacheReadTokens:     -5,
			CacheCreationTokens: -6,
			TotalTokens:         -7,
		},
	})

	got := tracker.snapshot()
	if len(got.Accounts) != 3 {
		t.Fatalf("account count = %d, want 3: %#v", len(got.Accounts), got.Accounts)
	}
	wantUpdatedAt := now.UTC()
	assertAccountUsage(t, got.Accounts["auth-a"], AccountUsage{
		Requests:            2,
		FailedRequests:      1,
		InputTokens:         17,
		OutputTokens:        9,
		ReasoningTokens:     5,
		CacheReadTokens:     15,
		CacheCreationTokens: 3,
		TotalTokens:         112,
		UpdatedAt:           wantUpdatedAt,
	})
	assertAccountUsage(t, got.Accounts["auth-b"], AccountUsage{
		Requests:       1,
		FailedRequests: 1,
		UpdatedAt:      wantUpdatedAt,
	})
	assertAccountUsage(t, got.Accounts["auth-c"], AccountUsage{
		Requests:  1,
		UpdatedAt: wantUpdatedAt,
	})

	got.Accounts["auth-a"] = AccountUsage{}
	if tracker.snapshot().Accounts["auth-a"].Requests != 2 {
		t.Fatal("snapshot returned a shared accounts map")
	}
}

func TestUsageTrackerReportsMissingStore(t *testing.T) {
	tracker := newUsageTracker(time.Now, time.Hour)
	tracker.observe(usageRecord("auth-a", 3, 5))

	err := tracker.shutdown()
	if err == nil {
		t.Fatal("shutdown succeeded with dirty usage and no state store")
	}
	degraded, message := tracker.health()
	if !degraded || message == "" {
		t.Fatalf("health = (%v, %q), want degraded with message", degraded, message)
	}
	if !tracker.isDirty() {
		t.Fatal("missing store cleared dirty usage")
	}
	got := tracker.snapshot().Accounts["auth-a"]
	if got.Requests != 1 || got.InputTokens != 3 || got.OutputTokens != 5 || got.TotalTokens != 8 {
		t.Fatalf("in-memory usage was not retained: %#v", got)
	}
}

func TestUsageTrackerSaturatesAllCounters(t *testing.T) {
	dir := t.TempDir()
	store := newStateStore(dir)
	maxed := AccountUsage{
		Requests:            math.MaxInt64,
		FailedRequests:      math.MaxInt64,
		InputTokens:         math.MaxInt64,
		OutputTokens:        math.MaxInt64,
		ReasoningTokens:     math.MaxInt64,
		CacheReadTokens:     math.MaxInt64,
		CacheCreationTokens: math.MaxInt64,
		TotalTokens:         math.MaxInt64,
	}
	if err := store.saveUsage(UsageDocument{
		Version:  stateVersion,
		Accounts: map[string]AccountUsage{"auth-a": maxed},
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	tracker := newUsageTracker(time.Now, time.Hour)
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure store: %v", err)
	}
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Failed:   true,
		Detail: pluginapi.UsageDetail{
			InputTokens:         1,
			OutputTokens:        1,
			ReasoningTokens:     1,
			CacheReadTokens:     1,
			CacheCreationTokens: 1,
			TotalTokens:         1,
		},
	})

	got := tracker.snapshot().Accounts["auth-a"]
	if got.Requests != math.MaxInt64 ||
		got.FailedRequests != math.MaxInt64 ||
		got.InputTokens != math.MaxInt64 ||
		got.OutputTokens != math.MaxInt64 ||
		got.ReasoningTokens != math.MaxInt64 ||
		got.CacheReadTokens != math.MaxInt64 ||
		got.CacheCreationTokens != math.MaxInt64 ||
		got.TotalTokens != math.MaxInt64 {
		t.Fatalf("counters did not saturate: %#v", got)
	}
}

func TestUsageTrackerMergesBurstAndPersistsForRestart(t *testing.T) {
	dir := t.TempDir()
	store := newStateStore(dir)
	tracker := newUsageTracker(time.Now, 20*time.Millisecond)
	var saves atomic.Int64
	tracker.save = func(store *stateStore, doc UsageDocument) error {
		saves.Add(1)
		return store.saveUsage(doc)
	}
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure store: %v", err)
	}

	tracker.observe(usageRecord("auth-a", 2, 3))
	tracker.observe(usageRecord("auth-a", 5, 7))

	waitForCondition(t, time.Second, func() bool {
		doc, err := store.loadUsage()
		return err == nil && doc.Accounts["auth-a"].Requests == 2
	}, "burst usage persisted")
	waitForCondition(t, time.Second, func() bool {
		return !tracker.isDirty()
	}, "tracker became clean")
	if got := saves.Load(); got != 1 {
		t.Fatalf("save count = %d, want 1", got)
	}
	if err := tracker.shutdown(); err != nil {
		t.Fatalf("shutdown usage tracker: %v", err)
	}

	restarted := newUsageTracker(time.Now, time.Hour)
	t.Cleanup(func() {
		if err := restarted.shutdown(); err != nil {
			t.Fatalf("shutdown restarted tracker: %v", err)
		}
	})
	if err := restarted.configureStore(newStateStore(dir)); err != nil {
		t.Fatalf("configure restarted tracker: %v", err)
	}
	got := restarted.snapshot().Accounts["auth-a"]
	if got.Requests != 2 || got.InputTokens != 7 || got.OutputTokens != 10 || got.TotalTokens != 17 {
		t.Fatalf("restarted usage = %#v", got)
	}
}

func TestUsageTrackerRetriesFailedSaveAndRecoversHealth(t *testing.T) {
	dir := t.TempDir()
	store := newStateStore(dir)
	tracker := newUsageTracker(time.Now, 20*time.Millisecond)
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})

	var saves atomic.Int64
	tracker.save = func(store *stateStore, doc UsageDocument) error {
		if saves.Add(1) == 1 {
			return fmt.Errorf("save failed: access_token=supersecret")
		}
		return store.saveUsage(doc)
	}
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure store: %v", err)
	}
	tracker.observe(usageRecord("auth-a", 4, 6))

	waitForCondition(t, time.Second, func() bool {
		degraded, message := tracker.health()
		return degraded && message != "" && !strings.Contains(message, "supersecret") && strings.Contains(message, "[redacted]")
	}, "sanitized degraded health")
	waitForCondition(t, time.Second, func() bool {
		doc, err := store.loadUsage()
		if err != nil || doc.Accounts["auth-a"].TotalTokens != 10 {
			return false
		}
		degraded, message := tracker.health()
		return !degraded && message == ""
	}, "automatic retry recovered")
	if saves.Load() < 2 {
		t.Fatalf("save attempts = %d, want at least 2", saves.Load())
	}
}

func TestUsageTrackerSwitchesStateDirectory(t *testing.T) {
	oldStore := newStateStore(t.TempDir())
	newStore := newStateStore(t.TempDir())
	if err := newStore.saveUsage(UsageDocument{
		Version: stateVersion,
		Accounts: map[string]AccountUsage{
			"auth-new": {Requests: 7, TotalTokens: 77},
		},
	}); err != nil {
		t.Fatalf("seed new store: %v", err)
	}

	tracker := newUsageTracker(time.Now, time.Hour)
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})
	if err := tracker.configureStore(oldStore); err != nil {
		t.Fatalf("configure old store: %v", err)
	}
	tracker.observe(usageRecord("auth-old", 3, 4))
	if err := tracker.configureStore(newStore); err != nil {
		t.Fatalf("switch store: %v", err)
	}

	oldDoc, err := oldStore.loadUsage()
	if err != nil {
		t.Fatalf("load old usage: %v", err)
	}
	if oldDoc.Accounts["auth-old"].TotalTokens != 7 {
		t.Fatalf("old usage was not flushed: %#v", oldDoc)
	}
	got := tracker.snapshot()
	if len(got.Accounts) != 1 || got.Accounts["auth-new"].Requests != 7 {
		t.Fatalf("new store was not loaded cleanly: %#v", got)
	}
	if _, exists := got.Accounts["auth-old"]; exists {
		t.Fatal("old account leaked into new state directory")
	}
}

func TestUsageTrackerRejectsSwitchWhenOldFlushFails(t *testing.T) {
	oldStore := newStateStore(t.TempDir())
	newStore := newStateStore(t.TempDir())
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(oldStore); err != nil {
		t.Fatalf("configure old store: %v", err)
	}
	tracker.observe(usageRecord("auth-old", 3, 4))
	tracker.save = func(store *stateStore, doc UsageDocument) error {
		if store.dir == oldStore.dir {
			return fmt.Errorf("flush failed: bearer topsecret")
		}
		return store.saveUsage(doc)
	}

	err := tracker.configureStore(newStore)
	if err == nil {
		t.Fatal("switch store succeeded despite old flush failure")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("switch error leaked secret: %v", err)
	}
	if got := tracker.snapshot(); got.Accounts["auth-old"].TotalTokens != 7 {
		t.Fatalf("old in-memory usage was not retained: %#v", got)
	}
	tracker.observe(usageRecord("auth-old", 1, 2))

	tracker.save = func(store *stateStore, doc UsageDocument) error {
		return store.saveUsage(doc)
	}
	if err := tracker.shutdown(); err != nil {
		t.Fatalf("shutdown usage tracker: %v", err)
	}
	oldDoc, errLoad := oldStore.loadUsage()
	if errLoad != nil {
		t.Fatalf("load old usage: %v", errLoad)
	}
	if oldDoc.Accounts["auth-old"].TotalTokens != 10 {
		t.Fatalf("tracker no longer used old store: %#v", oldDoc)
	}
	if _, errStat := os.Stat(filepath.Join(newStore.dir, usageFileName)); !os.IsNotExist(errStat) {
		t.Fatalf("new store unexpectedly written, stat error: %v", errStat)
	}
}

func TestUsageTrackerCorruptUsageStartsDegradedWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, usageFileName)
	corrupt := []byte("{not-json")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt usage: %v", err)
	}

	tracker := newUsageTracker(time.Now, 20*time.Millisecond)
	if err := tracker.configureStore(newStateStore(dir)); err != nil {
		t.Fatalf("configure corrupt store: %v", err)
	}
	if got := tracker.snapshot(); len(got.Accounts) != 0 {
		t.Fatalf("snapshot accounts = %#v, want empty", got.Accounts)
	}
	degraded, message := tracker.health()
	if !degraded || message == "" {
		t.Fatalf("health = (%v, %q), want degraded with message", degraded, message)
	}
	if err := tracker.shutdown(); err != nil {
		t.Fatalf("shutdown usage tracker: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corrupt usage: %v", err)
	}
	if string(raw) != string(corrupt) {
		t.Fatalf("corrupt usage was overwritten: %q", raw)
	}
}

func TestUsageTrackerShutdownFlushesWithLongInterval(t *testing.T) {
	store := newStateStore(t.TempDir())
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure store: %v", err)
	}
	tracker.observe(usageRecord("auth-a", 8, 13))

	if err := tracker.shutdown(); err != nil {
		t.Fatalf("shutdown usage tracker: %v", err)
	}
	if err := tracker.shutdown(); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	doc, err := store.loadUsage()
	if err != nil {
		t.Fatalf("load usage: %v", err)
	}
	if doc.Accounts["auth-a"].TotalTokens != 21 {
		t.Fatalf("shutdown did not flush usage: %#v", doc)
	}

	tracker.observe(usageRecord("ignored-after-shutdown", 1, 1))
	if _, exists := tracker.snapshot().Accounts["ignored-after-shutdown"]; exists {
		t.Fatal("observe accepted a record after shutdown")
	}
}

func TestUsageTrackerSameStoreDoesNotReloadMemory(t *testing.T) {
	dir := t.TempDir()
	store := newStateStore(dir)
	if err := store.saveUsage(UsageDocument{
		Version: stateVersion,
		Accounts: map[string]AccountUsage{
			"auth-loaded": {Requests: 1, TotalTokens: 2},
		},
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	tracker := newUsageTracker(time.Now, time.Hour)
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure store: %v", err)
	}
	tracker.observe(usageRecord("auth-memory", 3, 5))

	if err := store.saveUsage(defaultUsageDocument()); err != nil {
		t.Fatalf("replace disk usage: %v", err)
	}
	if err := tracker.configureStore(newStateStore(filepath.Join(dir, "."))); err != nil {
		t.Fatalf("reconfigure same store: %v", err)
	}

	got := tracker.snapshot()
	if got.Accounts["auth-loaded"].Requests != 1 || got.Accounts["auth-memory"].TotalTokens != 8 {
		t.Fatalf("same-store configure reloaded memory: %#v", got)
	}
}

func usageRecord(authID string, input, output int64) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   authID,
		Detail: pluginapi.UsageDetail{
			InputTokens:  input,
			OutputTokens: output,
		},
	}
}

func assertAccountUsage(t *testing.T, got, want AccountUsage) {
	t.Helper()
	if got != want {
		t.Fatalf("account usage = %#v, want %#v", got, want)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}
