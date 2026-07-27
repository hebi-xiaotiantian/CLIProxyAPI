# Codex Account Token Usage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist cumulative Codex token usage per `AuthID` and display it in the existing Codex Account Pool management table without affecting routing.

**Architecture:** Add a plugin-local `usageTracker` that consumes host `UsageRecord` values, keeps a mutex-protected aggregate in memory, and coalesces atomic `usage.json` writes to at most once per second. The existing accounts endpoint reads a tracker snapshot and adds an optional usage view; the embedded page refreshes only account data every five seconds while visible and merges it with pending policy edits.

**Tech Stack:** Go 1.26, standard library synchronization and JSON APIs, CLIProxyAPI plugin API, logrus, embedded HTML/CSS/JavaScript, Go tests.

---

## File Structure

- Create `examples/plugin/codex-account-pool/go/usage.go`: account-level aggregation, delayed persistence, retry behavior, snapshots, health, reconfiguration, and shutdown.
- Create `examples/plugin/codex-account-pool/go/usage_test.go`: accounting, filtering, saturation, persistence, retry, restart, reconfiguration, corrupt-state, and shutdown tests.
- Modify `examples/plugin/codex-account-pool/go/model.go`: persisted usage document and account counter types plus normalization helpers.
- Modify `examples/plugin/codex-account-pool/go/store.go`: `usage.json` load/save methods using the existing atomic JSON primitive.
- Modify `examples/plugin/codex-account-pool/go/policy_test.go`: focused usage model/store tests alongside the existing state-store tests.
- Modify `examples/plugin/codex-account-pool/go/plugin.go`: tracker ownership and lifecycle integration while preserving the HTTP 429 refresh path.
- Modify `examples/plugin/codex-account-pool/go/plugin_test.go`: plugin callback and reconfiguration integration tests.
- Modify `examples/plugin/codex-account-pool/go/management.go`: optional account usage serialization and usage persistence health.
- Modify `examples/plugin/codex-account-pool/go/management_test.go`: API shape, omission, sanitization, and embedded-resource checks.
- Modify `examples/plugin/codex-account-pool/go/web/index.html`: Token Usage column, compact formatting, hover details, and visibility-aware account polling.
- Modify `examples/plugin/codex-account-pool/go/go.mod`: direct logrus dependency used by shutdown logging.
- Modify `examples/plugin/codex-account-pool/go/go.sum`: dependency metadata produced by `go mod tidy`.
- Modify `examples/plugin/codex-account-pool/README.md`: document local cumulative usage semantics and `usage.json`.

### Task 1: Usage State Model and Atomic Store

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/model.go`
- Modify: `examples/plugin/codex-account-pool/go/store.go`
- Modify: `examples/plugin/codex-account-pool/go/policy_test.go`

- [ ] **Step 1: Write failing model and store tests**

Append these tests to `examples/plugin/codex-account-pool/go/policy_test.go`:

```go
func TestNormalizeUsageDocumentClampsCountersAndKeys(t *testing.T) {
	updatedAt := time.Date(2026, 7, 27, 10, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	got, err := normalizeUsageDocument(UsageDocument{
		Accounts: map[string]AccountUsage{
			" auth-a ": {
				Requests:            -1,
				FailedRequests:      -2,
				InputTokens:         -3,
				OutputTokens:        -4,
				ReasoningTokens:     -5,
				CacheReadTokens:     -6,
				CacheCreationTokens: -7,
				TotalTokens:         -8,
				UpdatedAt:           updatedAt,
			},
		},
	})
	if err != nil {
		t.Fatalf("normalizeUsageDocument() error = %v", err)
	}
	account, ok := got.Accounts["auth-a"]
	if !ok {
		t.Fatalf("normalized accounts = %#v", got.Accounts)
	}
	if got.Version != stateVersion {
		t.Fatalf("version = %d, want %d", got.Version, stateVersion)
	}
	if account.Requests != 0 || account.FailedRequests != 0 ||
		account.InputTokens != 0 || account.OutputTokens != 0 ||
		account.ReasoningTokens != 0 || account.CacheReadTokens != 0 ||
		account.CacheCreationTokens != 0 || account.TotalTokens != 0 {
		t.Fatalf("normalized account = %#v", account)
	}
	if account.UpdatedAt.Location() != time.UTC {
		t.Fatalf("updated_at location = %v, want UTC", account.UpdatedAt.Location())
	}
}

func TestNormalizeUsageDocumentRejectsUnsupportedVersion(t *testing.T) {
	_, err := normalizeUsageDocument(UsageDocument{
		Version:  stateVersion + 1,
		Accounts: map[string]AccountUsage{},
	})
	if err == nil {
		t.Fatal("normalizeUsageDocument() error = nil, want unsupported version")
	}
}

func TestSaturatingAddCapsAtMaxInt64(t *testing.T) {
	if got := saturatingAdd(math.MaxInt64-2, 10); got != math.MaxInt64 {
		t.Fatalf("saturatingAdd() = %d, want %d", got, int64(math.MaxInt64))
	}
	if got := saturatingAdd(10, -1); got != 10 {
		t.Fatalf("saturatingAdd() with negative delta = %d, want 10", got)
	}
}

func TestStateStoreUsageRoundTripUsesPrivateFile(t *testing.T) {
	store := newStateStore(t.TempDir())
	want := defaultUsageDocument()
	want.Accounts["auth-a"] = AccountUsage{
		Requests:            3,
		FailedRequests:      1,
		InputTokens:         100,
		OutputTokens:        25,
		ReasoningTokens:     10,
		CacheReadTokens:     40,
		CacheCreationTokens: 5,
		TotalTokens:         125,
		UpdatedAt:           time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC),
	}

	if err := store.saveUsage(want); err != nil {
		t.Fatalf("saveUsage() error = %v", err)
	}
	got, err := store.loadUsage()
	if err != nil {
		t.Fatalf("loadUsage() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded usage = %#v, want %#v", got, want)
	}
	info, err := os.Stat(filepath.Join(store.dir, usageFileName))
	if err != nil {
		t.Fatalf("stat usage file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("usage mode = %o, want 600", info.Mode().Perm())
	}
}

func TestStateStoreMissingUsageStartsEmpty(t *testing.T) {
	got, err := newStateStore(t.TempDir()).loadUsage()
	if err != nil {
		t.Fatalf("loadUsage() error = %v", err)
	}
	if got.Version != stateVersion || len(got.Accounts) != 0 {
		t.Fatalf("missing usage document = %#v", got)
	}
}
```

Add `"math"` and `"reflect"` to the test file imports.

- [ ] **Step 2: Run the focused tests and confirm they fail**

Run:

```bash
cd examples/plugin/codex-account-pool/go
go test -run 'TestNormalizeUsageDocument|TestSaturatingAdd|TestStateStoreUsage|TestStateStoreMissingUsage' ./...
```

Expected: FAIL with undefined `UsageDocument`, `AccountUsage`, `normalizeUsageDocument`, `saturatingAdd`, `usageFileName`, `saveUsage`, and `loadUsage`.

- [ ] **Step 3: Add the persisted model and normalization**

Add `"math"` to `examples/plugin/codex-account-pool/go/model.go`, then place these types after `QuotaDocument`:

```go
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
```

Add these helpers after `defaultQuotaDocument`:

```go
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
```

Add this normalizer after `normalizeQuotaDocument`:

```go
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
```

- [ ] **Step 4: Add `usage.json` load/save methods**

Change the constants in `examples/plugin/codex-account-pool/go/store.go` to:

```go
const (
	policyFileName = "policy.json"
	quotaFileName  = "quota.json"
	usageFileName  = "usage.json"
)
```

Add these methods after `saveQuota`:

```go
func (s *stateStore) loadUsage() (UsageDocument, error) {
	doc := defaultUsageDocument()
	err := s.readJSON(usageFileName, &doc)
	if os.IsNotExist(err) {
		return doc, nil
	}
	if err != nil {
		return UsageDocument{}, err
	}
	return normalizeUsageDocument(doc)
}

func (s *stateStore) saveUsage(doc UsageDocument) error {
	normalized, err := normalizeUsageDocument(doc)
	if err != nil {
		return err
	}
	return s.writeJSON(usageFileName, normalized)
}
```

- [ ] **Step 5: Format and verify the state tests pass**

Run:

```bash
cd examples/plugin/codex-account-pool/go
gofmt -w model.go store.go policy_test.go
go test -run 'TestNormalizeUsageDocument|TestSaturatingAdd|TestStateStoreUsage|TestStateStoreMissingUsage' ./...
```

Expected: PASS.

- [ ] **Step 6: Commit the state model and store**

```bash
git add examples/plugin/codex-account-pool/go/model.go \
  examples/plugin/codex-account-pool/go/store.go \
  examples/plugin/codex-account-pool/go/policy_test.go
git commit -m "feat: add codex account usage state"
```

### Task 2: Usage Aggregation and Persistence Lifecycle

**Files:**
- Create: `examples/plugin/codex-account-pool/go/usage.go`
- Create: `examples/plugin/codex-account-pool/go/usage_test.go`

- [ ] **Step 1: Write failing accounting tests**

Create `examples/plugin/codex-account-pool/go/usage_test.go` with:

```go
package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUsageTrackerAggregatesCodexRecordsByAccount(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	tracker := newUsageTracker(func() time.Time { return now }, time.Hour)
	if err := tracker.configureStore(newStateStore(t.TempDir())); err != nil {
		t.Fatalf("configureStore() error = %v", err)
	}
	t.Cleanup(func() { _ = tracker.shutdown() })

	tracker.observe(pluginapi.UsageRecord{
		Provider: "claude",
		AuthID:   "ignored-provider",
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "   ",
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "CODEX",
		AuthID:   " auth-a ",
		Failed:   true,
		Detail: pluginapi.UsageDetail{
			InputTokens:         100,
			OutputTokens:        25,
			ReasoningTokens:     10,
			CachedTokens:        30,
			CacheReadTokens:     20,
			CacheCreationTokens: 5,
		},
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail: pluginapi.UsageDetail{
			InputTokens:     50,
			OutputTokens:    10,
			ReasoningTokens: 4,
			CachedTokens:    8,
			CacheReadTokens: 12,
			TotalTokens:     999,
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

	doc := tracker.snapshot()
	if len(doc.Accounts) != 3 {
		t.Fatalf("usage accounts = %#v", doc.Accounts)
	}
	accountA := doc.Accounts["auth-a"]
	if accountA.Requests != 2 || accountA.FailedRequests != 1 {
		t.Fatalf("auth-a request counters = %#v", accountA)
	}
	if accountA.InputTokens != 150 || accountA.OutputTokens != 35 ||
		accountA.ReasoningTokens != 14 || accountA.CacheReadTokens != 42 ||
		accountA.CacheCreationTokens != 5 || accountA.TotalTokens != 1124 {
		t.Fatalf("auth-a token counters = %#v", accountA)
	}
	if !accountA.UpdatedAt.Equal(now) {
		t.Fatalf("auth-a updated_at = %v, want %v", accountA.UpdatedAt, now)
	}
	accountB := doc.Accounts["auth-b"]
	if accountB.Requests != 1 || accountB.FailedRequests != 1 || accountB.TotalTokens != 0 {
		t.Fatalf("auth-b counters = %#v", accountB)
	}
	accountC := doc.Accounts["auth-c"]
	if accountC.Requests != 1 || accountC.TotalTokens != 0 ||
		accountC.InputTokens != 0 || accountC.CacheReadTokens != 0 {
		t.Fatalf("auth-c counters = %#v", accountC)
	}
}

func TestUsageTrackerSaturatesEveryCounter(t *testing.T) {
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(newStateStore(t.TempDir())); err != nil {
		t.Fatalf("configureStore() error = %v", err)
	}
	t.Cleanup(func() { _ = tracker.shutdown() })

	tracker.mu.Lock()
	tracker.doc.Accounts["auth-a"] = AccountUsage{
		Requests:            math.MaxInt64,
		FailedRequests:      math.MaxInt64,
		InputTokens:         math.MaxInt64,
		OutputTokens:        math.MaxInt64,
		ReasoningTokens:     math.MaxInt64,
		CacheReadTokens:     math.MaxInt64,
		CacheCreationTokens: math.MaxInt64,
		TotalTokens:         math.MaxInt64,
	}
	tracker.mu.Unlock()

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
	if got.Requests != math.MaxInt64 || got.FailedRequests != math.MaxInt64 ||
		got.InputTokens != math.MaxInt64 || got.OutputTokens != math.MaxInt64 ||
		got.ReasoningTokens != math.MaxInt64 || got.CacheReadTokens != math.MaxInt64 ||
		got.CacheCreationTokens != math.MaxInt64 || got.TotalTokens != math.MaxInt64 {
		t.Fatalf("saturated account = %#v", got)
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
```

- [ ] **Step 2: Add failing persistence and lifecycle tests**

Append these tests to `examples/plugin/codex-account-pool/go/usage_test.go`:

```go
func TestUsageTrackerCoalescesWritesAndReloadsState(t *testing.T) {
	store := newStateStore(t.TempDir())
	tracker := newUsageTracker(time.Now, 25*time.Millisecond)
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configureStore() error = %v", err)
	}
	realSave := tracker.save
	var attempts atomic.Int32
	tracker.save = func(store *stateStore, doc UsageDocument) error {
		attempts.Add(1)
		return realSave(store, doc)
	}

	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 2},
	})
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{InputTokens: 20, OutputTokens: 3},
	})

	waitForCondition(t, time.Second, func() bool {
		_, err := os.Stat(filepath.Join(store.dir, usageFileName))
		return err == nil
	})
	time.Sleep(40 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("save attempts = %d, want 1 coalesced write", got)
	}
	if err := tracker.shutdown(); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	reloaded := newUsageTracker(time.Now, time.Hour)
	if err := reloaded.configureStore(store); err != nil {
		t.Fatalf("reload configureStore() error = %v", err)
	}
	t.Cleanup(func() { _ = reloaded.shutdown() })
	got := reloaded.snapshot().Accounts["auth-a"]
	if got.Requests != 2 || got.InputTokens != 30 || got.OutputTokens != 5 || got.TotalTokens != 35 {
		t.Fatalf("reloaded account = %#v", got)
	}
}

func TestUsageTrackerRetriesFailedSaveAndSanitizesHealth(t *testing.T) {
	store := newStateStore(t.TempDir())
	tracker := newUsageTracker(time.Now, 100*time.Millisecond)
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configureStore() error = %v", err)
	}
	t.Cleanup(func() { _ = tracker.shutdown() })
	realSave := tracker.save
	firstFailure := make(chan struct{})
	var attempts atomic.Int32
	tracker.save = func(store *stateStore, doc UsageDocument) error {
		if attempts.Add(1) == 1 {
			close(firstFailure)
			return errors.New("access_token=secret-value")
		}
		return realSave(store, doc)
	}

	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{TotalTokens: 9},
	})
	<-firstFailure
	waitForCondition(t, time.Second, func() bool {
		degraded, _ := tracker.health()
		return degraded
	})
	degraded, message := tracker.health()
	if !degraded || message == "" || strings.Contains(message, "secret-value") {
		t.Fatalf("health after failure = degraded:%v message:%q", degraded, message)
	}
	waitForCondition(t, time.Second, func() bool {
		degraded, _ := tracker.health()
		return attempts.Load() >= 2 && !degraded
	})
	loaded, err := store.loadUsage()
	if err != nil {
		t.Fatalf("loadUsage() error = %v", err)
	}
	if loaded.Accounts["auth-a"].TotalTokens != 9 {
		t.Fatalf("persisted account = %#v", loaded.Accounts["auth-a"])
	}
}

func TestUsageTrackerStateDirectorySwitchFlushesOldAndLoadsNew(t *testing.T) {
	oldStore := newStateStore(t.TempDir())
	newStore := newStateStore(t.TempDir())
	newDoc := defaultUsageDocument()
	newDoc.Accounts["auth-new"] = AccountUsage{Requests: 4, TotalTokens: 40}
	if err := newStore.saveUsage(newDoc); err != nil {
		t.Fatalf("save new usage state: %v", err)
	}
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(oldStore); err != nil {
		t.Fatalf("configure old store: %v", err)
	}
	t.Cleanup(func() { _ = tracker.shutdown() })
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-old",
		Detail:   pluginapi.UsageDetail{TotalTokens: 7},
	})

	if err := tracker.configureStore(newStore); err != nil {
		t.Fatalf("configure new store: %v", err)
	}
	oldDoc, err := oldStore.loadUsage()
	if err != nil {
		t.Fatalf("load old usage state: %v", err)
	}
	if oldDoc.Accounts["auth-old"].TotalTokens != 7 {
		t.Fatalf("old persisted account = %#v", oldDoc.Accounts["auth-old"])
	}
	snapshot := tracker.snapshot()
	if _, exists := snapshot.Accounts["auth-old"]; exists {
		t.Fatalf("old account remained after state switch: %#v", snapshot.Accounts)
	}
	if snapshot.Accounts["auth-new"].TotalTokens != 40 {
		t.Fatalf("new loaded account = %#v", snapshot.Accounts["auth-new"])
	}
}

func TestUsageTrackerRejectsStateDirectorySwitchWhenOldFlushFails(t *testing.T) {
	oldStore := newStateStore(t.TempDir())
	newStore := newStateStore(t.TempDir())
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(oldStore); err != nil {
		t.Fatalf("configure old store: %v", err)
	}
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-old",
		Detail:   pluginapi.UsageDetail{TotalTokens: 7},
	})
	tracker.save = func(*stateStore, UsageDocument) error {
		return errors.New("write denied")
	}

	if err := tracker.configureStore(newStore); err == nil {
		t.Fatal("configureStore() error = nil, want old-state flush failure")
	}
	tracker.mu.Lock()
	activeDir := tracker.store.dir
	tracker.mu.Unlock()
	if activeDir != oldStore.dir {
		t.Fatalf("active usage directory = %q, want %q", activeDir, oldStore.dir)
	}
	if tracker.snapshot().Accounts["auth-old"].TotalTokens != 7 {
		t.Fatalf("usage snapshot changed after rejected switch: %#v", tracker.snapshot())
	}
	_ = tracker.shutdown()
}

func TestUsageTrackerCorruptStateStartsEmptyAndReportsDegraded(t *testing.T) {
	store := newStateStore(t.TempDir())
	if err := os.WriteFile(filepath.Join(store.dir, usageFileName), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write corrupt usage state: %v", err)
	}
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configureStore() error = %v", err)
	}
	t.Cleanup(func() { _ = tracker.shutdown() })
	if len(tracker.snapshot().Accounts) != 0 {
		t.Fatalf("corrupt-state snapshot = %#v", tracker.snapshot())
	}
	degraded, message := tracker.health()
	if !degraded || message == "" {
		t.Fatalf("corrupt-state health = degraded:%v message:%q", degraded, message)
	}
}

func TestUsageTrackerShutdownPerformsFinalFlush(t *testing.T) {
	store := newStateStore(t.TempDir())
	tracker := newUsageTracker(time.Now, time.Hour)
	if err := tracker.configureStore(store); err != nil {
		t.Fatalf("configureStore() error = %v", err)
	}
	tracker.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{InputTokens: 6, OutputTokens: 4},
	})
	if err := tracker.shutdown(); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	loaded, err := store.loadUsage()
	if err != nil {
		t.Fatalf("loadUsage() error = %v", err)
	}
	if loaded.Accounts["auth-a"].TotalTokens != 10 {
		t.Fatalf("shutdown-persisted account = %#v", loaded.Accounts["auth-a"])
	}
}
```

- [ ] **Step 3: Run the tracker tests and confirm they fail**

Run:

```bash
cd examples/plugin/codex-account-pool/go
go test -run 'TestUsageTracker' ./...
```

Expected: FAIL with undefined `usageTracker` and `newUsageTracker`.

- [ ] **Step 4: Implement the tracker**

Create `examples/plugin/codex-account-pool/go/usage.go`:

```go
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const usageFlushInterval = time.Second

type usageTracker struct {
	mu            sync.Mutex
	persistMu     sync.Mutex
	store         *stateStore
	doc           UsageDocument
	dirty         bool
	revision      uint64
	degraded      bool
	lastError     string
	accepting     bool
	now           func() time.Time
	flushInterval time.Duration
	wake          chan struct{}
	stop          chan struct{}
	done          chan struct{}
	stopOnce      sync.Once
	save          func(*stateStore, UsageDocument) error
}

func newUsageTracker(now func() time.Time, flushInterval time.Duration) *usageTracker {
	if now == nil {
		now = time.Now
	}
	if flushInterval <= 0 {
		flushInterval = usageFlushInterval
	}
	tracker := &usageTracker{
		doc:           defaultUsageDocument(),
		accepting:     true,
		now:           now,
		flushInterval: flushInterval,
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	tracker.save = func(store *stateStore, doc UsageDocument) error {
		return store.saveUsage(doc)
	}
	go tracker.run()
	return tracker
}

func (t *usageTracker) observe(record pluginapi.UsageRecord) {
	if t == nil || !strings.EqualFold(strings.TrimSpace(record.Provider), "codex") {
		return
	}
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return
	}
	inputTokens := nonNegativeCounter(record.Detail.InputTokens)
	outputTokens := nonNegativeCounter(record.Detail.OutputTokens)
	reasoningTokens := nonNegativeCounter(record.Detail.ReasoningTokens)
	cacheReadTokens := nonNegativeCounter(record.Detail.CacheReadTokens)
	if legacyCached := nonNegativeCounter(record.Detail.CachedTokens); legacyCached > cacheReadTokens {
		cacheReadTokens = legacyCached
	}
	cacheCreationTokens := nonNegativeCounter(record.Detail.CacheCreationTokens)
	totalTokens := nonNegativeCounter(record.Detail.TotalTokens)
	if totalTokens == 0 {
		totalTokens = saturatingAdd(inputTokens, outputTokens)
	}
	updatedAt := t.now().UTC()

	t.mu.Lock()
	if !t.accepting {
		t.mu.Unlock()
		return
	}
	account := t.doc.Accounts[authID]
	account.Requests = saturatingAdd(account.Requests, 1)
	if record.Failed {
		account.FailedRequests = saturatingAdd(account.FailedRequests, 1)
	}
	account.InputTokens = saturatingAdd(account.InputTokens, inputTokens)
	account.OutputTokens = saturatingAdd(account.OutputTokens, outputTokens)
	account.ReasoningTokens = saturatingAdd(account.ReasoningTokens, reasoningTokens)
	account.CacheReadTokens = saturatingAdd(account.CacheReadTokens, cacheReadTokens)
	account.CacheCreationTokens = saturatingAdd(account.CacheCreationTokens, cacheCreationTokens)
	account.TotalTokens = saturatingAdd(account.TotalTokens, totalTokens)
	account.UpdatedAt = updatedAt
	t.doc.Accounts[authID] = account
	t.dirty = true
	t.revision++
	t.mu.Unlock()
	t.notify()
}

func (t *usageTracker) snapshot() UsageDocument {
	if t == nil {
		return defaultUsageDocument()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return cloneUsageDocument(t.doc)
}

func (t *usageTracker) health() (bool, string) {
	if t == nil {
		return false, ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.degraded, t.lastError
}

func (t *usageTracker) configureStore(next *stateStore) error {
	if t == nil {
		return fmt.Errorf("usage tracker is unavailable")
	}
	if next == nil {
		return fmt.Errorf("usage store is unavailable")
	}
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.accepting {
		return fmt.Errorf("usage tracker is stopped")
	}
	if t.store != nil && t.store.dir == next.dir {
		return nil
	}
	if t.store != nil && t.dirty {
		if errSave := t.save(t.store, cloneUsageDocument(t.doc)); errSave != nil {
			t.setPersistenceErrorLocked(fmt.Errorf("flush previous usage state: %w", errSave))
			return fmt.Errorf("flush previous usage state: %w", errSave)
		}
	}
	loaded, errLoad := next.loadUsage()
	t.store = next
	t.revision++
	t.dirty = false
	if errLoad != nil {
		t.doc = defaultUsageDocument()
		t.setPersistenceErrorLocked(fmt.Errorf("load usage state: %w", errLoad))
		return nil
	}
	t.doc = loaded
	t.degraded = false
	t.lastError = ""
	return nil
}

func (t *usageTracker) shutdown() error {
	if t == nil {
		return nil
	}
	t.stopOnce.Do(func() {
		t.mu.Lock()
		t.accepting = false
		t.mu.Unlock()
		close(t.stop)
		<-t.done
	})
	return t.flush()
}

func (t *usageTracker) run() {
	defer close(t.done)
	var timer *time.Timer
	var timerC <-chan time.Time
	schedule := func() {
		if timer != nil {
			return
		}
		timer = time.NewTimer(t.flushInterval)
		timerC = timer.C
	}
	for {
		select {
		case <-t.wake:
			schedule()
		case <-timerC:
			timer = nil
			timerC = nil
			_ = t.flush()
			if t.isDirty() {
				schedule()
			}
		case <-t.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		}
	}
}

func (t *usageTracker) notify() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *usageTracker) isDirty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dirty
}

func (t *usageTracker) flush() error {
	t.persistMu.Lock()
	defer t.persistMu.Unlock()

	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return nil
	}
	if t.store == nil {
		errStore := fmt.Errorf("usage store is unavailable")
		t.setPersistenceErrorLocked(errStore)
		t.mu.Unlock()
		return errStore
	}
	store := t.store
	revision := t.revision
	doc := cloneUsageDocument(t.doc)
	t.mu.Unlock()

	errSave := t.save(store, doc)
	t.mu.Lock()
	defer t.mu.Unlock()
	if errSave != nil {
		t.setPersistenceErrorLocked(errSave)
		return errSave
	}
	if t.store == store && t.revision == revision {
		t.dirty = false
	}
	t.degraded = false
	t.lastError = ""
	return nil
}

func (t *usageTracker) setPersistenceErrorLocked(err error) {
	t.degraded = true
	t.lastError = sanitizeError(err)
}

func cloneUsageDocument(doc UsageDocument) UsageDocument {
	out := doc
	out.Accounts = make(map[string]AccountUsage, len(doc.Accounts))
	for authID, account := range doc.Accounts {
		out.Accounts[authID] = account
	}
	return out
}
```

- [ ] **Step 5: Format and run the tracker tests**

Run:

```bash
cd examples/plugin/codex-account-pool/go
gofmt -w usage.go usage_test.go
go test -run 'TestUsageTracker' ./...
```

Expected: PASS.

- [ ] **Step 6: Run the complete plugin test suite**

Run:

```bash
cd examples/plugin/codex-account-pool/go
go test ./...
```

Expected: PASS.

- [ ] **Step 7: Commit the tracker**

```bash
git add examples/plugin/codex-account-pool/go/usage.go \
  examples/plugin/codex-account-pool/go/usage_test.go
git commit -m "feat: aggregate codex account token usage"
```

### Task 3: Plugin Callback and Lifecycle Integration

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/plugin.go`
- Modify: `examples/plugin/codex-account-pool/go/plugin_test.go`
- Modify: `examples/plugin/codex-account-pool/go/management_test.go`
- Modify: `examples/plugin/codex-account-pool/go/go.mod`
- Modify: `examples/plugin/codex-account-pool/go/go.sum`

- [ ] **Step 1: Expand the HTTP 429 usage callback test**

In `examples/plugin/codex-account-pool/go/plugin_test.go`, add `"errors"` to the imports and replace `TestUsage429RecordsFailureBeforeDelayedRefresh` with:

```go
func TestUsage429AggregatesTokensBeforeDelayedRefresh(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.inventory = []pluginapi.HostAuthFileEntry{{
		ID: "auth-a", AuthIndex: "index-a", Provider: "codex",
	}}
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"auth-a": {
				AuthID:            "auth-a",
				Plan:              PlanFree,
				FiveHourRemaining: 40,
				WeeklyRemaining:   60,
				RefreshedAt:       time.Now(),
			},
		},
	})
	record, err := json.Marshal(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Failed:   true,
		Failure:  pluginapi.UsageFailure{StatusCode: 429},
		Detail: pluginapi.UsageDetail{
			InputTokens:     80,
			OutputTokens:    20,
			ReasoningTokens: 7,
			CachedTokens:    30,
		},
	})
	if err != nil {
		t.Fatalf("marshal usage record: %v", err)
	}
	if _, errHandle := plugin.handleUsage(record); errHandle != nil {
		t.Fatalf("handleUsage() error = %v", errHandle)
	}
	usage := plugin.usage.snapshot().Accounts["auth-a"]
	if usage.Requests != 1 || usage.FailedRequests != 1 ||
		usage.InputTokens != 80 || usage.OutputTokens != 20 ||
		usage.ReasoningTokens != 7 || usage.CacheReadTokens != 30 ||
		usage.TotalTokens != 100 {
		t.Fatalf("usage after 429 = %#v", usage)
	}
	quota := plugin.quota.Load().Accounts["auth-a"]
	if quota.FiveHourRemaining != 40 || quota.Refresh.State != "failed" {
		t.Fatalf("quota after 429 = %#v", quota)
	}
}
```

- [ ] **Step 2: Add reconfiguration integration tests**

Append these tests to `examples/plugin/codex-account-pool/go/plugin_test.go`:

```go
func TestReconfigureSameStateDirectoryPreservesUsageTrackerState(t *testing.T) {
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{}),
	}}
	plugin := newAccountPoolPlugin(host)
	dir := t.TempDir()
	request, err := json.Marshal(lifecycleRequest{
		ConfigYAML: []byte("state_dir: " + dir + "\nrefresh_interval: 1h\n"),
	})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	if errConfigure := plugin.configure(request); errConfigure != nil {
		t.Fatalf("first configure() error = %v", errConfigure)
	}
	t.Cleanup(plugin.shutdown)
	plugin.usage.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{TotalTokens: 11},
	})

	if errConfigure := plugin.configure(request); errConfigure != nil {
		t.Fatalf("second configure() error = %v", errConfigure)
	}
	got := plugin.usage.snapshot().Accounts["auth-a"]
	if got.Requests != 1 || got.TotalTokens != 11 {
		t.Fatalf("usage after same-directory configure = %#v", got)
	}
}

func TestReconfigureAbortsBeforeStoppingCoordinatorWhenUsageFlushFails(t *testing.T) {
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{}),
	}}
	plugin := newAccountPoolPlugin(host)
	oldDir := t.TempDir()
	oldRequest, err := json.Marshal(lifecycleRequest{
		ConfigYAML: []byte("state_dir: " + oldDir + "\nrefresh_interval: 1h\n"),
	})
	if err != nil {
		t.Fatalf("marshal old lifecycle request: %v", err)
	}
	if errConfigure := plugin.configure(oldRequest); errConfigure != nil {
		t.Fatalf("configure old state: %v", errConfigure)
	}
	t.Cleanup(plugin.shutdown)
	oldCoordinator := plugin.coordinator
	realSave := plugin.usage.save
	plugin.usage.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{TotalTokens: 11},
	})
	plugin.usage.save = func(*stateStore, UsageDocument) error {
		return errors.New("usage write denied")
	}

	newDir := t.TempDir()
	newRequest, err := json.Marshal(lifecycleRequest{
		ConfigYAML: []byte("state_dir: " + newDir + "\nrefresh_interval: 1h\n"),
	})
	if err != nil {
		t.Fatalf("marshal new lifecycle request: %v", err)
	}
	if errConfigure := plugin.configure(newRequest); errConfigure == nil {
		t.Fatal("configure() error = nil, want usage flush failure")
	}
	plugin.usage.save = realSave
	if plugin.config.StateDir != oldDir {
		t.Fatalf("plugin state directory = %q, want %q", plugin.config.StateDir, oldDir)
	}
	if plugin.coordinator != oldCoordinator {
		t.Fatal("coordinator changed after rejected usage state switch")
	}
	plugin.usage.mu.Lock()
	activeUsageDir := plugin.usage.store.dir
	plugin.usage.mu.Unlock()
	if activeUsageDir != oldDir {
		t.Fatalf("usage state directory = %q, want %q", activeUsageDir, oldDir)
	}
}
```

- [ ] **Step 3: Make the shared test plugin configure and clean up its tracker**

Replace `newTestPlugin` in `examples/plugin/codex-account-pool/go/management_test.go` with:

```go
func newTestPlugin(t *testing.T) *accountPoolPlugin {
	t.Helper()
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{}),
	}}
	plugin := newAccountPoolPlugin(host)
	cfg := defaultConfig()
	cfg.StateDir = t.TempDir()
	cfg.StalePolicy = StaleAllow
	plugin.config = cfg
	plugin.store = newStateStore(cfg.StateDir)
	if err := plugin.usage.configureStore(plugin.store); err != nil {
		t.Fatalf("configure usage tracker: %v", err)
	}
	t.Cleanup(plugin.shutdown)
	return plugin
}
```

- [ ] **Step 4: Run the integration tests and confirm they fail**

Run:

```bash
cd examples/plugin/codex-account-pool/go
go test -run 'TestUsage429|TestReconfigureSameStateDirectory|TestReconfigureAbortsBeforeStoppingCoordinator' ./...
```

Expected: FAIL because `accountPoolPlugin` does not own or call a usage tracker.

- [ ] **Step 5: Wire the tracker into plugin construction, configuration, usage handling, and shutdown**

Add logrus to `examples/plugin/codex-account-pool/go/plugin.go` imports:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sirupsen/logrus"
)
```

Add a tracker field to `accountPoolPlugin`:

```go
	usage          *usageTracker
```

Initialize it in `newAccountPoolPlugin`:

```go
	plugin := &accountPoolPlugin{
		host:           host,
		selector:       newSelector(time.Now),
		usage:          newUsageTracker(time.Now, usageFlushInterval),
		config:         defaultConfig(),
		delayedRefresh: make(map[string]*time.Timer),
	}
```

In `configure`, create and switch the usage store immediately after acquiring `lifecycleMu`, before stopping the old refresh coordinator:

```go
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	store := newStateStore(cfg.StateDir)
	if errUsage := p.usage.configureStore(store); errUsage != nil {
		return fmt.Errorf("configure usage state: %w", errUsage)
	}
	p.stopDelayedRefreshes()
```

Remove the later duplicate:

```go
	store := newStateStore(cfg.StateDir)
```

Replace `shutdown` with:

```go
func (p *accountPoolPlugin) shutdown() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.stopDelayedRefreshes()
	p.configMu.Lock()
	coordinator := p.coordinator
	p.coordinator = nil
	p.configMu.Unlock()
	if coordinator != nil {
		coordinator.stop()
	}
	if errUsage := p.usage.shutdown(); errUsage != nil {
		logrus.WithField("error", sanitizeText(errUsage.Error())).
			Warn("codex account pool usage flush failed during shutdown")
	}
}
```

Call the tracker before the existing 429 behavior:

```go
func (p *accountPoolPlugin) handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, fmt.Errorf("decode usage record: %w", errUnmarshal)
	}
	p.usage.observe(record)
	if strings.EqualFold(record.Provider, "codex") && record.Failed && record.Failure.StatusCode == 429 {
		p.applyRefreshResult(QuotaSnapshot{AuthID: record.AuthID}, fmt.Errorf("codex request returned HTTP 429"))
		p.queueRefreshByID(record.AuthID, 30*time.Second)
	}
	return okEnvelope(map[string]any{})
}
```

- [ ] **Step 6: Add the direct logrus dependency and tidy the plugin module**

Change the requirement block in `examples/plugin/codex-account-pool/go/go.mod` to:

```go
require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
	github.com/sirupsen/logrus v1.9.3
	gopkg.in/yaml.v3 v3.0.1
)
```

Run:

```bash
cd examples/plugin/codex-account-pool/go
go mod tidy
```

Expected: `go.mod` and `go.sum` are consistent and logrus is available to the plugin build.

- [ ] **Step 7: Format and verify plugin integration**

Run:

```bash
cd examples/plugin/codex-account-pool/go
gofmt -w plugin.go plugin_test.go management_test.go
go test -run 'TestUsage429|TestReconfigureSameStateDirectory|TestReconfigureAbortsBeforeStoppingCoordinator' ./...
go test ./...
```

Expected: PASS.

- [ ] **Step 8: Commit lifecycle integration**

```bash
git add examples/plugin/codex-account-pool/go/plugin.go \
  examples/plugin/codex-account-pool/go/plugin_test.go \
  examples/plugin/codex-account-pool/go/management_test.go \
  examples/plugin/codex-account-pool/go/go.mod \
  examples/plugin/codex-account-pool/go/go.sum
git commit -m "feat: persist usage through plugin lifecycle"
```

### Task 4: Management API Usage Views and Health

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/management.go`
- Modify: `examples/plugin/codex-account-pool/go/management_test.go`

- [ ] **Step 1: Write failing account usage serialization tests**

Add `"errors"` and `"net/http"` to `examples/plugin/codex-account-pool/go/management_test.go` imports, then append:

```go
func TestManagementAccountsIncludeUsageOnlyForVisibleObservedAccounts(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.host = &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{
			Files: []pluginapi.HostAuthFileEntry{
				{ID: "auth-a", Provider: "codex", Email: "a@example.com"},
				{ID: "auth-b", Provider: "codex", Email: "b@example.com"},
			},
		}),
	}}
	plugin.usage.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Failed:   true,
		Detail: pluginapi.UsageDetail{
			InputTokens:         100,
			OutputTokens:        25,
			ReasoningTokens:     10,
			CacheReadTokens:     30,
			CacheCreationTokens: 5,
		},
	})
	plugin.usage.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "temporarily-absent",
		Detail:   pluginapi.UsageDetail{TotalTokens: 999},
	})

	response, err := plugin.managementResponse(managementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/codex-account-pool/accounts",
	})
	if err != nil {
		t.Fatalf("managementResponse() error = %v", err)
	}
	var payload struct {
		Accounts []accountView `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode accounts response: %v", errUnmarshal)
	}
	if len(payload.Accounts) != 2 {
		t.Fatalf("accounts response = %#v", payload.Accounts)
	}
	byID := make(map[string]accountView, len(payload.Accounts))
	for _, account := range payload.Accounts {
		byID[account.ID] = account
	}
	usage := byID["auth-a"].Usage
	if usage == nil {
		t.Fatal("auth-a usage = nil")
	}
	if usage.Requests != 1 || usage.FailedRequests != 1 ||
		usage.InputTokens != 100 || usage.OutputTokens != 25 ||
		usage.ReasoningTokens != 10 || usage.CacheTokens != 35 ||
		usage.TotalTokens != 125 {
		t.Fatalf("auth-a usage view = %#v", usage)
	}
	if byID["auth-b"].Usage != nil {
		t.Fatalf("auth-b usage = %#v, want nil", byID["auth-b"].Usage)
	}
	if strings.Contains(string(response.Body), "temporarily-absent") {
		t.Fatalf("accounts response exposed absent account usage: %s", response.Body)
	}
}

func TestManagementStatusReportsSanitizedUsagePersistenceHealth(t *testing.T) {
	plugin := newTestPlugin(t)
	realSave := plugin.usage.save
	plugin.usage.save = func(*stateStore, UsageDocument) error {
		return errors.New("access_token=secret-value")
	}
	plugin.usage.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{TotalTokens: 1},
	})
	if errFlush := plugin.usage.flush(); errFlush == nil {
		t.Fatal("flush() error = nil, want persistence failure")
	}

	status := plugin.statusView()
	if status["usage_degraded"] != true {
		t.Fatalf("usage_degraded = %#v, want true", status["usage_degraded"])
	}
	message, ok := status["usage_error"].(string)
	if !ok || message == "" || strings.Contains(message, "secret-value") {
		t.Fatalf("usage_error = %#v", status["usage_error"])
	}
	plugin.usage.save = realSave
	if errFlush := plugin.usage.flush(); errFlush != nil {
		t.Fatalf("recovery flush() error = %v", errFlush)
	}
}
```

- [ ] **Step 2: Run the management tests and confirm they fail**

Run:

```bash
cd examples/plugin/codex-account-pool/go
go test -run 'TestManagementAccountsIncludeUsage|TestManagementStatusReportsSanitizedUsage' ./...
```

Expected: FAIL because `accountView` has no `Usage` field and `statusView` has no usage health.

- [ ] **Step 3: Add the API usage view**

Add this type before `accountView` in `examples/plugin/codex-account-pool/go/management.go`:

```go
type accountUsageView struct {
	Requests        int64     `json:"requests"`
	FailedRequests  int64     `json:"failed_requests"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CacheTokens     int64     `json:"cache_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}
```

Add this optional field to `accountView`:

```go
	Usage             *accountUsageView `json:"usage,omitempty"`
```

At the start of `accountViews`, snapshot usage once:

```go
	usage := p.usage.snapshot()
```

Inside the account loop, build the view before appending:

```go
		var usageView *accountUsageView
		if accountUsage, exists := usage.Accounts[account.ID]; exists {
			value := accountUsageView{
				Requests:        accountUsage.Requests,
				FailedRequests:  accountUsage.FailedRequests,
				InputTokens:     accountUsage.InputTokens,
				OutputTokens:    accountUsage.OutputTokens,
				ReasoningTokens: accountUsage.ReasoningTokens,
				CacheTokens: saturatingAdd(
					accountUsage.CacheReadTokens,
					accountUsage.CacheCreationTokens,
				),
				TotalTokens: accountUsage.TotalTokens,
				UpdatedAt:   accountUsage.UpdatedAt,
			}
			usageView = &value
		}
```

Set the field in the appended `accountView`:

```go
				Usage:   usageView,
				Refresh: refresh,
				Policy:  accountPolicy,
```

- [ ] **Step 4: Add usage persistence health to status**

At the start of `statusView`, read tracker health:

```go
	usageDegraded, usageError := p.usage.health()
```

Add these response fields:

```go
			"usage_degraded":    usageDegraded,
			"usage_error":       usageError,
```

- [ ] **Step 5: Format and verify management behavior**

Run:

```bash
cd examples/plugin/codex-account-pool/go
gofmt -w management.go management_test.go
go test -run 'TestManagementAccountsIncludeUsage|TestManagementStatusReportsSanitizedUsage|TestManagementAccountsResponseContainsNoCredentialJSON' ./...
go test ./...
```

Expected: PASS, including the existing credential-response safety test.

- [ ] **Step 6: Commit management serialization**

```bash
git add examples/plugin/codex-account-pool/go/management.go \
  examples/plugin/codex-account-pool/go/management_test.go
git commit -m "feat: expose codex account usage in management"
```

### Task 5: Account Table Display and Visibility-Aware Polling

**Files:**
- Modify: `examples/plugin/codex-account-pool/go/web/index.html`
- Modify: `examples/plugin/codex-account-pool/go/management_test.go`

- [ ] **Step 1: Write failing embedded-resource assertions**

Replace `TestStaticResourceIncludesBulkReserveAndTemporaryControls` in `examples/plugin/codex-account-pool/go/management_test.go` with:

```go
func TestStaticResourceIncludesUsageDisplayAndPollingControls(t *testing.T) {
	text := string(accountPoolHTML)
	for _, required := range []string{
		"batchFiveHourReserve",
		"batchWeeklyReserve",
		"clearTemporaryButton",
		"profileStatus",
		"customPlanOrder",
		"pollRefreshStatus",
		"five_hour_window_present",
		"weekly_window_present",
		`<th class="usage">Token 用量</th>`,
		"usageCell",
		"formatTokens",
		"代理累计",
		"usage.total_tokens",
		"usage.input_tokens",
		"usage.output_tokens",
		"usage.reasoning_tokens",
		"usage.cache_tokens",
		"usage.requests",
		"usage.failed_requests",
		"usage.updated_at",
		"document.hidden",
		`document.addEventListener("visibilitychange"`,
		"scheduleAccountPolling",
		"5000",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("static resource is missing %q", required)
		}
	}
	if count := strings.Count(text, "pending.clear()"); count != 1 {
		t.Fatalf("pending.clear() count = %d, want 1 only after a successful apply", count)
	}
}
```

- [ ] **Step 2: Run the static-resource test and confirm it fails**

Run:

```bash
cd examples/plugin/codex-account-pool/go
go test -run TestStaticResourceIncludesUsageDisplayAndPollingControls ./...
```

Expected: FAIL because the usage column and polling functions are absent.

- [ ] **Step 3: Add the Token Usage column styles and markup**

In `examples/plugin/codex-account-pool/go/web/index.html`, change the table styles to:

```css
    table{width:100%;min-width:1470px;border-collapse:collapse;table-layout:fixed}th,td{border-bottom:1px solid #e7ebef;padding:8px 7px;text-align:left;vertical-align:middle}
```

Replace the column-width rule with:

```css
    .check{width:38px;text-align:center}.account{width:245px}.plan{width:88px}.quota{width:110px}.usage{width:250px}.state{width:108px}.number{width:90px}.toggle{width:72px;text-align:center}.actions{width:90px}
```

Add usage cell styles after `.account-sub`:

```css
    .usage-total{font-weight:700;white-space:nowrap}.usage-detail{font-size:10px;color:var(--muted);line-height:1.4;margin-top:3px;white-space:normal}
```

Replace the table header and initial empty row with:

```html
        <thead><tr><th class="check"><input id="selectAll" type="checkbox" aria-label="全选"></th><th class="account">账号</th><th class="plan">套餐</th><th class="quota">5 小时</th><th class="quota">周额度</th><th class="usage">Token 用量</th><th class="state">状态</th><th class="number">优先级</th><th class="number">权重</th><th class="toggle">备用</th><th class="toggle">启用</th><th class="number">5h 保留</th><th class="number">周保留</th></tr></thead>
        <tbody id="accountRows"><tr><td colspan="13" class="empty">输入管理密钥后加载账号</td></tr></tbody>
```

- [ ] **Step 4: Add compact formatting and exact hover details**

Extend the script state with a polling timer:

```js
    const apiBase="/v0/management/codex-account-pool";let managementKey=sessionStorage.getItem("codexPoolKey")||"";let accounts=[];let pending=new Map();let selected=new Set();let currentProfile="";let accountPollTimer=0;
```

Add these functions after `shortTime`:

```js
    function formatTokens(value){const n=Math.max(0,Number(value)||0);for(const [divisor,suffix] of [[1e9,"B"],[1e6,"M"],[1e3,"K"]]){if(n>=divisor)return`${(n/divisor).toFixed(2).replace(/\.?0+$/,"")}${suffix}`}return String(Math.trunc(n))}
    function exactTokens(value){return new Intl.NumberFormat("zh-CN",{maximumFractionDigits:0}).format(Math.max(0,Number(value)||0))}
    function usageCell(usage){if(!usage)return'<span class="status">--</span>';const updated=shortTime(usage.updated_at)||"--",details=[`代理累计用量（仅本 CLIProxyAPI 实例）`,`总量：${exactTokens(usage.total_tokens)}`,`输入：${exactTokens(usage.input_tokens)}`,`输出：${exactTokens(usage.output_tokens)}`,`推理：${exactTokens(usage.reasoning_tokens)}`,`缓存：${exactTokens(usage.cache_tokens)}`,`请求：${exactTokens(usage.requests)}`,`失败：${exactTokens(usage.failed_requests)}`,`更新：${updated}`].join("\n");return`<div title="${escapeHTML(details)}" aria-label="${escapeHTML(details)}"><div class="usage-total">代理累计 ${formatTokens(usage.total_tokens)}</div><div class="usage-detail">输入 ${formatTokens(usage.input_tokens)} · 输出 ${formatTokens(usage.output_tokens)} · 推理 ${formatTokens(usage.reasoning_tokens)} · 缓存 ${formatTokens(usage.cache_tokens)}</div></div>`}
```

- [ ] **Step 5: Render usage without disturbing pending policy edits**

In `render`, change both empty-row colspans from `12` to `13`.

Insert this cell immediately after the weekly quota cell in each account row:

```js
<td class="usage">${usageCell(a.usage)}</td>
```

The complete row field order must be:

```js
return `<tr class="${changed?"changed":""}" data-id="${escapeHTML(a.id)}"><td class="check"><input type="checkbox" data-select="${escapeHTML(a.id)}" ${selected.has(a.id)?"checked":""}></td><td class="account"><div class="account-name">${escapeHTML(a.label||a.email||a.id)}</div><div class="account-sub">${escapeHTML(a.email||a.id)}</div></td><td><span class="pill ${a.plan}">${escapeHTML(a.plan_type||a.plan)}</span></td><td>${quotaCell(a.five_hour_remaining,a.five_hour_window_present)}</td><td>${quotaCell(a.weekly_remaining,a.weekly_window_present)}</td><td class="usage">${usageCell(a.usage)}</td><td><span class="pill ${statusClass}">${status}</span>${detail?`<div class="state-sub" title="${escapeHTML(detail)}">${escapeHTML(detail)}</div>`:""}</td><td><input class="number-input" type="number" data-field="priority" value="${p.priority}"></td><td><input class="number-input" type="number" min="1" data-field="weight" value="${p.weight}"></td><td class="toggle"><button class="switch ${p.backup?"on":""}" data-toggle="backup" aria-label="备用"><span></span></button></td><td class="toggle"><button class="switch ${p.enabled?"on":""}" data-toggle="enabled" aria-label="启用"><span></span></button></td><td><input class="number-input" type="number" min="0" max="100" data-field="five_hour_reserve" value="${p.five_hour_reserve}"></td><td><input class="number-input" type="number" min="0" max="100" data-field="weekly_reserve" value="${p.weekly_reserve}"></td></tr>`
```

Keep `policyFor(account)` unchanged so `pending` remains the authoritative overlay during every render.

- [ ] **Step 6: Add five-second account-only polling that pauses while hidden**

Add these functions after `load`:

```js
    function setConnectionStatus(message,className){$("connectionStatus").textContent=message;$("connectionStatus").className=`status${className?` ${className}`:""}`}
    async function refreshAccounts(){if(document.hidden||!managementKey)return false;try{const accountData=await api("/accounts");accounts=accountData.accounts||[];setConnectionStatus("已连接","success");render();return true}catch(error){setConnectionStatus(error.message,"error");return false}}
    function scheduleAccountPolling(){clearTimeout(accountPollTimer);if(document.hidden||!managementKey)return;accountPollTimer=setTimeout(async()=>{await refreshAccounts();scheduleAccountPolling()},5000)}
```

At the successful end of `load`, call:

```js
scheduleAccountPolling()
```

Replace `pollRefreshStatus` with:

```js
    async function pollRefreshStatus(attempt=0){const loaded=await refreshAccounts();if(!loaded)return;const active=accounts.some(account=>["queued","refreshing"].includes(account.refresh?.state));if(active&&attempt<59)setTimeout(()=>pollRefreshStatus(attempt+1),2000)}
```

Add visibility handling before the initial `managementKey` load:

```js
    document.addEventListener("visibilitychange",()=>{clearTimeout(accountPollTimer);if(!document.hidden&&managementKey)refreshAccounts().finally(scheduleAccountPolling)});
```

Do not add any `pending.clear()` call to `refreshAccounts`, `scheduleAccountPolling`, `pollRefreshStatus`, `load`, or the visibility handler. The existing `applyPending` success path remains the only place that clears pending edits.

- [ ] **Step 7: Run static-resource and plugin tests**

Run:

```bash
cd examples/plugin/codex-account-pool/go
gofmt -w management_test.go
go test -run 'TestStaticResource|TestManagementAccountsIncludeUsage' ./...
go test ./...
```

Expected: PASS.

- [ ] **Step 8: Inspect the page at desktop and mobile widths**

Use the in-app browser against the running local management page:

```text
http://localhost:64432/v0/resource/plugins/codex-account-pool/pool
```

Verify at `1440x900` and `390x844`:

- the table has one Token Usage column between weekly quota and status;
- total and detail text remain inside the cell;
- exact values, request/failure counts, and update time appear in hover details;
- editing priority or weight, waiting more than five seconds, and returning to the tab preserves the changed-row state and input value;
- hiding the page stops account requests and showing it triggers an immediate refresh;
- a polling error leaves the previous rows visible and changes only connection status.

- [ ] **Step 9: Commit the management page**

```bash
git add examples/plugin/codex-account-pool/go/web/index.html \
  examples/plugin/codex-account-pool/go/management_test.go
git commit -m "feat: display codex account token usage"
```

### Task 6: Documentation and Full Verification

**Files:**
- Modify: `examples/plugin/codex-account-pool/README.md`

- [ ] **Step 1: Document the new persisted state file**

Replace the state-file paragraph in `examples/plugin/codex-account-pool/README.md` with:

```markdown
Use an absolute `state_dir` in production. `policy.json`, `quota.json`, and
`usage.json` are written with mode `0600`. They never contain OAuth tokens, raw
credential JSON, request bodies, or response bodies.
```

- [ ] **Step 2: Document usage semantics and the table display**

Add this section after the quota-window paragraph:

```markdown
## Token Usage

The account table displays cumulative token usage observed by this CLIProxyAPI
instance for each Codex `AuthID`. It includes input, output, reasoning,
cache-read, cache-creation, total token, request, and failed-request counters.
Reasoning tokens remain a subset of output tokens, and cache tokens remain a
subset of input tokens, so neither is added again to the displayed total.

Usage is aggregated in memory and saved atomically to `usage.json` at most once
per second. The final dirty snapshot is flushed during plugin shutdown and
before switching `state_dir`. These counters are local observations, not the
account's global OpenAI or ChatGPT subscription usage, and they never affect
priority, weight, eligibility, reserves, or route selection.

The browser refreshes account data every five seconds while visible. Polling
preserves unsaved policy edits and pauses when the page is hidden.
```

- [ ] **Step 3: Format all Go files**

Run from the repository root:

```bash
gofmt -w .
```

Expected: no formatting errors.

- [ ] **Step 4: Run the plugin test suite**

```bash
cd examples/plugin/codex-account-pool/go
go test ./...
```

Expected: PASS.

- [ ] **Step 5: Run the repository test suite**

```bash
cd /Users/hebi/Documents/GitHub/CLIProxyAPI
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Verify the server build**

```bash
cd /Users/hebi/Documents/GitHub/CLIProxyAPI
go build -o test-output ./cmd/server
rm test-output
```

Expected: build succeeds and `test-output` is removed.

- [ ] **Step 7: Verify the C shared-library plugin build and remove generated artifacts**

```bash
cd /Users/hebi/Documents/GitHub/CLIProxyAPI/examples/plugin/codex-account-pool/go
go build -buildmode=c-shared -o test-codex-account-pool.so .
rm -f test-codex-account-pool.so test-codex-account-pool.h
```

Expected: build succeeds and neither generated file remains.

- [ ] **Step 8: Confirm no generated artifacts or unrelated files are staged**

Run:

```bash
cd /Users/hebi/Documents/GitHub/CLIProxyAPI
git status --short
git diff --check
```

Expected:

- no `test-output`, `test-codex-account-pool.so`, or `test-codex-account-pool.h`;
- `.superpowers/`, `batch_register.py`,
  `openspec/changes/add-codex-account-pool-plugin/implementation-plan.md`, and
  `proxy_fetcher.py` remain untracked and unstaged;
- any pre-existing unrelated source edits remain untouched and unstaged;
- `git diff --check` reports no whitespace errors.

- [ ] **Step 9: Commit documentation and final verification changes**

```bash
git add examples/plugin/codex-account-pool/README.md
git commit -m "docs: explain codex account token usage"
```
