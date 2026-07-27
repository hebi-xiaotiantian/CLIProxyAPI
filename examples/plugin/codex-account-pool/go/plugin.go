package main

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
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type accountPoolPlugin struct {
	host           hostCaller
	selector       *selector
	lifecycleMu    sync.Mutex
	configMu       sync.RWMutex
	config         Config
	store          *stateStore
	policyMu       sync.Mutex
	policy         atomic.Pointer[PolicyDocument]
	quota          atomic.Pointer[QuotaDocument]
	quotaMu        sync.Mutex
	inventoryMu    sync.RWMutex
	inventory      []pluginapi.HostAuthFileEntry
	statusMu       sync.RWMutex
	statusError    string
	policyDegraded bool
	delayedMu      sync.Mutex
	delayedRefresh map[string]*time.Timer
	coordinator    *refreshCoordinator
}

func newAccountPoolPlugin(host hostCaller) *accountPoolPlugin {
	plugin := &accountPoolPlugin{
		host:           host,
		selector:       newSelector(time.Now),
		config:         defaultConfig(),
		delayedRefresh: make(map[string]*time.Timer),
	}
	policy := defaultPolicyDocument()
	quota := defaultQuotaDocument()
	plugin.policy.Store(&policy)
	plugin.quota.Store(&quota)
	return plugin
}

func (p *accountPoolPlugin) configure(raw []byte) error {
	var request lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	cfg, errConfig := decodeConfig(request.ConfigYAML)
	if errConfig != nil {
		return errConfig
	}

	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.stopDelayedRefreshes()
	p.configMu.Lock()
	oldCoordinator := p.coordinator
	p.coordinator = nil
	p.configMu.Unlock()
	if oldCoordinator != nil {
		oldCoordinator.stop()
	}

	p.policyMu.Lock()
	p.quotaMu.Lock()
	store := newStateStore(cfg.StateDir)
	policyExists := store.policyExists()
	policy, errPolicy := store.loadPolicy()
	if errPolicy != nil {
		policy = defaultPolicyDocument()
	} else if !policyExists {
		policy.CustomPlanOrder = append([]PlanKind(nil), cfg.CustomPlanOrder...)
		policy, errPolicy = normalizePolicyDocument(policy)
	}
	quota, errQuota := store.loadQuota()
	if errQuota != nil {
		quota = defaultQuotaDocument()
	}
	client := quotaClient{host: p.host, endpoint: cfg.QuotaURL, now: time.Now}
	coordinator := newRefreshCoordinator(cfg.RefreshConcurrency, func(_ context.Context, account pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		return client.refresh(account)
	}, p.applyRefreshResult)
	coordinator.setStateCallback(p.applyRefreshState)

	p.configMu.Lock()
	p.config = cfg
	p.store = store
	p.policy.Store(&policy)
	p.quota.Store(&quota)
	p.coordinator = coordinator
	p.configMu.Unlock()
	p.quotaMu.Unlock()
	p.policyMu.Unlock()

	statusMessages := make([]string, 0, 2)
	if errPolicy != nil {
		statusMessages = append(statusMessages, errPolicy.Error())
	}
	if errQuota != nil {
		statusMessages = append(statusMessages, errQuota.Error())
	}
	p.statusMu.Lock()
	p.policyDegraded = errPolicy != nil
	p.statusError = sanitizeText(strings.Join(statusMessages, "; "))
	p.statusMu.Unlock()

	if errSync := p.syncInventory(); errSync != nil {
		p.setStatusError(errSync.Error())
	}
	coordinator.startSchedule(cfg.RefreshInterval, p.scheduledAccounts)
	return nil
}

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
}

func (p *accountPoolPlugin) pick(raw []byte) ([]byte, error) {
	var request pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return nil, fmt.Errorf("decode scheduler request: %w", errUnmarshal)
	}
	p.configMu.RLock()
	cfg := p.config
	p.configMu.RUnlock()
	response, errPick := p.selector.pick(request, *p.policy.Load(), *p.quota.Load(), cfg)
	if errPick != nil {
		return errorEnvelope("codex_account_pool_unavailable", errPick.Error(), true, 503), nil
	}
	return okEnvelope(response)
}

func (p *accountPoolPlugin) handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return nil, fmt.Errorf("decode usage record: %w", errUnmarshal)
	}
	if strings.EqualFold(record.Provider, "codex") && record.Failed && record.Failure.StatusCode == 429 {
		p.applyRefreshResult(QuotaSnapshot{AuthID: record.AuthID}, fmt.Errorf("codex request returned HTTP 429"))
		p.queueRefreshByID(record.AuthID, 30*time.Second)
	}
	return okEnvelope(map[string]any{})
}

func (p *accountPoolPlugin) syncInventory() error {
	if p.host == nil {
		return fmt.Errorf("host callback is unavailable")
	}
	raw, errCall := p.host.Call(pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return fmt.Errorf("list host accounts: %w", errCall)
	}
	var response authListResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return fmt.Errorf("decode host account list: %w", errUnmarshal)
	}
	accounts := make([]pluginapi.HostAuthFileEntry, 0, len(response.Files))
	for _, entry := range response.Files {
		if strings.EqualFold(entry.Provider, "codex") || strings.EqualFold(entry.Type, "codex") {
			accounts = append(accounts, entry)
		}
	}
	p.inventoryMu.Lock()
	p.inventory = accounts
	p.inventoryMu.Unlock()
	return p.ensureDefaultPolicies(accounts)
}

func (p *accountPoolPlugin) ensureDefaultPolicies(accounts []pluginapi.HostAuthFileEntry) error {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	current := p.policy.Load()
	next := clonePolicyDocument(*current)
	changed := false
	quota := p.quota.Load()
	p.configMu.RLock()
	cfg := p.config
	store := p.store
	p.configMu.RUnlock()
	for _, account := range accounts {
		if _, exists := next.Accounts[account.ID]; exists {
			continue
		}
		item := defaultAccountPolicy(account.Priority)
		plan := PlanUnknown
		if snapshot, ok := quota.Accounts[account.ID]; ok {
			plan = snapshot.Plan
		}
		reserve := cfg.UnknownReserveDefault
		if plan == PlanFree {
			reserve = cfg.FreeReserveDefault
		} else if plan == PlanPaid {
			reserve = cfg.PaidReserveDefault
		}
		item.FiveHourReserve = reserve
		item.WeeklyReserve = reserve
		item.ReserveDefaultsForPlan = plan
		next.Accounts[account.ID] = item
		changed = true
	}
	if !changed {
		return nil
	}
	next.Revision++
	normalized, errNormalize := normalizePolicyDocument(next)
	if errNormalize != nil {
		return errNormalize
	}
	p.statusMu.RLock()
	degraded := p.policyDegraded
	p.statusMu.RUnlock()
	if degraded {
		p.policy.Store(&normalized)
		return nil
	}
	if errSave := store.savePolicy(normalized); errSave != nil {
		return errSave
	}
	p.policy.Store(&normalized)
	return nil
}

func (p *accountPoolPlugin) applyRefreshResult(snapshot QuotaSnapshot, errRefresh error) {
	p.quotaMu.Lock()
	current := p.quota.Load()
	next := cloneQuotaDocument(*current)
	authID := snapshot.AuthID
	if errRefresh != nil {
		if authID == "" {
			p.quotaMu.Unlock()
			return
		}
		previous := next.Accounts[authID]
		previous.AuthID = authID
		previous.Refresh = RefreshStatus{
			State:     "failed",
			LastError: sanitizeError(errRefresh),
			FailedAt:  time.Now().UTC(),
		}
		next.Accounts[authID] = previous
	} else {
		next.Accounts[snapshot.AuthID] = snapshot
	}
	p.configMu.RLock()
	store := p.store
	p.configMu.RUnlock()
	if store == nil {
		p.quotaMu.Unlock()
		return
	}
	if errSave := store.saveQuota(next); errSave != nil {
		p.quotaMu.Unlock()
		p.setStatusError(errSave.Error())
		return
	}
	p.quota.Store(&next)
	p.quotaMu.Unlock()
	if errRefresh == nil {
		p.applyDetectedPlanDefaults(snapshot.AuthID, snapshot.Plan)
	}
}

func (p *accountPoolPlugin) applyRefreshState(authID, state string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	p.quotaMu.Lock()
	current := p.quota.Load()
	next := cloneQuotaDocument(*current)
	snapshot := next.Accounts[authID]
	snapshot.AuthID = authID
	snapshot.Refresh.State = state
	next.Accounts[authID] = snapshot
	p.quota.Store(&next)
	p.quotaMu.Unlock()
}

func (p *accountPoolPlugin) inventorySnapshot() []pluginapi.HostAuthFileEntry {
	p.inventoryMu.RLock()
	defer p.inventoryMu.RUnlock()
	return append([]pluginapi.HostAuthFileEntry(nil), p.inventory...)
}

func (p *accountPoolPlugin) scheduledAccounts() []pluginapi.HostAuthFileEntry {
	if errSync := p.syncInventory(); errSync != nil {
		p.setStatusError(errSync.Error())
	}
	return p.inventorySnapshot()
}

func (p *accountPoolPlugin) applyDetectedPlanDefaults(authID string, plan PlanKind) {
	authID = strings.TrimSpace(authID)
	if authID == "" || !validPlan(plan) {
		return
	}
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	current := p.policy.Load()
	accountPolicy, ok := current.Accounts[authID]
	if !ok || accountPolicy.ReserveDefaultsForPlan == "" || accountPolicy.ReserveDefaultsForPlan == plan {
		return
	}
	p.configMu.RLock()
	cfg := p.config
	p.configMu.RUnlock()
	reserve := cfg.UnknownReserveDefault
	if plan == PlanFree {
		reserve = cfg.FreeReserveDefault
	} else if plan == PlanPaid {
		reserve = cfg.PaidReserveDefault
	}
	next := clonePolicyDocument(*current)
	accountPolicy.FiveHourReserve = reserve
	accountPolicy.WeeklyReserve = reserve
	accountPolicy.ReserveDefaultsForPlan = plan
	next.Accounts[authID] = accountPolicy
	if errPersist := p.persistPolicy(next); errPersist != nil {
		p.setStatusError(errPersist.Error())
	}
}

func (p *accountPoolPlugin) queueRefreshByID(authID string, delay time.Duration) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	queue := func() bool {
		for _, account := range p.inventorySnapshot() {
			if account.ID == authID {
				p.configMu.RLock()
				coordinator := p.coordinator
				p.configMu.RUnlock()
				return coordinator != nil && coordinator.queue(account)
			}
		}
		return false
	}
	if delay <= 0 {
		p.delayedMu.Lock()
		if timer := p.delayedRefresh[authID]; timer != nil {
			timer.Stop()
			delete(p.delayedRefresh, authID)
		}
		p.delayedMu.Unlock()
		return queue()
	}
	p.delayedMu.Lock()
	if _, exists := p.delayedRefresh[authID]; exists {
		p.delayedMu.Unlock()
		return false
	}
	timer := time.AfterFunc(delay, func() {
		p.delayedMu.Lock()
		delete(p.delayedRefresh, authID)
		p.delayedMu.Unlock()
		_ = queue()
	})
	p.delayedRefresh[authID] = timer
	p.delayedMu.Unlock()
	return true
}

func (p *accountPoolPlugin) stopDelayedRefreshes() {
	p.delayedMu.Lock()
	for authID, timer := range p.delayedRefresh {
		timer.Stop()
		delete(p.delayedRefresh, authID)
	}
	p.delayedMu.Unlock()
}

func (p *accountPoolPlugin) setStatusError(message string) {
	p.statusMu.Lock()
	p.statusError = sanitizeText(message)
	p.statusMu.Unlock()
}

func (p *accountPoolPlugin) getStatusError() string {
	p.statusMu.RLock()
	defer p.statusMu.RUnlock()
	return p.statusError
}

func clonePolicyDocument(doc PolicyDocument) PolicyDocument {
	out := doc
	out.CustomPlanOrder = append([]PlanKind(nil), doc.CustomPlanOrder...)
	if doc.TemporaryOverride != nil {
		copyOverride := *doc.TemporaryOverride
		out.TemporaryOverride = &copyOverride
	}
	out.Accounts = make(map[string]AccountPolicy, len(doc.Accounts))
	for key, value := range doc.Accounts {
		value.Tags = append([]string(nil), value.Tags...)
		out.Accounts[key] = value
	}
	return out
}

func cloneQuotaDocument(doc QuotaDocument) QuotaDocument {
	out := doc
	out.Accounts = make(map[string]QuotaSnapshot, len(doc.Accounts))
	for key, value := range doc.Accounts {
		out.Accounts[key] = value
	}
	return out
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return sanitizeText(err.Error())
}

var sensitiveErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)((?:access|refresh|id)[_-]?token["']?\s*[:=]\s*["']?)[^"\s,;]+`),
	regexp.MustCompile(`(?i)(bearer\s+)[^\s,;]+`),
}

func sanitizeText(value string) string {
	text := strings.TrimSpace(value)
	for _, pattern := range sensitiveErrorPatterns {
		text = pattern.ReplaceAllString(text, "${1}[redacted]")
	}
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}
