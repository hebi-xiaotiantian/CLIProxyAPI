package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDecodeConfigAppliesDefaults(t *testing.T) {
	cfg, err := decodeConfig([]byte("state_dir: /tmp/pool\nrefresh_interval: 5m\nstale_policy: allow\n"))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	if cfg.StateDir != "/tmp/pool" || cfg.RefreshInterval != 5*time.Minute || cfg.StalePolicy != StaleAllow {
		t.Fatalf("config = %#v", cfg)
	}
}

func TestDecodeConfigRejectsInvalidDuration(t *testing.T) {
	if _, err := decodeConfig([]byte("refresh_interval: soon\n")); err == nil {
		t.Fatal("decodeConfig() error = nil, want invalid duration")
	}
}

func TestDecodeConfigAcceptsCustomPlanOrder(t *testing.T) {
	cfg, err := decodeConfig([]byte("custom_plan_order: [free, unknown, paid]\n"))
	if err != nil {
		t.Fatalf("decodeConfig() error = %v", err)
	}
	want := []PlanKind{PlanFree, PlanUnknown, PlanPaid}
	if len(cfg.CustomPlanOrder) != len(want) {
		t.Fatalf("custom plan order = %#v", cfg.CustomPlanOrder)
	}
	for index := range want {
		if cfg.CustomPlanOrder[index] != want[index] {
			t.Fatalf("custom plan order = %#v, want %#v", cfg.CustomPlanOrder, want)
		}
	}
}

func TestPluginRegistrationDeclaresRequiredCapabilities(t *testing.T) {
	registration := pluginRegistration()
	if !registration.Capabilities.Scheduler || !registration.Capabilities.UsagePlugin || !registration.Capabilities.ManagementAPI {
		t.Fatalf("capabilities = %#v", registration.Capabilities)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion || registration.Metadata.Name != pluginID {
		t.Fatalf("registration = %#v", registration)
	}
	if registration.Metadata.Version != "0.2.0" {
		t.Fatalf("registration version = %q, want 0.2.0", registration.Metadata.Version)
	}
}

func TestSafeCallConvertsPanicToErrorEnvelope(t *testing.T) {
	raw := safeCall(func() ([]byte, error) {
		panic("boom")
	})
	var envelope envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.OK || envelope.Error == nil || !strings.Contains(envelope.Error.Message, "panic") {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestManagementRegistrationUsesExactRoutes(t *testing.T) {
	got := managementRegistration()
	if len(got.Resources) != 1 || got.Resources[0].Path != "/pool" {
		t.Fatalf("resources = %#v", got.Resources)
	}
	want := map[string]bool{
		"GET /codex-account-pool/status":     false,
		"GET /codex-account-pool/accounts":   false,
		"PATCH /codex-account-pool/accounts": false,
		"GET /codex-account-pool/profile":    false,
		"PUT /codex-account-pool/profile":    false,
		"POST /codex-account-pool/refresh":   false,
		"POST /codex-account-pool/preview":   false,
		"GET /codex-account-pool/decisions":  false,
	}
	for _, route := range got.Routes {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Fatalf("missing management route %s", route)
		}
	}
}

func TestPluginPickRecordsRealSchedulingDecision(t *testing.T) {
	plugin := newTestPlugin(t)
	now := time.Now()
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfileQuotaLowFirst,
		Accounts: map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 100, Weight: 1},
			"b": {Enabled: true, Priority: 100, Weight: 1},
		},
	})
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"a": {
				AuthID: "a", Plan: PlanPaid, PlanType: "plus",
				FiveHourRemaining: 20, WeeklyRemaining: 20,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
			"b": {
				AuthID: "b", Plan: PlanPaid, PlanType: "plus",
				FiveHourRemaining: 40, WeeklyRemaining: 40,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
		},
	})
	request := codexRequest("a", "b")
	request.Model = "gpt-5.4"
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal scheduler request: %v", errMarshal)
	}

	if _, errPick := plugin.pick(raw); errPick != nil {
		t.Fatalf("plugin pick() error = %v", errPick)
	}
	got := plugin.decisions.snapshot()
	if len(got) != 1 {
		t.Fatalf("decisions = %#v, want one", got)
	}
	decision := got[0]
	if decision.AuthID != "a" || decision.Model != "gpt-5.4" ||
		decision.Profile != ProfileQuotaLowFirst || decision.Layer != "regular" ||
		decision.CandidateCount != 2 || decision.EligibleCount != 2 || decision.GroupSize != 1 ||
		decision.Timestamp.IsZero() {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestPluginPickBoundsDecisionModelLength(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 100, Weight: 1},
		},
	})
	request := codexRequest("a")
	request.Model = strings.Repeat("x", 500)
	raw, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		t.Fatalf("marshal scheduler request: %v", errMarshal)
	}

	if _, errPick := plugin.pick(raw); errPick != nil {
		t.Fatalf("plugin pick() error = %v", errPick)
	}
	got := plugin.decisions.snapshot()
	if len(got) != 1 || len(got[0].Model) != 300 {
		t.Fatalf("decision model length = %d, want 300", len(got[0].Model))
	}
}

func TestConfigureClearsDecisionHistory(t *testing.T) {
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{}),
	}}
	plugin := newAccountPoolPlugin(host)
	oldSelector := plugin.selector
	plugin.selector.affinity["old-session"] = affinityEntry{
		AuthID:    "old-auth",
		Revision:  1,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	plugin.decisions.add(schedulingDecision{
		Timestamp: time.Now(),
		AuthID:    "old-auth",
		Profile:   ProfilePaidFirst,
		Layer:     "regular",
	})
	request, errMarshal := json.Marshal(lifecycleRequest{
		ConfigYAML: []byte("state_dir: " + t.TempDir() + "\nrefresh_interval: 1h\n"),
	})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}

	if errConfigure := plugin.configure(request); errConfigure != nil {
		t.Fatalf("configure() error = %v", errConfigure)
	}
	defer plugin.shutdown()
	if got := plugin.decisions.snapshot(); len(got) != 0 {
		t.Fatalf("decisions after configure = %#v, want empty", got)
	}
	if plugin.selector == oldSelector || len(plugin.selector.affinity) != 0 {
		t.Fatalf("selector runtime state survived configure: %#v", plugin.selector.affinity)
	}
}

func TestUsage429RecordsFailureBeforeDelayedRefresh(t *testing.T) {
	plugin := newTestPlugin(t)
	defer plugin.shutdown()
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
	})
	if err != nil {
		t.Fatalf("marshal usage record: %v", err)
	}
	if _, errHandle := plugin.handleUsage(record); errHandle != nil {
		t.Fatalf("handleUsage() error = %v", errHandle)
	}
	got := plugin.quota.Load().Accounts["auth-a"]
	if got.FiveHourRemaining != 40 || got.Refresh.State != "failed" {
		t.Fatalf("quota after 429 = %#v", got)
	}
}

func TestReconfigureWaitsForOldRefreshBeforeSwitchingState(t *testing.T) {
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{}),
	}}
	plugin := newAccountPoolPlugin(host)
	oldDir := t.TempDir()
	plugin.config = defaultConfig()
	plugin.config.StateDir = oldDir
	plugin.store = newStateStore(oldDir)
	started := make(chan struct{})
	release := make(chan struct{})
	plugin.coordinator = newRefreshCoordinator(1, func(context.Context, pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
		close(started)
		<-release
		return QuotaSnapshot{
			AuthID:            "auth-a",
			Plan:              PlanFree,
			FiveHourRemaining: 80,
			WeeklyRemaining:   70,
			RefreshedAt:       time.Now(),
		}, nil
	}, plugin.applyRefreshResult)
	if !plugin.coordinator.queue(pluginapi.HostAuthFileEntry{ID: "auth-a", Provider: "codex"}) {
		t.Fatal("old refresh queue = false")
	}
	<-started

	newDir := t.TempDir()
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("state_dir: " + newDir + "\nrefresh_interval: 1h\n")})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	configured := make(chan error, 1)
	go func() {
		configured <- plugin.configure(request)
	}()
	time.Sleep(20 * time.Millisecond)
	if got := plugin.store.dir; got != oldDir {
		t.Fatalf("state directory switched before old refresh stopped: %q", got)
	}
	close(release)
	if errConfigure := <-configured; errConfigure != nil {
		t.Fatalf("configure() error = %v", errConfigure)
	}
	defer plugin.shutdown()
	if _, errStat := os.Stat(filepath.Join(oldDir, quotaFileName)); errStat != nil {
		t.Fatalf("old quota file: %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(newDir, quotaFileName)); !os.IsNotExist(errStat) {
		t.Fatalf("new quota file error = %v, want not exist", errStat)
	}
}

func TestConfigureReadsStateUnderMutationLocks(t *testing.T) {
	dir := t.TempDir()
	store := newStateStore(dir)
	initialPolicy := defaultPolicyDocument()
	initialPolicy.Accounts["auth-a"] = defaultAccountPolicy(1)
	if errSave := store.savePolicy(initialPolicy); errSave != nil {
		t.Fatalf("save initial policy: %v", errSave)
	}
	initialQuota := defaultQuotaDocument()
	initialQuota.Accounts["auth-a"] = QuotaSnapshot{
		AuthID:            "auth-a",
		FiveHourRemaining: 10,
		RefreshedAt:       time.Now(),
	}
	if errSave := store.saveQuota(initialQuota); errSave != nil {
		t.Fatalf("save initial quota: %v", errSave)
	}

	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{}),
	}}
	plugin := newAccountPoolPlugin(host)
	request, errMarshal := json.Marshal(lifecycleRequest{ConfigYAML: []byte("state_dir: " + dir + "\nrefresh_interval: 1h\n")})
	if errMarshal != nil {
		t.Fatalf("marshal lifecycle request: %v", errMarshal)
	}

	plugin.policyMu.Lock()
	plugin.quotaMu.Lock()
	configured := make(chan error, 1)
	go func() {
		configured <- plugin.configure(request)
	}()
	select {
	case errConfigure := <-configured:
		plugin.quotaMu.Unlock()
		plugin.policyMu.Unlock()
		t.Fatalf("configure() returned before mutation locks were released: %v", errConfigure)
	case <-time.After(20 * time.Millisecond):
	}

	updatedPolicy := clonePolicyDocument(initialPolicy)
	updatedAccount := updatedPolicy.Accounts["auth-a"]
	updatedAccount.Priority = 9
	updatedPolicy.Accounts["auth-a"] = updatedAccount
	updatedPolicy.Revision++
	if errSave := store.savePolicy(updatedPolicy); errSave != nil {
		plugin.quotaMu.Unlock()
		plugin.policyMu.Unlock()
		t.Fatalf("save updated policy: %v", errSave)
	}
	updatedQuota := cloneQuotaDocument(initialQuota)
	updatedSnapshot := updatedQuota.Accounts["auth-a"]
	updatedSnapshot.FiveHourRemaining = 90
	updatedQuota.Accounts["auth-a"] = updatedSnapshot
	if errSave := store.saveQuota(updatedQuota); errSave != nil {
		plugin.quotaMu.Unlock()
		plugin.policyMu.Unlock()
		t.Fatalf("save updated quota: %v", errSave)
	}
	plugin.quotaMu.Unlock()
	plugin.policyMu.Unlock()

	if errConfigure := <-configured; errConfigure != nil {
		t.Fatalf("configure() error = %v", errConfigure)
	}
	defer plugin.shutdown()
	if got := plugin.policy.Load().Accounts["auth-a"].Priority; got != 9 {
		t.Fatalf("configured priority = %d, want 9", got)
	}
	if got := plugin.quota.Load().Accounts["auth-a"].FiveHourRemaining; got != 90 {
		t.Fatalf("configured five-hour quota = %v, want 90", got)
	}
}

func TestSuccessfulReconfigureClearsDegradedStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, policyFileName)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write corrupt policy: %v", err)
	}
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{}),
	}}
	plugin := newAccountPoolPlugin(host)
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("state_dir: " + dir + "\nrefresh_interval: 1h\n")})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	if errConfigure := plugin.configure(request); errConfigure != nil {
		t.Fatalf("first configure() error = %v", errConfigure)
	}
	if !plugin.isPolicyDegraded() || plugin.getStatusError() == "" {
		t.Fatal("first configure did not report degraded policy")
	}
	if errWrite := os.WriteFile(path, mustJSON(t, defaultPolicyDocument()), 0o600); errWrite != nil {
		t.Fatalf("write valid policy: %v", errWrite)
	}
	if errConfigure := plugin.configure(request); errConfigure != nil {
		t.Fatalf("second configure() error = %v", errConfigure)
	}
	defer plugin.shutdown()
	if plugin.isPolicyDegraded() || plugin.getStatusError() != "" {
		t.Fatalf("status after recovery = degraded:%v error:%q", plugin.isPolicyDegraded(), plugin.getStatusError())
	}
}
