package main

import (
	"encoding/json"
	"fmt"
	"net/http"
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
