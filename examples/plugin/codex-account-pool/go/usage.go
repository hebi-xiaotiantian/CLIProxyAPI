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
	mu              sync.Mutex
	persistenceMu   sync.Mutex
	shutdownMu      sync.Mutex
	doc             UsageDocument
	store           *stateStore
	dirty           bool
	revision        uint64
	degraded        bool
	lastError       string
	loadBlocked     bool
	stopped         bool
	shutdownStarted bool
	shutdownErr     error
	now             func() time.Time
	flushInterval   time.Duration
	wake            chan struct{}
	stop            chan struct{}
	done            chan struct{}
	save            func(*stateStore, UsageDocument) error
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
		now:           now,
		flushInterval: flushInterval,
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		save: func(store *stateStore, doc UsageDocument) error {
			return store.saveUsage(doc)
		},
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
	cachedTokens := nonNegativeCounter(record.Detail.CachedTokens)
	if cachedTokens > cacheReadTokens {
		cacheReadTokens = cachedTokens
	}
	cacheCreationTokens := nonNegativeCounter(record.Detail.CacheCreationTokens)
	totalTokens := nonNegativeCounter(record.Detail.TotalTokens)
	if totalTokens == 0 {
		totalTokens = saturatingAdd(inputTokens, outputTokens)
	}

	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	if t.doc.Accounts == nil {
		t.doc.Accounts = make(map[string]AccountUsage)
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
	account.UpdatedAt = t.now().UTC()
	t.doc.Accounts[authID] = account
	t.revision++
	t.dirty = true
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

func (t *usageTracker) configureStore(store *stateStore) error {
	if t == nil {
		return fmt.Errorf("usage tracker is required")
	}
	if store == nil {
		return fmt.Errorf("usage state store is required")
	}

	t.persistenceMu.Lock()
	defer t.persistenceMu.Unlock()

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stopped {
		return fmt.Errorf("usage tracker is shut down")
	}
	sameStore := t.store != nil && t.store.dir == store.dir
	if sameStore && !t.loadBlocked {
		return nil
	}
	if t.loadBlocked && t.dirty && !sameStore {
		message := t.lastError
		if message == "" {
			message = "usage state could not be loaded"
		}
		return fmt.Errorf("switch usage state directory while load is blocked: %s", message)
	}
	initialConfigure := t.store == nil
	retryingBlockedLoad := sameStore && t.loadBlocked
	preConfigDirty := (initialConfigure || retryingBlockedLoad) && t.dirty
	preConfigDoc := defaultUsageDocument()
	if preConfigDirty {
		preConfigDoc = cloneUsageDocument(t.doc)
	}
	if t.store != nil && t.dirty && !sameStore {
		if err := t.save(t.store, cloneUsageDocument(t.doc)); err != nil {
			message := sanitizeError(err)
			t.degraded = true
			t.lastError = message
			return fmt.Errorf("flush usage before switching state directory: %s", message)
		}
		t.dirty = false
	}

	doc, err := store.loadUsage()
	t.store = store
	t.revision++
	if err != nil {
		if !retryingBlockedLoad {
			t.doc = defaultUsageDocument()
			t.dirty = false
		}
		t.loadBlocked = true
		t.degraded = true
		t.lastError = sanitizeError(err)
		return nil
	}
	if preConfigDirty {
		t.doc = mergeUsageDocuments(doc, preConfigDoc)
		t.dirty = true
		t.notify()
	} else {
		t.doc = cloneUsageDocument(doc)
		t.dirty = false
	}
	t.loadBlocked = false
	t.degraded = false
	t.lastError = ""
	return nil
}

func (t *usageTracker) shutdown() error {
	if t == nil {
		return nil
	}

	t.shutdownMu.Lock()
	defer t.shutdownMu.Unlock()
	if t.shutdownStarted {
		return t.shutdownErr
	}
	t.shutdownStarted = true

	t.mu.Lock()
	t.stopped = true
	close(t.stop)
	t.mu.Unlock()
	<-t.done

	t.shutdownErr = t.flush()
	return t.shutdownErr
}

func (t *usageTracker) run() {
	defer close(t.done)
	for {
		select {
		case <-t.stop:
			return
		case <-t.wake:
		}

		timer := time.NewTimer(t.flushInterval)
		for {
			select {
			case <-t.stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-t.wake:
			case <-timer.C:
				_ = t.flush()
				if t.isDirty() {
					timer.Reset(t.flushInterval)
					continue
				}
				goto nextWake
			}
		}

	nextWake:
	}
}

func (t *usageTracker) notify() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *usageTracker) isDirty() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dirty
}

func (t *usageTracker) flush() error {
	t.persistenceMu.Lock()
	defer t.persistenceMu.Unlock()

	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return nil
	}
	if t.loadBlocked {
		message := t.lastError
		if message == "" {
			message = "usage state could not be loaded"
			t.lastError = message
		}
		t.degraded = true
		t.mu.Unlock()
		return fmt.Errorf("save usage blocked after load failure: %s", message)
	}
	if t.store == nil {
		message := sanitizeError(fmt.Errorf("usage state store is unavailable"))
		t.degraded = true
		t.lastError = message
		t.mu.Unlock()
		return fmt.Errorf("%s", message)
	}
	store := t.store
	doc := cloneUsageDocument(t.doc)
	revision := t.revision
	t.mu.Unlock()

	err := t.save(store, doc)

	t.mu.Lock()
	if err != nil {
		message := sanitizeError(err)
		t.degraded = true
		t.lastError = message
		t.mu.Unlock()
		return fmt.Errorf("save usage: %s", message)
	}
	if t.store == store && t.revision == revision {
		t.dirty = false
	}
	t.degraded = false
	t.lastError = ""
	dirty := t.dirty
	t.mu.Unlock()

	if dirty {
		t.notify()
	}
	return nil
}

func cloneUsageDocument(doc UsageDocument) UsageDocument {
	out := doc
	out.Accounts = make(map[string]AccountUsage, len(doc.Accounts))
	for authID, account := range doc.Accounts {
		out.Accounts[authID] = account
	}
	return out
}

func mergeUsageDocuments(base, delta UsageDocument) UsageDocument {
	out := cloneUsageDocument(base)
	if out.Version == 0 {
		out.Version = stateVersion
	}
	for authID, addition := range delta.Accounts {
		account := out.Accounts[authID]
		account.Requests = saturatingAdd(account.Requests, addition.Requests)
		account.FailedRequests = saturatingAdd(account.FailedRequests, addition.FailedRequests)
		account.InputTokens = saturatingAdd(account.InputTokens, addition.InputTokens)
		account.OutputTokens = saturatingAdd(account.OutputTokens, addition.OutputTokens)
		account.ReasoningTokens = saturatingAdd(account.ReasoningTokens, addition.ReasoningTokens)
		account.CacheReadTokens = saturatingAdd(account.CacheReadTokens, addition.CacheReadTokens)
		account.CacheCreationTokens = saturatingAdd(account.CacheCreationTokens, addition.CacheCreationTokens)
		account.TotalTokens = saturatingAdd(account.TotalTokens, addition.TotalTokens)
		if !addition.UpdatedAt.IsZero() && (account.UpdatedAt.IsZero() || addition.UpdatedAt.After(account.UpdatedAt)) {
			account.UpdatedAt = addition.UpdatedAt
		}
		out.Accounts[authID] = account
	}
	return out
}
