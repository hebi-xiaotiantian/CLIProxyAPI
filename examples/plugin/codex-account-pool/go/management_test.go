package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestApplyAccountPatchIsAtomic(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 10, Weight: 1},
			"b": {Enabled: true, Priority: 20, Weight: 1},
		},
	})
	invalidWeight := 0
	if err := plugin.applyAccountPatch([]string{"a", "b"}, AccountPolicyPatch{Weight: &invalidWeight}); err == nil {
		t.Fatal("applyAccountPatch() error = nil, want invalid weight")
	}
	if plugin.policy.Load().Accounts["a"].Weight != 1 || plugin.policy.Load().Accounts["b"].Weight != 1 {
		t.Fatalf("invalid patch changed policy: %#v", plugin.policy.Load().Accounts)
	}

	priority := 100
	weight := 3
	if err := plugin.applyAccountPatch([]string{"a", "b"}, AccountPolicyPatch{Priority: &priority, Weight: &weight}); err != nil {
		t.Fatalf("applyAccountPatch() error = %v", err)
	}
	if plugin.policy.Load().Accounts["a"].Priority != 100 || plugin.policy.Load().Accounts["b"].Weight != 3 {
		t.Fatalf("valid patch was not applied: %#v", plugin.policy.Load().Accounts)
	}
}

func TestApplyAccountPatchKeepsMemoryWhenPersistenceFails(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 10, Weight: 1},
		},
	})
	blockedPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedPath, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("write blocked state path: %v", err)
	}
	plugin.store = newStateStore(blockedPath)
	priority := 100

	if err := plugin.applyAccountPatch([]string{"a"}, AccountPolicyPatch{Priority: &priority}); err == nil {
		t.Fatal("applyAccountPatch() error = nil, want persistence failure")
	}
	if got := plugin.policy.Load().Accounts["a"].Priority; got != 10 {
		t.Fatalf("in-memory priority = %d, want 10", got)
	}
}

func TestConcurrentAccountPatchesDoNotLoseUpdates(t *testing.T) {
	plugin := newTestPlugin(t)
	accounts := make(map[string]AccountPolicy)
	for index := range 24 {
		accounts[fmt.Sprintf("auth-%02d", index)] = AccountPolicy{Enabled: true, Priority: 1, Weight: 1}
	}
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts:      accounts,
	})
	start := make(chan struct{})
	errs := make(chan error, len(accounts))
	var workers sync.WaitGroup
	for index := range 24 {
		index := index
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			priority := 100 + index
			errs <- plugin.applyAccountPatch(
				[]string{fmt.Sprintf("auth-%02d", index)},
				AccountPolicyPatch{Priority: &priority},
			)
		}()
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent patch error = %v", err)
		}
	}
	for index := range 24 {
		id := fmt.Sprintf("auth-%02d", index)
		if got := plugin.policy.Load().Accounts[id].Priority; got != 100+index {
			t.Fatalf("account %s priority = %d, want %d", id, got, 100+index)
		}
	}
}

func TestSetTemporaryProfile(t *testing.T) {
	plugin := newTestPlugin(t)
	expiresAt := time.Now().Add(time.Hour).UTC()
	if err := plugin.setProfile(ProfileFreeFirst, &expiresAt); err != nil {
		t.Fatalf("setProfile() error = %v", err)
	}
	policy := plugin.policy.Load()
	if policy.TemporaryOverride == nil || policy.TemporaryOverride.Profile != ProfileFreeFirst {
		t.Fatalf("temporary override = %#v", policy.TemporaryOverride)
	}
}

func TestProfileViewHidesExpiredTemporaryOverride(t *testing.T) {
	plugin := newTestPlugin(t)
	expiredAt := time.Now().Add(-time.Minute).UTC()
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      2,
		ActiveProfile: ProfilePaidFirst,
		TemporaryOverride: &ProfileOverride{
			Profile:   ProfileFreeFirst,
			ExpiresAt: expiredAt,
		},
		CustomPlanOrder: []PlanKind{PlanPaid, PlanFree, PlanUnknown},
		Accounts:        map[string]AccountPolicy{},
	})

	view := plugin.profileView()
	if view["effective"] != ProfilePaidFirst {
		t.Fatalf("effective profile = %v, want %q", view["effective"], ProfilePaidFirst)
	}
	if view["temporary"] != nil {
		t.Fatalf("temporary profile = %#v, want nil", view["temporary"])
	}
}

func TestSetCustomProfilePersistsPlanOrder(t *testing.T) {
	plugin := newTestPlugin(t)
	order := []PlanKind{PlanFree, PlanUnknown, PlanPaid}
	if err := plugin.setProfileWithOrder(ProfileCustom, nil, order); err != nil {
		t.Fatalf("setProfileWithOrder() error = %v", err)
	}
	got := plugin.policy.Load()
	if got.ActiveProfile != ProfileCustom || !reflect.DeepEqual(got.CustomPlanOrder, order) {
		t.Fatalf("custom profile = %q order = %#v", got.ActiveProfile, got.CustomPlanOrder)
	}
}

func TestManagementAccountsResponseContainsNoCredentialJSON(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.inventory = []pluginapi.HostAuthFileEntry{{
		ID: "auth-a", AuthIndex: "index-a", Provider: "codex", Email: "user@example.com",
	}}
	response, err := plugin.managementResponse(managementRequest{
		Method: "GET",
		Path:   "/v0/management/codex-account-pool/accounts",
	})
	if err != nil {
		t.Fatalf("managementResponse() error = %v", err)
	}
	text := strings.ToLower(string(response.Body))
	if strings.Contains(text, "access_token") || strings.Contains(text, "refresh_token") {
		t.Fatalf("accounts response contains credential data: %s", text)
	}
	if response.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Headers.Get("Cache-Control"))
	}
}

func TestStaticResourceContainsNoAccountState(t *testing.T) {
	text := strings.ToLower(string(accountPoolHTML))
	for _, forbidden := range []string{"user@example.com", "access_token", "auth-a"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("static resource contains %q", forbidden)
		}
	}
}

func TestPreviewDoesNotConsumeSchedulerState(t *testing.T) {
	plugin := newTestPlugin(t)
	now := time.Now()
	plugin.inventory = []pluginapi.HostAuthFileEntry{
		{ID: "a", Provider: "codex", Status: "active", Priority: 100},
		{ID: "b", Provider: "codex", Status: "active", Priority: 100},
	}
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 100, Weight: 3},
			"b": {Enabled: true, Priority: 100, Weight: 1},
		},
	})
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"a": {AuthID: "a", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80, RefreshedAt: now},
			"b": {AuthID: "b", Plan: PlanPaid, FiveHourRemaining: 80, WeeklyRemaining: 80, RefreshedAt: now},
		},
	})

	first, err := plugin.selector.pick(codexRequest("a", "b"), *plugin.policy.Load(), *plugin.quota.Load(), plugin.config)
	if err != nil {
		t.Fatalf("first pick() error = %v", err)
	}
	if first.AuthID != "a" {
		t.Fatalf("first auth = %q, want a", first.AuthID)
	}
	preview, err := plugin.preview("", "")
	if err != nil {
		t.Fatalf("preview() error = %v", err)
	}
	next, err := plugin.selector.pick(codexRequest("a", "b"), *plugin.policy.Load(), *plugin.quota.Load(), plugin.config)
	if err != nil {
		t.Fatalf("next pick() error = %v", err)
	}
	if preview["selected_auth"] != next.AuthID {
		t.Fatalf("preview selected = %v, next auth = %q", preview["selected_auth"], next.AuthID)
	}
	group, ok := preview["selection_group"].([]string)
	if !ok || len(group) != 2 {
		t.Fatalf("preview selection group = %#v", preview["selection_group"])
	}
}

func TestManagementRejectsInvalidRefreshPlan(t *testing.T) {
	plugin := newTestPlugin(t)
	_, err := plugin.managementResponse(managementRequest{
		Method: "POST",
		Path:   "/v0/management/codex-account-pool/refresh",
		Body:   []byte(`{"plan":"enterprise"}`),
	})
	if err == nil {
		t.Fatal("managementResponse() error = nil, want invalid plan")
	}
}

func TestStaticResourceIncludesBulkReserveAndTemporaryControls(t *testing.T) {
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
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("static resource is missing %q", required)
		}
	}
}

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
	return plugin
}
