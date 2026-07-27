package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type fakeHostCaller struct {
	mu       sync.Mutex
	calls    []string
	requests []any
	results  map[string]json.RawMessage
	errors   map[string]error
	handler  func(string, any) (json.RawMessage, error)
}

func (f *fakeHostCaller) Call(method string, payload any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	f.requests = append(f.requests, payload)
	if f.handler != nil {
		return f.handler(method, payload)
	}
	if err := f.errors[method]; err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), f.results[method]...), nil
}

func TestQuotaClientUsesHostCallbacks(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthGet: mustJSON(t, pluginapi.HostAuthGetResponse{
			AuthIndex: "index-a",
			JSON:      json.RawMessage(`{"access_token":"secret-token","account_id":"account-a"}`),
		}),
		pluginabi.MethodHostHTTPDo: mustJSON(t, pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Body:       []byte(`{"plan_type":"free","rate_limit":{"primary_window":{"used_percent":20},"secondary_window":{"used_percent":40}}}`),
		}),
	}}
	client := quotaClient{host: host, endpoint: "https://example.invalid/usage", now: func() time.Time { return now }}
	got, err := client.refresh(pluginapi.HostAuthFileEntry{ID: "auth-a", AuthIndex: "index-a", Provider: "codex"})
	if err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if got.Plan != PlanFree || got.FiveHourRemaining != 80 || got.WeeklyRemaining != 60 {
		t.Fatalf("snapshot = %#v", got)
	}
	if len(host.calls) != 2 || host.calls[0] != pluginabi.MethodHostAuthGet || host.calls[1] != pluginabi.MethodHostHTTPDo {
		t.Fatalf("host calls = %#v", host.calls)
	}
	request, ok := host.requests[1].(hostHTTPRequest)
	if !ok {
		t.Fatalf("HTTP payload type = %T", host.requests[1])
	}
	if request.Headers.Get("Authorization") != "Bearer secret-token" || request.Headers.Get("ChatGPT-Account-Id") != "account-a" {
		t.Fatalf("quota headers = %#v", request.Headers)
	}
}

func TestQuotaClientSanitizesHTTPFailure(t *testing.T) {
	host := &fakeHostCaller{results: map[string]json.RawMessage{
		pluginabi.MethodHostAuthGet: mustJSON(t, pluginapi.HostAuthGetResponse{
			AuthIndex: "index-a",
			JSON:      json.RawMessage(`{"access_token":"secret-token"}`),
		}),
		pluginabi.MethodHostHTTPDo: mustJSON(t, pluginapi.HTTPResponse{
			StatusCode: http.StatusUnauthorized,
			Body:       []byte(`{"error":"secret-token rejected"}`),
		}),
	}}
	_, err := (quotaClient{host: host, endpoint: "https://example.invalid/usage", now: time.Now}).refresh(
		pluginapi.HostAuthFileEntry{ID: "auth-a", AuthIndex: "index-a", Provider: "codex"},
	)
	if err == nil {
		t.Fatal("refresh() error = nil, want HTTP failure")
	}
	if text := err.Error(); text != "quota endpoint returned HTTP 401" {
		t.Fatalf("refresh() error = %q", text)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}
