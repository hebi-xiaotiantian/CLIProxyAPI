package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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

func TestSetAutomaticProfilePreservesManualPriorityAndWeight(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 123, Weight: 7},
		},
	})

	if err := plugin.setProfile(ProfileQuotaLowFirst, nil); err != nil {
		t.Fatalf("setProfile() error = %v", err)
	}
	got := plugin.policy.Load()
	if got.ActiveProfile != ProfileQuotaLowFirst {
		t.Fatalf("active profile = %q, want %q", got.ActiveProfile, ProfileQuotaLowFirst)
	}
	if got.Accounts["a"].Priority != 123 || got.Accounts["a"].Weight != 7 {
		t.Fatalf("manual policy changed = %#v", got.Accounts["a"])
	}
}

func TestManagementUpdatesCustomPlanOrderWithoutChangingProfile(t *testing.T) {
	plugin := newTestPlugin(t)
	expiresAt := time.Now().Add(time.Hour).UTC()
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      3,
		ActiveProfile: ProfilePaidFirst,
		TemporaryOverride: &ProfileOverride{
			Profile:   ProfileCustom,
			ExpiresAt: expiresAt,
		},
		CustomPlanOrder: []PlanKind{PlanPaid, PlanFree, PlanUnknown},
		Accounts:        map[string]AccountPolicy{},
	})

	response, err := plugin.managementResponse(managementRequest{
		Method: http.MethodPut,
		Path:   "/v0/management/codex-account-pool/profile",
		Body:   []byte(`{"custom_plan_order":["free","unknown","paid"]}`),
	})
	if err != nil {
		t.Fatalf("managementResponse() error = %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	got := plugin.policy.Load()
	if got.ActiveProfile != ProfilePaidFirst || got.TemporaryOverride == nil ||
		got.TemporaryOverride.Profile != ProfileCustom || !got.TemporaryOverride.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("profile changed = %#v", got)
	}
	wantOrder := []PlanKind{PlanFree, PlanUnknown, PlanPaid}
	if !reflect.DeepEqual(got.CustomPlanOrder, wantOrder) {
		t.Fatalf("custom order = %#v, want %#v", got.CustomPlanOrder, wantOrder)
	}
}

func TestManagementAccountsResponseContainsNoCredentialJSON(t *testing.T) {
	plugin := newTestPlugin(t)
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, map[string]any{
			"files": []map[string]any{{
				"id":            "visible-auth-a",
				"auth_index":    "sensitive-auth-index-sentinel",
				"name":          "credential-file.json",
				"provider":      "codex",
				"label":         "Visible Account",
				"email":         "visible@example.com",
				"path":          "/sensitive/path/sentinel.json",
				"access_token":  "sensitive-access-token-sentinel",
				"refresh_token": "sensitive-refresh-token-sentinel",
				"secret":        "sensitive-secret-sentinel",
			}},
		}),
	}}
	plugin.host = host
	response, err := plugin.managementResponse(managementRequest{
		Method: "GET",
		Path:   "/v0/management/codex-account-pool/accounts",
	})
	if err != nil {
		t.Fatalf("managementResponse() error = %v", err)
	}
	text := strings.ToLower(string(response.Body))
	for _, public := range []string{"visible-auth-a", "visible account", "visible@example.com"} {
		if !strings.Contains(text, public) {
			t.Fatalf("accounts response is missing public field %q: %s", public, text)
		}
	}
	for _, sensitive := range []string{
		"auth_index",
		"sensitive-auth-index-sentinel",
		"access_token",
		"sensitive-access-token-sentinel",
		"refresh_token",
		"sensitive-refresh-token-sentinel",
		`"secret"`,
		"sensitive-secret-sentinel",
		`"path"`,
		"/sensitive/path/sentinel.json",
	} {
		if strings.Contains(text, sensitive) {
			t.Fatalf("accounts response contains sensitive sentinel %q: %s", sensitive, text)
		}
	}
	if response.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Headers.Get("Cache-Control"))
	}

	host.mu.Lock()
	calls := append([]string(nil), host.calls...)
	host.mu.Unlock()
	sawAuthList := false
	for _, method := range calls {
		if method == pluginabi.MethodHostAuthGet {
			t.Fatalf("host calls include %q: %#v", pluginabi.MethodHostAuthGet, calls)
		}
		if method == pluginabi.MethodHostAuthList {
			sawAuthList = true
		}
	}
	if !sawAuthList {
		t.Fatalf("host calls = %#v, want %q", calls, pluginabi.MethodHostAuthList)
	}
}

func TestManagementAccountsIncludeUsageOnlyForVisibleObservedAccounts(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.host = &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{
			Files: []pluginapi.HostAuthFileEntry{
				{ID: "auth-a", AuthIndex: "index-a", Provider: "codex", Email: "a@example.com"},
				{ID: "auth-b", AuthIndex: "index-b", Provider: "codex", Email: "b@example.com"},
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
	if response.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Headers.Get("Cache-Control"))
	}
	if strings.Contains(string(response.Body), "temporarily-absent") {
		t.Fatalf("accounts response contains absent account usage: %s", response.Body)
	}

	var body struct {
		Accounts []accountView `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("unmarshal accounts response: %v", errUnmarshal)
	}
	if len(body.Accounts) != 2 {
		t.Fatalf("accounts length = %d, want 2", len(body.Accounts))
	}
	byID := make(map[string]accountView, len(body.Accounts))
	for _, account := range body.Accounts {
		byID[account.ID] = account
	}
	authA := byID["auth-a"]
	if authA.Usage == nil {
		t.Fatal("auth-a usage = nil")
	}
	if authA.Usage.Requests != 1 ||
		authA.Usage.FailedRequests != 1 ||
		authA.Usage.InputTokens != 100 ||
		authA.Usage.OutputTokens != 25 ||
		authA.Usage.ReasoningTokens != 10 ||
		authA.Usage.CacheTokens != 35 ||
		authA.Usage.TotalTokens != 125 ||
		authA.Usage.UpdatedAt.IsZero() {
		t.Fatalf("auth-a usage = %#v", authA.Usage)
	}
	if authB := byID["auth-b"]; authB.Usage != nil {
		t.Fatalf("auth-b usage = %#v, want nil", authB.Usage)
	}

	var rawBody struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &rawBody); errUnmarshal != nil {
		t.Fatalf("unmarshal raw accounts response: %v", errUnmarshal)
	}
	for _, account := range rawBody.Accounts {
		var id string
		if errUnmarshal := json.Unmarshal(account["id"], &id); errUnmarshal != nil {
			t.Fatalf("unmarshal account id: %v", errUnmarshal)
		}
		if id == "auth-b" {
			if _, exists := account["usage"]; exists {
				t.Fatalf("auth-b JSON contains usage: %s", response.Body)
			}
			return
		}
	}
	t.Fatal("auth-b missing from accounts response")
}

func TestManagementStatusReportsSanitizedUsagePersistenceHealth(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.usage.persistenceMu.Lock()
	originalSave := plugin.usage.save
	plugin.usage.save = func(*stateStore, UsageDocument) error {
		return fmt.Errorf("persist failed: access_token=secret-value")
	}
	plugin.usage.persistenceMu.Unlock()
	t.Cleanup(func() {
		plugin.usage.persistenceMu.Lock()
		plugin.usage.save = originalSave
		plugin.usage.persistenceMu.Unlock()
	})

	plugin.usage.observe(pluginapi.UsageRecord{
		Provider: "codex",
		AuthID:   "auth-a",
		Detail:   pluginapi.UsageDetail{TotalTokens: 1},
	})
	if errFlush := plugin.usage.flush(); errFlush == nil {
		t.Fatal("flush() error = nil, want persistence failure")
	}
	status := plugin.statusView()
	if got, ok := status["usage_degraded"].(bool); !ok || !got {
		t.Fatalf("usage_degraded = %#v, want true", status["usage_degraded"])
	}
	usageError, ok := status["usage_error"].(string)
	if !ok || usageError == "" {
		t.Fatalf("usage_error = %#v, want non-empty string", status["usage_error"])
	}
	if strings.Contains(usageError, "secret-value") {
		t.Fatalf("usage_error contains secret: %q", usageError)
	}
	if !strings.Contains(usageError, "[redacted]") {
		t.Fatalf("usage_error = %q, want redaction marker", usageError)
	}

	plugin.usage.persistenceMu.Lock()
	plugin.usage.save = originalSave
	plugin.usage.persistenceMu.Unlock()
	if errFlush := plugin.usage.flush(); errFlush != nil {
		t.Fatalf("flush() after restoring save = %v", errFlush)
	}
}

func TestStatusViewReportsPluginVersion(t *testing.T) {
	plugin := newTestPlugin(t)
	if got := plugin.statusView()["version"]; got != "0.2.0" {
		t.Fatalf("status version = %#v, want 0.2.0", got)
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

func TestPreviewReturnsStructuredFunnelAndCandidateDiagnostics(t *testing.T) {
	plugin := newTestPlugin(t)
	now := time.Now()
	plugin.inventory = []pluginapi.HostAuthFileEntry{
		{ID: "selected", Provider: "codex", Status: "active", Priority: 1, Label: "Selected account"},
		{ID: "backup", Provider: "codex", Status: "active", Priority: 999, Label: "Backup account"},
		{ID: "host-disabled", Provider: "codex", Status: "active", Priority: 999, Label: "Host disabled", Disabled: true},
		{ID: "plugin-disabled", Provider: "codex", Status: "active", Priority: 999, Label: "Plugin disabled"},
		{ID: "below-reserve", Provider: "codex", Status: "active", Priority: 999, Label: "Below reserve"},
	}
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfileAuto,
		Accounts: map[string]AccountPolicy{
			"selected":        {Enabled: true, Priority: 1, Weight: 1},
			"backup":          {Enabled: true, Priority: 999, Weight: 9, Backup: true},
			"host-disabled":   {Enabled: true, Priority: 999, Weight: 9},
			"plugin-disabled": {Enabled: false, Priority: 999, Weight: 9},
			"below-reserve": {
				Enabled: true, Priority: 999, Weight: 9,
				FiveHourReserve: 20, WeeklyReserve: 20,
			},
		},
	})
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"selected": {
				AuthID: "selected", Plan: PlanPaid, PlanType: "pro",
				FiveHourRemaining: 60, WeeklyRemaining: 50,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
			"backup": {
				AuthID: "backup", Plan: PlanPaid, PlanType: "enterprise",
				FiveHourRemaining: 90, WeeklyRemaining: 90,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
			"host-disabled": {
				AuthID: "host-disabled", Plan: PlanPaid, PlanType: "enterprise",
				FiveHourRemaining: 90, WeeklyRemaining: 90,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
			"plugin-disabled": {
				AuthID: "plugin-disabled", Plan: PlanPaid, PlanType: "enterprise",
				FiveHourRemaining: 90, WeeklyRemaining: 90,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
			"below-reserve": {
				AuthID: "below-reserve", Plan: PlanPaid, PlanType: "plus",
				FiveHourRemaining: 10, WeeklyRemaining: 50,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
		},
	})

	preview, err := plugin.preview("gpt-5.4", "")
	if err != nil {
		t.Fatalf("preview() error = %v", err)
	}
	raw, errMarshal := json.Marshal(preview)
	if errMarshal != nil {
		t.Fatalf("marshal preview: %v", errMarshal)
	}
	var got struct {
		Profile         RouteProfile `json:"profile"`
		Automatic       bool         `json:"automatic"`
		Scope           string       `json:"scope"`
		Model           string       `json:"model"`
		SelectedAuth    string       `json:"selected_auth"`
		SelectedAccount struct {
			ID         string `json:"id"`
			Label      string `json:"label"`
			Layer      string `json:"layer"`
			PlanType   string `json:"plan_type"`
			QuotaScore int    `json:"quota_score"`
		} `json:"selected_account"`
		Funnel struct {
			Candidates    int `json:"candidates"`
			HostAvailable int `json:"host_available"`
			QuotaEligible int `json:"quota_eligible"`
			ActiveLayer   int `json:"active_layer"`
			StrategyGroup int `json:"strategy_group"`
		} `json:"funnel"`
		Exclusions []struct {
			Reason string `json:"reason"`
			Label  string `json:"label"`
			Count  int    `json:"count"`
		} `json:"exclusions"`
		Candidates []struct {
			ID            string `json:"id"`
			Label         string `json:"label"`
			Reason        string `json:"reason"`
			ReasonLabel   string `json:"reason_label"`
			PlanType      string `json:"plan_type"`
			PlanRank      int    `json:"plan_rank"`
			PlanKnown     bool   `json:"plan_known"`
			QuotaScore    int    `json:"quota_score"`
			QuotaKnown    bool   `json:"quota_known"`
			InActiveLayer bool   `json:"in_active_layer"`
			InActiveGroup bool   `json:"in_active_group"`
		} `json:"candidates"`
	}
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("decode preview: %v", errUnmarshal)
	}
	if got.Profile != ProfileAuto || !got.Automatic || got.Scope != "inventory" || got.Model != "gpt-5.4" {
		t.Fatalf("preview mode = profile:%q automatic:%v scope:%q model:%q", got.Profile, got.Automatic, got.Scope, got.Model)
	}
	if got.SelectedAuth != "selected" || got.SelectedAccount.ID != "selected" ||
		got.SelectedAccount.Label != "Selected account" || got.SelectedAccount.Layer != "regular" ||
		got.SelectedAccount.PlanType != "pro" || got.SelectedAccount.QuotaScore != 50 {
		t.Fatalf("selected account = %#v", got.SelectedAccount)
	}
	if got.Funnel.Candidates != 5 || got.Funnel.HostAvailable != 4 ||
		got.Funnel.QuotaEligible != 2 || got.Funnel.ActiveLayer != 1 || got.Funnel.StrategyGroup != 1 {
		t.Fatalf("funnel = %#v", got.Funnel)
	}
	exclusions := make(map[string]int, len(got.Exclusions))
	for _, item := range got.Exclusions {
		if item.Label == "" {
			t.Fatalf("exclusion %q has empty label", item.Reason)
		}
		exclusions[item.Reason] = item.Count
	}
	wantExclusions := map[string]int{
		"host_disabled":     1,
		"plugin_disabled":   1,
		"five_hour_reserve": 1,
	}
	if !reflect.DeepEqual(exclusions, wantExclusions) {
		t.Fatalf("exclusions = %#v, want %#v", exclusions, wantExclusions)
	}
	if len(got.Candidates) != 5 || got.Candidates[0].ID != "selected" {
		t.Fatalf("candidates = %#v", got.Candidates)
	}
	selected := got.Candidates[0]
	if selected.Label != "Selected account" || selected.PlanType != "pro" ||
		selected.PlanRank != 400 || !selected.PlanKnown || selected.QuotaScore != 50 ||
		!selected.QuotaKnown || !selected.InActiveLayer || !selected.InActiveGroup {
		t.Fatalf("selected candidate = %#v", selected)
	}
	for _, candidate := range got.Candidates {
		if candidate.Reason != "" && candidate.ReasonLabel == "" {
			t.Fatalf("candidate %q reason %q has no label", candidate.ID, candidate.Reason)
		}
	}
}

func TestPreviewUsesAutomaticSelectionGroup(t *testing.T) {
	plugin := newTestPlugin(t)
	now := time.Now()
	plugin.inventory = []pluginapi.HostAuthFileEntry{
		{ID: "high-quota", Provider: "codex", Status: "active", Priority: 999},
		{ID: "low-quota", Provider: "codex", Status: "active", Priority: 1},
	}
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfileQuotaLowFirst,
		Accounts: map[string]AccountPolicy{
			"high-quota": {Enabled: true, Priority: 999, Weight: 9},
			"low-quota":  {Enabled: true, Priority: 1, Weight: 1},
		},
	})
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"high-quota": {
				AuthID: "high-quota", Plan: PlanPaid, PlanType: "pro",
				FiveHourRemaining: 80, WeeklyRemaining: 80,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
			"low-quota": {
				AuthID: "low-quota", Plan: PlanFree, PlanType: "free",
				FiveHourRemaining: 20, WeeklyRemaining: 20,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
		},
	})

	preview, err := plugin.preview("", "")
	if err != nil {
		t.Fatalf("preview() error = %v", err)
	}
	group, ok := preview["selection_group"].([]string)
	if !ok || !reflect.DeepEqual(group, []string{"low-quota"}) {
		t.Fatalf("selection group = %#v, want low-quota", preview["selection_group"])
	}
	if preview["selected_auth"] != "low-quota" {
		t.Fatalf("selected auth = %#v, want low-quota", preview["selected_auth"])
	}
}

func TestPreviewDoesNotRecordSchedulingDecision(t *testing.T) {
	plugin := newTestPlugin(t)
	now := time.Now()
	plugin.inventory = []pluginapi.HostAuthFileEntry{
		{ID: "a", Provider: "codex", Status: "active", Priority: 100},
	}
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"a": {Enabled: true, Priority: 100, Weight: 1},
		},
	})
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"a": {
				AuthID: "a", Plan: PlanPaid, PlanType: "plus",
				FiveHourRemaining: 80, WeeklyRemaining: 80,
				FiveHourWindowPresent: true, WeeklyWindowPresent: true, RefreshedAt: now,
			},
		},
	})

	if _, err := plugin.preview("", "preview-session"); err != nil {
		t.Fatalf("preview() error = %v", err)
	}
	if got := plugin.decisions.snapshot(); len(got) != 0 {
		t.Fatalf("preview decisions = %#v, want none", got)
	}
}

func TestManagementDecisionsReturnsNewestRealEntries(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.decisions.add(schedulingDecision{
		Timestamp: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		AuthID:    "older",
		Profile:   ProfilePaidFirst,
		Layer:     "regular",
	})
	plugin.decisions.add(schedulingDecision{
		Timestamp: time.Date(2026, 7, 27, 12, 1, 0, 0, time.UTC),
		AuthID:    "newer",
		Profile:   ProfileAuto,
		Layer:     "backup",
	})

	response, err := plugin.managementResponse(managementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/codex-account-pool/decisions",
	})
	if err != nil {
		t.Fatalf("managementResponse() error = %v", err)
	}
	var body struct {
		Decisions []schedulingDecision `json:"decisions"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &body); errUnmarshal != nil {
		t.Fatalf("decode decisions response: %v", errUnmarshal)
	}
	if len(body.Decisions) != 2 || body.Decisions[0].AuthID != "newer" || body.Decisions[1].AuthID != "older" {
		t.Fatalf("decisions = %#v", body.Decisions)
	}
	if response.Headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", response.Headers.Get("Cache-Control"))
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
		"async function refreshAccounts(generation = null)",
		"refreshAccounts(generation)",
		"accountRequestSequence",
		"startAccountRequest",
		"accountRequestIsCurrent",
		"applyAccountsResponse",
		"request.sequence === accountRequestSequence",
		"request.generation === null || request.generation === refreshPollGeneration",
		"activeRefreshPollGeneration",
		"finishRefreshPolling",
		"activeRefreshPollGeneration = generation",
		"activeRefreshPollGeneration !== generation",
		"document.hidden || !managementKey || activeRefreshPollGeneration !== 0",
		"refreshAccountsResult",
		`reason: "superseded"`,
		`accountResult.reason === "superseded"`,
		"const generation = activeRefreshPollGeneration",
		"refreshAccounts(generation)",
		"loadSequence",
		"stageEdit",
		"input.oninput",
		"accountEditIsFocused",
		"renderAccounts(true)",
		`tabindex="0"`,
		`role="status" aria-live="polite"`,
		"refreshPollTimer",
		"5000",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("static resource is missing %q", required)
		}
	}
	if count := strings.Count(text, "startAccountRequest(generation)"); count != 2 {
		t.Fatalf("startAccountRequest(generation) count = %d, want load and refreshAccounts", count)
	}
	if count := strings.Count(text, "accounts = accountData.accounts || [];"); count != 1 {
		t.Fatalf("direct accounts assignment count = %d, want one guarded assignment", count)
	}
	if count := strings.Count(text, "finishRefreshPolling(generation)"); count < 4 {
		t.Fatalf("finishRefreshPolling(generation) count = %d, want all polling exit paths guarded", count)
	}
	if strings.Contains(text, "!document.hidden && managementKey && activeRefreshPollGeneration === 0") {
		t.Fatal("visibility refresh must not require activeRefreshPollGeneration === 0")
	}
	if count := strings.Count(text, "pending.clear()"); count != 1 {
		t.Fatalf("pending.clear() count = %d, want 1 only after a successful apply", count)
	}
}

func TestWebIndexJavaScript(t *testing.T) {
	nodePath, errLookPath := exec.LookPath("node")
	if errLookPath != nil {
		t.Skip("node is not installed")
	}
	command := exec.Command(nodePath, "--test", "web/index.test.mjs")
	output, errRun := command.CombinedOutput()
	if errRun != nil {
		t.Fatalf("node --test web/index.test.mjs failed: %v\n%s", errRun, output)
	}
}

func TestStaticResourceIncludesAutomaticModesAndVisualDiagnostics(t *testing.T) {
	text := string(accountPoolHTML)
	for _, required := range []string{
		"automaticProfileSegments",
		"strictProfileSegments",
		"profileRule",
		"manualRoutingNotice",
		"previewFunnel",
		"previewSelected",
		"previewCandidates",
		"previewExclusions",
		"decisionRows",
		`api("/decisions")`,
		`data-profile="auto"`,
		`data-profile="quota-high-first"`,
		`data-profile="quota-low-first"`,
		`data-profile="plan-high-first"`,
		`data-profile="plan-low-first"`,
		"库存级预估",
		"refreshPollGeneration",
		"refreshRequestGeneration",
		"async function load(generation = null)",
		"requestGeneration < refreshRequestGeneration",
		`$("customPlanOrder").onchange`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("static resource is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`<pre class="preview"`,
		`JSON.stringify(await api("/preview"`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("static resource still contains raw preview marker %q", forbidden)
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
	if errConfigure := plugin.usage.configureStore(plugin.store); errConfigure != nil {
		t.Fatalf("configure usage store: %v", errConfigure)
	}
	t.Cleanup(plugin.shutdown)
	return plugin
}
