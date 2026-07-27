package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const weeklyWindowMinutes = 7 * 24 * 60

type credential struct {
	AccessToken string
	AccountID   string
}

type usageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit struct {
		PrimaryWindow   *usageWindow `json:"primary_window"`
		SecondaryWindow *usageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type usageWindow struct {
	UsedPercent        *float64        `json:"used_percent"`
	ResetAt            json.RawMessage `json:"reset_at"`
	ResetAfterSeconds  int64           `json:"reset_after_seconds"`
	LimitWindowSeconds int             `json:"limit_window_seconds"`
	LimitWindowMinutes int             `json:"limit_window_minutes"`
}

func parseUsageResponse(authID string, raw []byte, now time.Time) (QuotaSnapshot, error) {
	var usage usageResponse
	if errUnmarshal := json.Unmarshal(raw, &usage); errUnmarshal != nil {
		return QuotaSnapshot{}, fmt.Errorf("decode quota response: %w", errUnmarshal)
	}
	if usage.RateLimit.PrimaryWindow == nil && usage.RateLimit.SecondaryWindow == nil {
		return QuotaSnapshot{}, fmt.Errorf("quota response has no usage window")
	}
	snapshot := QuotaSnapshot{
		AuthID:      strings.TrimSpace(authID),
		PlanType:    strings.TrimSpace(usage.PlanType),
		Plan:        normalizePlan(usage.PlanType),
		RefreshedAt: now.UTC(),
		Refresh:     RefreshStatus{State: "succeeded"},
	}
	if usage.RateLimit.PrimaryWindow != nil {
		if errApply := applyUsageWindow(&snapshot, *usage.RateLimit.PrimaryWindow, false, now); errApply != nil {
			return QuotaSnapshot{}, fmt.Errorf("invalid primary quota window: %w", errApply)
		}
	}
	if usage.RateLimit.SecondaryWindow != nil {
		if errApply := applyUsageWindow(&snapshot, *usage.RateLimit.SecondaryWindow, true, now); errApply != nil {
			return QuotaSnapshot{}, fmt.Errorf("invalid secondary quota window: %w", errApply)
		}
	}
	return snapshot, nil
}

func applyUsageWindow(snapshot *QuotaSnapshot, window usageWindow, defaultWeekly bool, now time.Time) error {
	remaining, errRemaining := remainingPercent(window.UsedPercent)
	if errRemaining != nil {
		return errRemaining
	}
	minutes := windowMinutes(window)
	weekly := defaultWeekly
	if minutes > 0 {
		weekly = minutes >= weeklyWindowMinutes-1
	}
	resetAt := windowResetAt(window, now)
	if weekly {
		if !snapshot.WeeklyWindowPresent || remaining < snapshot.WeeklyRemaining {
			snapshot.WeeklyRemaining = remaining
			snapshot.WeeklyResetAt = resetAt
			snapshot.WeeklyWindowMinutes = minutes
		}
		snapshot.WeeklyWindowPresent = true
		return nil
	}
	if !snapshot.FiveHourWindowPresent || remaining < snapshot.FiveHourRemaining {
		snapshot.FiveHourRemaining = remaining
		snapshot.FiveHourResetAt = resetAt
		snapshot.FiveHourWindowMinutes = minutes
	}
	snapshot.FiveHourWindowPresent = true
	return nil
}

func remainingPercent(used *float64) (int, error) {
	if used == nil {
		return 0, fmt.Errorf("used_percent is missing")
	}
	if math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 || *used > 100 {
		return 0, fmt.Errorf("used_percent %.2f is outside 0-100", *used)
	}
	return clampPercent(int(math.Round(100 - *used))), nil
}

func windowMinutes(window usageWindow) int {
	if window.LimitWindowMinutes > 0 {
		return window.LimitWindowMinutes
	}
	if window.LimitWindowSeconds > 0 {
		return (window.LimitWindowSeconds + 59) / 60
	}
	return 0
}

func windowResetAt(window usageWindow, now time.Time) time.Time {
	if len(window.ResetAt) > 0 && string(window.ResetAt) != "null" {
		var unixSeconds float64
		if errNumber := json.Unmarshal(window.ResetAt, &unixSeconds); errNumber == nil && unixSeconds > 0 {
			return time.Unix(int64(unixSeconds), 0).UTC()
		}
		var raw string
		if errString := json.Unmarshal(window.ResetAt, &raw); errString == nil {
			if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
				return parsed.UTC()
			}
		}
	}
	if window.ResetAfterSeconds > 0 {
		return now.Add(time.Duration(window.ResetAfterSeconds) * time.Second).UTC()
	}
	return time.Time{}
}

func extractCredential(raw json.RawMessage) (credential, error) {
	var value map[string]any
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		return credential{}, fmt.Errorf("decode credential JSON: %w", errUnmarshal)
	}
	accessToken := stringField(value, "access_token")
	accountID := firstStringField(value, "account_id", "chatgpt_account_id")
	for _, key := range []string{"token", "tokens"} {
		nested, ok := value[key].(map[string]any)
		if !ok {
			continue
		}
		if accessToken == "" {
			accessToken = stringField(nested, "access_token")
		}
		if accountID == "" {
			accountID = firstStringField(nested, "account_id", "chatgpt_account_id")
		}
	}
	if accessToken == "" {
		return credential{}, fmt.Errorf("credential access token is missing")
	}
	return credential{AccessToken: accessToken, AccountID: accountID}, nil
}

func firstStringField(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result := stringField(value, key); result != "" {
			return result
		}
	}
	return ""
}

func stringField(value map[string]any, key string) string {
	raw, _ := value[key].(string)
	return strings.TrimSpace(raw)
}
