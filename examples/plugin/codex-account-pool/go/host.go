package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostCaller interface {
	Call(method string, payload any) (json.RawMessage, error)
}

type hostHTTPRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

type authListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type quotaClient struct {
	host     hostCaller
	endpoint string
	now      func() time.Time
}

func (c quotaClient) refresh(entry pluginapi.HostAuthFileEntry) (QuotaSnapshot, error) {
	if c.host == nil {
		return QuotaSnapshot{}, fmt.Errorf("host callback is unavailable")
	}
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return QuotaSnapshot{}, fmt.Errorf("account auth index is missing")
	}
	rawAuth, errGet := c.host.Call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: entry.AuthIndex})
	if errGet != nil {
		return QuotaSnapshot{}, fmt.Errorf("read account credential: %w", errGet)
	}
	var authResponse pluginapi.HostAuthGetResponse
	if errUnmarshal := json.Unmarshal(rawAuth, &authResponse); errUnmarshal != nil {
		return QuotaSnapshot{}, fmt.Errorf("decode account credential response: %w", errUnmarshal)
	}
	auth, errCredential := extractCredential(authResponse.JSON)
	if errCredential != nil {
		return QuotaSnapshot{}, errCredential
	}
	headers := http.Header{
		"Authorization": {"Bearer " + auth.AccessToken},
		"Accept":        {"application/json"},
		"Content-Type":  {"application/json"},
		"OpenAI-Beta":   {"codex-1"},
		"Originator":    {"Codex Desktop"},
		"User-Agent":    {"codex-account-pool/0.1.0"},
	}
	if auth.AccountID != "" {
		headers.Set("ChatGPT-Account-Id", auth.AccountID)
	}
	rawHTTP, errHTTP := c.host.Call(pluginabi.MethodHostHTTPDo, hostHTTPRequest{
		Method:  http.MethodGet,
		URL:     c.endpoint,
		Headers: headers,
	})
	if errHTTP != nil {
		return QuotaSnapshot{}, fmt.Errorf("request quota endpoint: %w", errHTTP)
	}
	var response pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(rawHTTP, &response); errUnmarshal != nil {
		return QuotaSnapshot{}, fmt.Errorf("decode quota HTTP response: %w", errUnmarshal)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return QuotaSnapshot{}, fmt.Errorf("quota endpoint returned HTTP %d", response.StatusCode)
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	return parseUsageResponse(entry.ID, response.Body, now())
}
