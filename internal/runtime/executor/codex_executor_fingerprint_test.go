package executor

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func codexFingerprintAuth(id string, mode string, extra map[string]any) *cliproxyauth.Auth {
	metadata := map[string]any{"codex_fingerprint_mode": mode}
	for k, v := range extra {
		metadata[k] = v
	}
	return &cliproxyauth.Auth{ID: id, Provider: "codex", Metadata: metadata}
}

func TestCodexFingerprintDeviceModeConvergesInstallation(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "device", nil)
	clientBodyA := []byte(`{"model":"gpt-5-codex","client_metadata":{"x-codex-installation-id":"client-device-A","session_id":"sess-A"}}`)
	clientBodyB := []byte(`{"model":"gpt-5-codex","client_metadata":{"x-codex-installation-id":"client-device-B","session_id":"sess-B"}}`)

	bodyA, stateA := applyCodexIdentityConfuseBody(nil, auth, clientBodyA, clientBodyA)
	bodyB, stateB := applyCodexIdentityConfuseBody(nil, auth, clientBodyB, clientBodyB)

	installA := gjson.GetBytes(bodyA, "client_metadata.x-codex-installation-id").String()
	installB := gjson.GetBytes(bodyB, "client_metadata.x-codex-installation-id").String()
	if installA == "" {
		t.Fatal("device mode must set a converged installation id")
	}
	if installA != installB {
		t.Fatalf("installation ids not converged across clients: %q != %q", installA, installB)
	}
	if installA == "client-device-A" || installA == "client-device-B" {
		t.Fatalf("installation id leaked the client value: %q", installA)
	}
	if gjson.GetBytes(bodyA, "client_metadata.session_id").String() != "sess-A" {
		t.Fatalf("device mode must preserve client sessions: %q", gjson.GetBytes(bodyA, "client_metadata.session_id").String())
	}
	if stateA.installationID != installA || stateB.installationID != installB {
		t.Fatalf("identity state installation ids mismatch: %+v / %+v", stateA, stateB)
	}
	if stateA.mode != helps.CodexFingerprintDevice {
		t.Fatalf("state mode = %q, want device", stateA.mode)
	}
}

func TestCodexFingerprintDeviceModeHeaders(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "device", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","client_metadata":{"x-codex-installation-id":"client-device-A"}}`)
	_, state := applyCodexIdentityConfuseBody(nil, auth, clientBody, clientBody)

	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"installation_id":"client-device-A","turn_id":"turn-1"}`)
	applyCodexIdentityConfuseHeaders(headers, &state)

	if got := headers.Get("X-Codex-Installation-Id"); got != state.installationID {
		t.Fatalf("X-Codex-Installation-Id = %q, want %q", got, state.installationID)
	}
	if got := gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "installation_id").String(); got != state.installationID {
		t.Fatalf("turn metadata installation_id = %q, want %q", got, state.installationID)
	}
	if got := headers.Get("Session_id"); got != "" {
		t.Fatalf("device mode must not rewrite session headers, got %q", got)
	}
}

func TestCodexFingerprintSessionModeConvergesSessionAndThread(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "session", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"sess-A","client_metadata":{"x-codex-installation-id":"client-device-A","session_id":"sess-A"}}`)

	body, state := applyCodexIdentityConfuseBody(nil, auth, clientBody, clientBody)

	if got := gjson.GetBytes(body, "client_metadata.session_id").String(); got != state.sessionID {
		t.Fatalf("client_metadata.session_id = %q, want converged %q", got, state.sessionID)
	}
	if got := gjson.GetBytes(body, "client_metadata.thread_id").String(); got != state.threadID {
		t.Fatalf("client_metadata.thread_id = %q, want %q", got, state.threadID)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != state.sessionID {
		t.Fatalf("default prompt_cache_key = %q, want converged session %q", got, state.sessionID)
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-window-id").String(); got != state.threadID+":0" {
		t.Fatalf("x-codex-window-id = %q, want %q", got, state.threadID+":0")
	}
	if state.originalPromptCacheKey != "sess-A" {
		t.Fatalf("original prompt cache key = %q, want sess-A", state.originalPromptCacheKey)
	}
	if state.sessionID == "" || state.threadID == "" {
		t.Fatalf("session mode must converge session and thread: %+v", state)
	}
	if state.threadID == state.sessionID {
		t.Fatal("session mode thread must differ from the converged session")
	}
}

func TestCodexFingerprintSessionModeThreadPerClientSession(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "session", nil)
	bodyA := []byte(`{"model":"gpt-5-codex","client_metadata":{"session_id":"sess-A"}}`)
	bodyB := []byte(`{"model":"gpt-5-codex","client_metadata":{"session_id":"sess-B"}}`)

	_, stateA := applyCodexIdentityConfuseBody(nil, auth, bodyA, bodyA)
	_, stateB := applyCodexIdentityConfuseBody(nil, auth, bodyB, bodyB)

	if stateA.threadID == stateB.threadID {
		t.Fatalf("different client sessions must derive different threads, got %q", stateA.threadID)
	}
	// Same client session stays stable.
	_, stateA2 := applyCodexIdentityConfuseBody(nil, auth, bodyA, bodyA)
	if stateA2.threadID != stateA.threadID {
		t.Fatalf("thread id not stable per client session: %q != %q", stateA2.threadID, stateA.threadID)
	}
}

func TestCodexFingerprintSessionModePreservesCustomCacheKey(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "session", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"custom-conversation-key","client_metadata":{"session_id":"sess-A"}}`)

	body, state := applyCodexIdentityConfuseBody(nil, auth, clientBody, clientBody)

	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "custom-conversation-key" {
		t.Fatalf("custom prompt_cache_key must be preserved, got %q", got)
	}
	if state.promptCacheKey != "" {
		t.Fatalf("custom cache key must not set the converged prompt cache state, got %q", state.promptCacheKey)
	}
}

func TestCodexFingerprintSessionModeHeaders(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "session", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"sess-A","client_metadata":{"session_id":"sess-A"}}`)
	_, state := applyCodexIdentityConfuseBody(nil, auth, clientBody, clientBody)

	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"installation_id":"client-device-A","session_id":"sess-A","thread_id":"thread-A","window_id":"window-A","turn_id":"turn-1"}`)
	headers.Set("Conversation_id", "sess-A")
	applyCodexIdentityConfuseHeaders(headers, &state)

	if got := headers.Get("X-Codex-Installation-Id"); got != state.installationID {
		t.Fatalf("X-Codex-Installation-Id = %q, want %q", got, state.installationID)
	}
	for _, name := range []string{"session_id", "Session-Id", "session-id"} {
		if got := headerValueCaseInsensitive(headers, name); got != state.sessionID {
			t.Fatalf("%s = %q, want converged session %q", name, got, state.sessionID)
		}
	}
	if got := headers.Get("Conversation_id"); got != state.sessionID {
		t.Fatalf("Conversation_id = %q, want %q", got, state.sessionID)
	}
	for _, name := range []string{"X-Client-Request-Id", "Thread-Id"} {
		if got := headers.Get(name); got != state.threadID {
			t.Fatalf("%s = %q, want thread %q", name, got, state.threadID)
		}
	}
	if got := headers.Get("X-Codex-Window-Id"); got != state.threadID+":0" {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", got, state.threadID+":0")
	}
	turnMetadata := headers.Get("X-Codex-Turn-Metadata")
	if got := gjson.Get(turnMetadata, "installation_id").String(); got != state.installationID {
		t.Fatalf("turn metadata installation_id = %q, want %q", got, state.installationID)
	}
	if got := gjson.Get(turnMetadata, "session_id").String(); got != state.sessionID {
		t.Fatalf("turn metadata session_id = %q, want %q", got, state.sessionID)
	}
	if got := gjson.Get(turnMetadata, "thread_id").String(); got != state.threadID {
		t.Fatalf("turn metadata thread_id = %q, want %q", got, state.threadID)
	}
	if got := gjson.Get(turnMetadata, "window_id").String(); got != state.threadID+":0" {
		t.Fatalf("turn metadata window_id = %q, want %q", got, state.threadID+":0")
	}
}

func TestCodexFingerprintFullModeThreadEqualsSession(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "full", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"sess-A","client_metadata":{"session_id":"sess-A"}}`)

	body, state := applyCodexIdentityConfuseBody(nil, auth, clientBody, clientBody)

	if state.sessionID == "" {
		t.Fatal("full mode must converge the session id")
	}
	if state.threadID != state.sessionID {
		t.Fatalf("full mode thread = %q, want session %q", state.threadID, state.sessionID)
	}
	if got := gjson.GetBytes(body, "client_metadata.thread_id").String(); got != state.sessionID {
		t.Fatalf("client_metadata.thread_id = %q, want %q", got, state.sessionID)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != state.sessionID {
		t.Fatalf("prompt_cache_key = %q, want %q", got, state.sessionID)
	}
}

func TestCodexFingerprintOffDisablesEverything(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := codexFingerprintAuth("auth-1", "off", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"cache-1","client_metadata":{"x-codex-installation-id":"client-device-A"}}`)

	body, state := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)

	if state.enabled {
		t.Fatal("explicit off must disable confusion")
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "cache-1" {
		t.Fatalf("prompt_cache_key must be untouched in off mode, got %q", got)
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != "client-device-A" {
		t.Fatalf("installation id must be untouched in off mode, got %q", got)
	}
}

func TestCodexFingerprintUnsetFallsBackToLegacyBehavior(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"cache-1","client_metadata":{"x-codex-installation-id":"install-1"}}`)

	body, state := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)

	expectedPromptCacheKey := codexIdentityConfuseUUID("auth-1", "prompt-cache", "cache-1")
	expectedInstallationID := codexIdentityConfuseUUID("auth-1", "installation", "install-1")
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != expectedPromptCacheKey {
		t.Fatalf("prompt_cache_key = %q, want legacy confused %q", got, expectedPromptCacheKey)
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != expectedInstallationID {
		t.Fatalf("installation id = %q, want legacy confused %q", got, expectedInstallationID)
	}
	if state.mode != "" {
		t.Fatalf("legacy state mode = %q, want empty", state.mode)
	}

	// Without the global toggle there is no confusion either.
	cfgOff := &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}}
	bodyOff, stateOff := applyCodexIdentityConfuseBody(cfgOff, auth, clientBody, clientBody)
	if stateOff.enabled {
		t.Fatal("legacy confusion must stay disabled without the global toggle")
	}
	if got := gjson.GetBytes(bodyOff, "prompt_cache_key").String(); got != "cache-1" {
		t.Fatalf("prompt_cache_key = %q, want untouched", got)
	}
}

func TestCodexFingerprintResponseUnconfusesConvergedCacheKey(t *testing.T) {
	auth := codexFingerprintAuth("auth-1", "session", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"sess-A","client_metadata":{"session_id":"sess-A"}}`)
	_, state := applyCodexIdentityConfuseBody(nil, auth, clientBody, clientBody)

	upstream := []byte(`{"id":"resp-1","prompt_cache_key":"` + state.promptCacheKey + `","output":[{"type":"message","id":"msg-1"}]}`)
	restored := applyCodexIdentityExposeResponsePayload(upstream, state)
	if got := gjson.GetBytes(restored, "prompt_cache_key").String(); got != "sess-A" {
		t.Fatalf("response prompt_cache_key = %q, want original sess-A", got)
	}
}

func TestCodexFingerprintModeWinsOverGlobalToggleOff(t *testing.T) {
	cfg := &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first"}}
	auth := codexFingerprintAuth("auth-1", "device", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","client_metadata":{"x-codex-installation-id":"client-device-A"}}`)

	body, state := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)

	if !state.enabled {
		t.Fatal("per-account device mode must apply even when the global toggle is off")
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != state.installationID {
		t.Fatalf("installation id = %q, want converged %q", got, state.installationID)
	}
}

func TestCodexFingerprintHeadersNoopWithoutState(t *testing.T) {
	headers := http.Header{}
	headers.Set("Session-Id", "keep-me")
	applyCodexIdentityConfuseHeaders(headers, nil)
	if got := headers.Get("Session-Id"); got != "keep-me" {
		t.Fatalf("headers must stay untouched without state, got %q", got)
	}
	disabled := &codexIdentityConfuseState{enabled: false}
	applyCodexIdentityConfuseHeaders(headers, disabled)
	if got := headers.Get("Session-Id"); got != "keep-me" {
		t.Fatalf("headers must stay untouched with disabled state, got %q", got)
	}
	// Real request path smoke test: WS-style headers keep a single session key.
	req := httptest.NewRequest("POST", "https://example.com/responses", nil)
	req.Header.Set("session_id", "old-session")
	auth := codexFingerprintAuth("auth-1", "session", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"sess-A","client_metadata":{"session_id":"sess-A"}}`)
	_, state := applyCodexIdentityConfuseBody(nil, auth, clientBody, clientBody)
	applyCodexIdentityConfuseHeaders(req.Header, &state)
	if got := headerValueCaseInsensitive(req.Header, "session_id"); got != state.sessionID {
		t.Fatalf("session_id = %q, want %q", got, state.sessionID)
	}
	if got := headerValueCaseInsensitive(req.Header, "Session-Id"); got != state.sessionID {
		t.Fatalf("Session-Id = %q, want %q", got, state.sessionID)
	}
}

func TestCodexFingerprintGlobalSwitchAppliesWhenMetadataUnset(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{FingerprintMode: "device"},
	}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	clientBody := []byte(`{"model":"gpt-5-codex","client_metadata":{"x-codex-installation-id":"client-device-A"}}`)

	body, state := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)

	if !state.enabled {
		t.Fatal("global device switch must enable convergence without account metadata")
	}
	if state.mode != helps.CodexFingerprintDevice {
		t.Fatalf("state mode = %q, want device", state.mode)
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != state.installationID {
		t.Fatalf("installation id = %q, want converged %q", got, state.installationID)
	}
}

func TestCodexFingerprintGlobalSwitchAccountMetadataWins(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{FingerprintMode: "full"},
	}
	auth := codexFingerprintAuth("auth-1", "session", nil)
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"sess-A","client_metadata":{"session_id":"sess-A"}}`)

	body, state := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)

	if state.mode != helps.CodexFingerprintSession {
		t.Fatalf("account metadata must override the global switch, mode = %q", state.mode)
	}
	if state.threadID == state.sessionID {
		t.Fatal("session mode must keep per-client threads even under a global full switch")
	}
	if got := gjson.GetBytes(body, "client_metadata.thread_id").String(); got != state.threadID {
		t.Fatalf("client_metadata.thread_id = %q, want %q", got, state.threadID)
	}
}

func TestCodexFingerprintGlobalOffDisablesEverything(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{FingerprintMode: "off", IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"cache-1","client_metadata":{"x-codex-installation-id":"client-device-A"}}`)

	body, state := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)

	if state.enabled {
		t.Fatal("global off switch must disable all confusion")
	}
	if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != "client-device-A" {
		t.Fatalf("installation id must stay untouched, got %q", got)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "cache-1" {
		t.Fatalf("prompt_cache_key must stay untouched, got %q", got)
	}
}
