package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNormalizePolicyRejectsInvalidWeight(t *testing.T) {
	_, err := normalizePolicyDocument(PolicyDocument{
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"auth-a": {Enabled: true, Weight: 0},
		},
	})
	if err == nil {
		t.Fatal("normalizePolicyDocument() error = nil, want invalid weight")
	}
}

func TestNormalizePolicyRejectsInvalidReserveAndProfile(t *testing.T) {
	tests := []PolicyDocument{
		{
			ActiveProfile: RouteProfile("invalid"),
			Accounts:      map[string]AccountPolicy{},
		},
		{
			ActiveProfile: ProfilePaidFirst,
			Accounts: map[string]AccountPolicy{
				"auth-a": {Enabled: true, Weight: 1, FiveHourReserve: 101},
			},
		},
	}
	for index, doc := range tests {
		if _, err := normalizePolicyDocument(doc); err == nil {
			t.Fatalf("case %d normalizePolicyDocument() error = nil", index)
		}
	}
}

func TestNormalizePolicyAppliesDefaults(t *testing.T) {
	got, err := normalizePolicyDocument(PolicyDocument{
		Accounts: map[string]AccountPolicy{
			" auth-a ": {Enabled: true, Priority: 50, Weight: 2},
		},
	})
	if err != nil {
		t.Fatalf("normalizePolicyDocument() error = %v", err)
	}
	if got.Version != stateVersion || got.ActiveProfile != ProfilePaidFirst {
		t.Fatalf("normalized header = %#v", got)
	}
	policy, ok := got.Accounts["auth-a"]
	if !ok || policy.Priority != 50 || policy.Weight != 2 {
		t.Fatalf("normalized policy = %#v", got.Accounts)
	}
}

func TestStateStoreRoundTripUsesPrivateFiles(t *testing.T) {
	store := newStateStore(t.TempDir())
	want := defaultPolicyDocument()
	want.Accounts["auth-a"] = AccountPolicy{Enabled: true, Priority: 80, Weight: 3}

	if err := store.savePolicy(want); err != nil {
		t.Fatalf("savePolicy() error = %v", err)
	}
	got, err := store.loadPolicy()
	if err != nil {
		t.Fatalf("loadPolicy() error = %v", err)
	}
	if got.Accounts["auth-a"].Weight != 3 {
		t.Fatalf("loaded policy = %#v", got.Accounts["auth-a"])
	}
	info, err := os.Stat(filepath.Join(store.dir, policyFileName))
	if err != nil {
		t.Fatalf("stat policy file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("policy mode = %o, want 600", info.Mode().Perm())
	}
}

func TestStateStoreRetainsCorruptPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, policyFileName)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write corrupt policy: %v", err)
	}
	store := newStateStore(dir)
	if _, err := store.loadPolicy(); err == nil {
		t.Fatal("loadPolicy() error = nil, want corrupt policy error")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corrupt policy: %v", err)
	}
	if string(raw) != "{broken" {
		t.Fatalf("corrupt policy was overwritten: %q", raw)
	}
}

func TestConfigureDoesNotOverwriteCorruptPolicyDuringInventorySync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, policyFileName)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write corrupt policy: %v", err)
	}
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{
			Files: []pluginapi.HostAuthFileEntry{{
				ID: "auth-a", AuthIndex: "index-a", Provider: "codex", Priority: 80,
			}},
		}),
	}}
	plugin := newAccountPoolPlugin(host)
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("state_dir: " + dir + "\nrefresh_interval: 1h\n")})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	if errConfigure := plugin.configure(request); errConfigure != nil {
		t.Fatalf("configure() error = %v", errConfigure)
	}
	defer plugin.shutdown()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corrupt policy: %v", err)
	}
	if string(raw) != "{broken" {
		t.Fatalf("corrupt policy was overwritten: %q", raw)
	}
	if !plugin.isPolicyDegraded() {
		t.Fatal("policy degraded status = false")
	}
	policy := plugin.policy.Load().Accounts["auth-a"]
	if policy.Priority != 80 || policy.Weight != 1 || !policy.Enabled {
		t.Fatalf("default synchronized policy = %#v", policy)
	}
}

func TestInventorySyncCreatesDefaultPolicyFromHostPriority(t *testing.T) {
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthList: mustJSON(t, authListResponse{
			Files: []pluginapi.HostAuthFileEntry{{
				ID: "auth-a", AuthIndex: "index-a", Provider: "codex", Priority: 70,
			}},
		}),
	}}
	plugin := newTestPlugin(t)
	plugin.host = host
	if err := plugin.syncInventory(); err != nil {
		t.Fatalf("syncInventory() error = %v", err)
	}
	got := plugin.policy.Load().Accounts["auth-a"]
	if !got.Enabled || got.Priority != 70 || got.Weight != 1 {
		t.Fatalf("default account policy = %#v", got)
	}
}

func TestScheduledAccountsSynchronizesLatestInventory(t *testing.T) {
	var calls int
	host := &fakeHostCaller{}
	host.handler = func(method string, _ any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostAuthList {
			return nil, fmt.Errorf("unexpected host method %s", method)
		}
		calls++
		files := []pluginapi.HostAuthFileEntry{{
			ID: "auth-a", AuthIndex: "index-a", Provider: "codex", Priority: 70,
		}}
		if calls > 1 {
			files = append(files, pluginapi.HostAuthFileEntry{
				ID: "auth-b", AuthIndex: "index-b", Provider: "codex", Priority: 60,
			})
		}
		return mustJSON(t, authListResponse{Files: files}), nil
	}
	plugin := newTestPlugin(t)
	plugin.host = host

	if got := plugin.scheduledAccounts(); len(got) != 1 {
		t.Fatalf("first scheduled accounts = %#v", got)
	}
	if got := plugin.scheduledAccounts(); len(got) != 2 {
		t.Fatalf("second scheduled accounts = %#v", got)
	}
	if _, ok := plugin.policy.Load().Accounts["auth-b"]; !ok {
		t.Fatal("new inventory account has no default policy")
	}
}

func TestEffectiveProfileExpiresTemporaryOverride(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	doc := defaultPolicyDocument()
	doc.ActiveProfile = ProfilePaidFirst
	doc.TemporaryOverride = &ProfileOverride{
		Profile:   ProfileFreeFirst,
		ExpiresAt: now.Add(-time.Second),
	}
	if got := effectiveProfile(doc, now); got != ProfilePaidFirst {
		t.Fatalf("effectiveProfile() = %q, want %q", got, ProfilePaidFirst)
	}
}
