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

func TestUsageTrackerInitialConfigureMergesDiskAndMemory(t *testing.T) {
	oldTime := time.Date(2026, time.July, 26, 10, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(24 * time.Hour)
	store := newStateStore(t.TempDir())
	diskUsage := AccountUsage{
		Requests:            math.MaxInt64,
		FailedRequests:      math.MaxInt64,
		InputTokens:         math.MaxInt64 - 5,
		OutputTokens:        math.MaxInt64 - 5,
		ReasoningTokens:     math.MaxInt64 - 5,
		CacheReadTokens:     math.MaxInt64 - 5,
		CacheCreationTokens: math.MaxInt64 - 5,
		TotalTokens:         math.MaxInt64 - 5,
		UpdatedAt:           oldTime,
	}
	if err := store.saveUsage(UsageDocument{
		Version:  stateVersion,
		Accounts: map[string]AccountUsage{"auth-a": diskUsage},
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	tracker := newUsageTracker(func() time.Time { return newTime }, time.Hour)
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Failed:   true,
		Detail: pluginapi.UsageDetail{
			InputTokens:         10,
			OutputTokens:        10,
			ReasoningTokens:     10,
			CacheReadTokens:     10,
			CacheCreationTokens: 10,
			TotalTokens:         10,
		},
	})
	tracker.observe(usageRecord("auth-memory", 3, 5))

	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure store: %v", err)
	}
	got := tracker.snapshot()
	assertAccountUsage(t, got.Accounts["auth-a"], AccountUsage{
		Requests:            math.MaxInt64,
		FailedRequests:      math.MaxInt64,
		InputTokens:         math.MaxInt64,
		OutputTokens:        math.MaxInt64,
		ReasoningTokens:     math.MaxInt64,
		CacheReadTokens:     math.MaxInt64,
		CacheCreationTokens: math.MaxInt64,
		TotalTokens:         math.MaxInt64,
		UpdatedAt:           newTime,
	})
	assertAccountUsage(t, got.Accounts["auth-memory"], AccountUsage{
		Requests:     1,
		InputTokens:  3,
		OutputTokens: 5,
		TotalTokens:  8,
		UpdatedAt:    newTime,
	})
	if !tracker.isDirty() {
		t.Fatal("initial configure cleared merged usage")
	}

	if err := tracker.flush(); err != nil {
		t.Fatalf("flush merged usage: %v", err)
	}
	persisted, err := store.loadUsage()
	if err != nil {
		t.Fatalf("load merged usage: %v", err)
	}
	assertAccountUsage(t, persisted.Accounts["auth-a"], got.Accounts["auth-a"])
	assertAccountUsage(t, persisted.Accounts["auth-memory"], got.Accounts["auth-memory"])
}

func TestUsageTrackerInitialConfigureDropsMemoryAfterCorruptLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, usageFileName)
	corrupt := []byte("{not-json")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt usage: %v", err)
	}
	tracker := newUsageTracker(time.Now, time.Hour)
	t.Cleanup(func() {
		_ = tracker.shutdown()
	})
	tracker.observe(usageRecord("auth-memory", 3, 5))

	if err := tracker.configureStore(newStateStore(dir)); err != nil {
		t.Fatalf("configure corrupt store: %v", err)
	}
	if got := tracker.snapshot(); len(got.Accounts) != 0 {
		t.Fatalf("snapshot accounts = %#v, want empty", got.Accounts)
	}
	if tracker.isDirty() {
		t.Fatal("corrupt load retained dirty pre-config usage")
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

func TestUsageTrackerKeepsDirtyWhenObserveRacesWithSave(t *testing.T) {
	store := newStateStore(t.TempDir())
	tracker := newUsageTracker(time.Now, time.Hour)
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure store: %v", err)
	}

	var saves atomic.Int64
	tracker.save = func(store *stateStore, doc UsageDocument) error {
		if saves.Add(1) == 1 {
			close(started)
			<-release
		}
		return store.saveUsage(doc)
	}
	tracker.observe(usageRecord("auth-a", 2, 3))
	flushDone := make(chan error, 1)
	go func() {
		flushDone <- tracker.flush()
	}()

	waitForSignal(t, started, time.Second, "first save started")
	tracker.observe(usageRecord("auth-a", 5, 7))
	close(release)
	if err := waitForResult(t, flushDone, time.Second, "first save completed"); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if !tracker.isDirty() {
		t.Fatal("first flush cleared concurrent usage")
	}
	if err := tracker.flush(); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	persisted, err := store.loadUsage()
	if err != nil {
		t.Fatalf("load usage: %v", err)
	}
	got := persisted.Accounts["auth-a"]
	if got.Requests != 2 || got.InputTokens != 7 || got.OutputTokens != 10 || got.TotalTokens != 17 {
		t.Fatalf("concurrent usage was not persisted: %#v", got)
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

func TestUsageTrackerLoadBlockedDoesNotOverwriteUsage(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{
			name: "corrupt",
			raw:  []byte(`{"access_token":"supersecret"`),
		},
		{
			name: "unsupported version",
			raw:  []byte(`{"version":2,"accounts":{"auth-history":{"requests":9}}}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, usageFileName)
			if err := os.WriteFile(path, tt.raw, 0o600); err != nil {
				t.Fatalf("write invalid usage: %v", err)
			}

			tracker := newUsageTracker(time.Now, time.Hour)
			var saves atomic.Int64
			tracker.save = func(store *stateStore, doc UsageDocument) error {
				saves.Add(1)
				return store.saveUsage(doc)
			}
			if err := tracker.configureStore(newStateStore(dir)); err != nil {
				t.Fatalf("configure invalid store: %v", err)
			}
			tracker.observe(usageRecord("auth-memory", 3, 5))

			errFlush := tracker.flush()
			if errFlush == nil {
				t.Fatal("flush succeeded while usage load was blocked")
			}
			if strings.Contains(errFlush.Error(), "supersecret") {
				t.Fatalf("flush error leaked file content: %v", errFlush)
			}
			assertFileBytes(t, path, tt.raw)
			degraded, message := tracker.health()
			if !degraded || message == "" {
				t.Fatalf("health after flush = (%v, %q), want degraded with message", degraded, message)
			}
			if !tracker.isDirty() {
				t.Fatal("blocked flush cleared dirty usage")
			}

			errShutdown := tracker.shutdown()
			if errShutdown == nil {
				t.Fatal("shutdown succeeded while usage load was blocked")
			}
			if strings.Contains(errShutdown.Error(), "supersecret") {
				t.Fatalf("shutdown error leaked file content: %v", errShutdown)
			}
			assertFileBytes(t, path, tt.raw)
			degraded, message = tracker.health()
			if !degraded || message == "" {
				t.Fatalf("health after shutdown = (%v, %q), want degraded with message", degraded, message)
			}
			if got := saves.Load(); got != 0 {
				t.Fatalf("save attempts = %d, want 0", got)
			}
		})
	}
}

func TestUsageTrackerReconfigureRepairsLoadBlockedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, usageFileName)
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt usage: %v", err)
	}
	now := time.Date(2026, time.July, 27, 20, 0, 0, 0, time.UTC)
	tracker := newUsageTracker(func() time.Time { return now }, time.Hour)
	t.Cleanup(func() {
		if err := tracker.shutdown(); err != nil {
			t.Fatalf("shutdown usage tracker: %v", err)
		}
	})
	store := newStateStore(dir)
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configure corrupt store: %v", err)
	}
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Failed:   true,
		Detail: pluginapi.UsageDetail{
			InputTokens:         10,
			OutputTokens:        10,
			ReasoningTokens:     10,
			CacheReadTokens:     10,
			CacheCreationTokens: 10,
			TotalTokens:         10,
		},
	})
	tracker.observe(usageRecord("auth-memory", 3, 5))

	diskTime := now.Add(-time.Hour)
	if err := store.saveUsage(UsageDocument{
		Version: stateVersion,
		Accounts: map[string]AccountUsage{
			"auth-a": {
				Requests:            math.MaxInt64,
				FailedRequests:      math.MaxInt64,
				InputTokens:         math.MaxInt64 - 5,
				OutputTokens:        math.MaxInt64 - 5,
				ReasoningTokens:     math.MaxInt64 - 5,
				CacheReadTokens:     math.MaxInt64 - 5,
				CacheCreationTokens: math.MaxInt64 - 5,
				TotalTokens:         math.MaxInt64 - 5,
				UpdatedAt:           diskTime,
			},
			"auth-disk": {Requests: 4, TotalTokens: 40, UpdatedAt: diskTime},
		},
	}); err != nil {
		t.Fatalf("repair usage: %v", err)
	}

	if err := tracker.configureStore(newStateStore(filepath.Join(dir, "."))); err != nil {
		t.Fatalf("reconfigure repaired store: %v", err)
	}
	got := tracker.snapshot()
	assertAccountUsage(t, got.Accounts["auth-a"], AccountUsage{
		Requests:            math.MaxInt64,
		FailedRequests:      math.MaxInt64,
		InputTokens:         math.MaxInt64,
		OutputTokens:        math.MaxInt64,
		ReasoningTokens:     math.MaxInt64,
		CacheReadTokens:     math.MaxInt64,
		CacheCreationTokens: math.MaxInt64,
		TotalTokens:         math.MaxInt64,
		UpdatedAt:           now,
	})
	assertAccountUsage(t, got.Accounts["auth-memory"], AccountUsage{
		Requests:     1,
		InputTokens:  3,
		OutputTokens: 5,
		TotalTokens:  8,
		UpdatedAt:    now,
	})
	assertAccountUsage(t, got.Accounts["auth-disk"], AccountUsage{
		Requests:    4,
		TotalTokens: 40,
		UpdatedAt:   diskTime,
	})
	degraded, message := tracker.health()
	if degraded || message != "" {
		t.Fatalf("health after repair = (%v, %q), want healthy", degraded, message)
	}
	if !tracker.isDirty() {
		t.Fatal("reconfigure cleared merged usage")
	}

	if err := tracker.flush(); err != nil {
		t.Fatalf("flush repaired usage: %v", err)
	}
	persisted, err := store.loadUsage()
	if err != nil {
		t.Fatalf("load repaired usage: %v", err)
	}
	assertAccountUsage(t, persisted.Accounts["auth-a"], got.Accounts["auth-a"])
	assertAccountUsage(t, persisted.Accounts["auth-memory"], got.Accounts["auth-memory"])
	assertAccountUsage(t, persisted.Accounts["auth-disk"], got.Accounts["auth-disk"])
}

func TestUsageTrackerRejectsSwitchWhileLoadBlockedAndDirty(t *testing.T) {
	oldDir := t.TempDir()
	oldPath := filepath.Join(oldDir, usageFileName)
	corrupt := []byte(`{"refresh_token":"topsecret"`)
	if err := os.WriteFile(oldPath, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt usage: %v", err)
	}
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
		_ = tracker.shutdown()
	})
	oldStore := newStateStore(oldDir)
	if err := tracker.configureStore(oldStore); err != nil {
		t.Fatalf("configure corrupt store: %v", err)
	}
	tracker.observe(usageRecord("auth-memory", 3, 5))

	err := tracker.configureStore(newStore)
	if err == nil {
		t.Fatal("switch store succeeded while usage load was blocked and dirty")
	}
	if strings.Contains(err.Error(), "topsecret") {
		t.Fatalf("switch error leaked file content: %v", err)
	}
	if tracker.store.dir != oldStore.dir {
		t.Fatalf("usage store switched to %q, want %q", tracker.store.dir, oldStore.dir)
	}
	if got := tracker.snapshot().Accounts["auth-memory"]; got.Requests != 1 || got.TotalTokens != 8 {
		t.Fatalf("in-memory usage was not retained: %#v", got)
	}
	degraded, message := tracker.health()
	if !degraded || message == "" {
		t.Fatalf("health = (%v, %q), want degraded with message", degraded, message)
	}
	assertFileBytes(t, oldPath, corrupt)
}

func TestUsageTrackerAllowsSwitchWhileLoadBlockedAndClean(t *testing.T) {
	oldDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldDir, usageFileName), []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt usage: %v", err)
	}
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
	if err := tracker.configureStore(newStateStore(oldDir)); err != nil {
		t.Fatalf("configure corrupt store: %v", err)
	}
	if err := tracker.configureStore(newStore); err != nil {
		t.Fatalf("switch clean blocked store: %v", err)
	}

	got := tracker.snapshot()
	if len(got.Accounts) != 1 || got.Accounts["auth-new"].Requests != 7 {
		t.Fatalf("new store was not loaded: %#v", got)
	}
	degraded, message := tracker.health()
	if degraded || message != "" {
		t.Fatalf("health after switch = (%v, %q), want healthy", degraded, message)
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

func TestUsageTrackerRejectsConfigureAfterShutdown(t *testing.T) {
	oldStore := newStateStore(t.TempDir())
	newStore := newStateStore(t.TempDir())
	if err := newStore.saveUsage(UsageDocument{
		Version: stateVersion,
		Accounts: map[string]AccountUsage{
			"auth-new": {Requests: 9, TotalTokens: 99},
		},
	}); err != nil {
		t.Fatalf("seed new store: %v", err)
	}
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(oldStore); err != nil {
		t.Fatalf("configure old store: %v", err)
	}
	tracker.observe(usageRecord("auth-old", 3, 5))
	if err := tracker.shutdown(); err != nil {
		t.Fatalf("shutdown usage tracker: %v", err)
	}

	err := tracker.configureStore(newStore)
	if err == nil {
		t.Fatal("configure succeeded after shutdown")
	}
	if tracker.store != oldStore {
		t.Fatal("configure after shutdown replaced the state store")
	}
	got := tracker.snapshot()
	if len(got.Accounts) != 1 || got.Accounts["auth-old"].TotalTokens != 8 {
		t.Fatalf("configure after shutdown replaced usage: %#v", got)
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

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("file %s = %q, want %q", path, got, want)
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

func waitForSignal(t *testing.T, signal <-chan struct{}, timeout time.Duration, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitForResult(t *testing.T, result <-chan error, timeout time.Duration, description string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", description)
		return nil
	}
}
