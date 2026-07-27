package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseUsageResponseNormalizesWindows(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	raw := []byte(`{
		"plan_type":"free",
		"rate_limit":{
			"primary_window":{"used_percent":25,"reset_at":1785157200,"limit_window_seconds":18000},
			"secondary_window":{"used_percent":60,"reset_after_seconds":3600,"limit_window_seconds":604800}
		}
	}`)
	got, err := parseUsageResponse("auth-a", raw, now)
	if err != nil {
		t.Fatalf("parseUsageResponse() error = %v", err)
	}
	if got.Plan != PlanFree || got.FiveHourRemaining != 75 || got.WeeklyRemaining != 40 {
		t.Fatalf("snapshot = %#v", got)
	}
	if got.FiveHourWindowMinutes != 300 || got.WeeklyWindowMinutes != 10080 {
		t.Fatalf("window minutes = %d/%d", got.FiveHourWindowMinutes, got.WeeklyWindowMinutes)
	}
	if got.WeeklyResetAt.IsZero() {
		t.Fatal("weekly reset is zero")
	}
}

func TestParseUsageResponseRejectsMissingOrOutOfRangePercentages(t *testing.T) {
	for _, raw := range []string{
		`{"rate_limit":{"primary_window":{},"secondary_window":{"used_percent":10}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":101},"secondary_window":{"used_percent":10}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":-1}}}`,
	} {
		if _, err := parseUsageResponse("auth-a", []byte(raw), time.Now()); err == nil {
			t.Fatalf("parseUsageResponse(%s) error = nil, want invalid percentage", raw)
		}
	}
}

func TestParseUsageResponseAcceptsWeeklyOnlyPrimaryWindow(t *testing.T) {
	raw := []byte(`{
		"plan_type":"free",
		"rate_limit":{
			"primary_window":{"used_percent":27,"limit_window_seconds":604800}
		}
	}`)
	got, err := parseUsageResponse("auth-free", raw, time.Now())
	if err != nil {
		t.Fatalf("parseUsageResponse() error = %v", err)
	}
	if got.FiveHourWindowPresent || !got.WeeklyWindowPresent {
		t.Fatalf("window presence = five-hour:%v weekly:%v", got.FiveHourWindowPresent, got.WeeklyWindowPresent)
	}
	if got.WeeklyRemaining != 73 || got.WeeklyWindowMinutes != 10080 {
		t.Fatalf("weekly window = %d%%/%d minutes", got.WeeklyRemaining, got.WeeklyWindowMinutes)
	}
}

func TestParseUsageResponseRejectsMissingWindows(t *testing.T) {
	for _, raw := range []string{
		`{"plan_type":"team","rate_limit":{}}`,
		`{"plan_type":"free"}`,
	} {
		if _, err := parseUsageResponse("auth-a", []byte(raw), time.Now()); err == nil {
			t.Fatalf("parseUsageResponse(%s) error = nil, want missing window", raw)
		}
	}
}

func TestExtractCredentialSupportsNestedToken(t *testing.T) {
	raw := json.RawMessage(`{"type":"codex","token":{"access_token":"secret-token","account_id":"account-a"}}`)
	got, err := extractCredential(raw)
	if err != nil {
		t.Fatalf("extractCredential() error = %v", err)
	}
	if got.AccessToken != "secret-token" || got.AccountID != "account-a" {
		t.Fatalf("credential = %#v", got)
	}
}

func TestQuotaPersistenceContainsNoSecrets(t *testing.T) {
	store := newStateStore(t.TempDir())
	doc := QuotaDocument{Accounts: map[string]QuotaSnapshot{
		"auth-a": {
			AuthID:            "auth-a",
			Plan:              PlanFree,
			FiveHourRemaining: 80,
			WeeklyRemaining:   60,
			RefreshedAt:       time.Now(),
		},
	}}
	if err := store.saveQuota(doc); err != nil {
		t.Fatalf("saveQuota() error = %v", err)
	}
	raw, err := store.readQuotaBytes()
	if err != nil {
		t.Fatalf("readQuotaBytes() error = %v", err)
	}
	text := strings.ToLower(string(raw))
	for _, secretKey := range []string{"access_token", "refresh_token", "id_token", "secret-token"} {
		if strings.Contains(text, secretKey) {
			t.Fatalf("quota state contains %q: %s", secretKey, text)
		}
	}
}

func TestApplyRefreshResultPreservesPreviousSnapshotOnFailure(t *testing.T) {
	plugin := newTestPlugin(t)
	refreshedAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	plugin.quota.Store(&QuotaDocument{
		Version: stateVersion,
		Accounts: map[string]QuotaSnapshot{
			"auth-a": {
				AuthID:            "auth-a",
				Plan:              PlanFree,
				FiveHourRemaining: 72,
				WeeklyRemaining:   54,
				RefreshedAt:       refreshedAt,
			},
		},
	})

	plugin.applyRefreshResult(QuotaSnapshot{AuthID: "auth-a"}, errors.New("access_token=secret-token rejected"))

	got := plugin.quota.Load().Accounts["auth-a"]
	if got.Plan != PlanFree || got.FiveHourRemaining != 72 || got.WeeklyRemaining != 54 || !got.RefreshedAt.Equal(refreshedAt) {
		t.Fatalf("failed refresh replaced quota values: %#v", got)
	}
	if got.Refresh.State != "failed" || strings.Contains(got.Refresh.LastError, "secret-token") {
		t.Fatalf("refresh status = %#v", got.Refresh)
	}
}

func TestFailedRefreshWithoutAuthIDReleasesQuotaLock(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.applyRefreshResult(QuotaSnapshot{}, errors.New("missing account id"))

	acquired := make(chan struct{})
	go func() {
		plugin.quotaMu.Lock()
		plugin.quotaMu.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("quota lock remained held after refresh without auth id")
	}
}

func TestDetectedPlanUpdatesOnlyAutomaticReserveDefaults(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.config.FreeReserveDefault = 0
	plugin.config.PaidReserveDefault = 20
	plugin.config.UnknownReserveDefault = 10
	plugin.policy.Store(&PolicyDocument{
		Version:       stateVersion,
		Revision:      1,
		ActiveProfile: ProfilePaidFirst,
		Accounts: map[string]AccountPolicy{
			"automatic": {
				Enabled:                true,
				Weight:                 1,
				FiveHourReserve:        10,
				WeeklyReserve:          10,
				ReserveDefaultsForPlan: PlanUnknown,
			},
			"manual": {
				Enabled:         true,
				Weight:          1,
				FiveHourReserve: 7,
				WeeklyReserve:   9,
			},
		},
	})

	plugin.applyDetectedPlanDefaults("automatic", PlanFree)
	plugin.applyDetectedPlanDefaults("manual", PlanPaid)

	automatic := plugin.policy.Load().Accounts["automatic"]
	if automatic.FiveHourReserve != 0 || automatic.WeeklyReserve != 0 || automatic.ReserveDefaultsForPlan != PlanFree {
		t.Fatalf("automatic policy = %#v", automatic)
	}
	manual := plugin.policy.Load().Accounts["manual"]
	if manual.FiveHourReserve != 7 || manual.WeeklyReserve != 9 || manual.ReserveDefaultsForPlan != "" {
		t.Fatalf("manual policy = %#v", manual)
	}
}
