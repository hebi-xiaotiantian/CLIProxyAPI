package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginID = "codex-account-pool"

type rawConfig struct {
	StateDir              string   `yaml:"state_dir"`
	RefreshInterval       string   `yaml:"refresh_interval"`
	RefreshConcurrency    int      `yaml:"refresh_concurrency"`
	SnapshotMaxAge        string   `yaml:"snapshot_max_age"`
	StalePolicy           string   `yaml:"stale_policy"`
	AffinityTTL           string   `yaml:"affinity_ttl"`
	QuotaURL              string   `yaml:"quota_url"`
	FreeReserveDefault    *int     `yaml:"free_reserve_default"`
	PaidReserveDefault    *int     `yaml:"paid_reserve_default"`
	UnknownReserveDefault *int     `yaml:"unknown_reserve_default"`
	CustomPlanOrder       []string `yaml:"custom_plan_order"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler     bool `json:"scheduler"`
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

func decodeConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	var input rawConfig
	if errUnmarshal := yaml.Unmarshal(raw, &input); errUnmarshal != nil {
		return Config{}, fmt.Errorf("decode plugin config: %w", errUnmarshal)
	}
	if value := strings.TrimSpace(input.StateDir); value != "" {
		cfg.StateDir = value
	}
	var err error
	if cfg.RefreshInterval, err = parseOptionalDuration(input.RefreshInterval, cfg.RefreshInterval, "refresh_interval"); err != nil {
		return Config{}, err
	}
	if cfg.SnapshotMaxAge, err = parseOptionalDuration(input.SnapshotMaxAge, cfg.SnapshotMaxAge, "snapshot_max_age"); err != nil {
		return Config{}, err
	}
	if cfg.AffinityTTL, err = parseOptionalDuration(input.AffinityTTL, cfg.AffinityTTL, "affinity_ttl"); err != nil {
		return Config{}, err
	}
	if input.RefreshConcurrency != 0 {
		cfg.RefreshConcurrency = input.RefreshConcurrency
	}
	if cfg.RefreshConcurrency < 1 || cfg.RefreshConcurrency > 32 {
		return Config{}, fmt.Errorf("refresh_concurrency must be between 1 and 32")
	}
	if value := strings.ToLower(strings.TrimSpace(input.StalePolicy)); value != "" {
		cfg.StalePolicy = StalePolicy(value)
	}
	if cfg.StalePolicy != StaleExclude && cfg.StalePolicy != StaleAllow {
		return Config{}, fmt.Errorf("stale_policy must be exclude or allow")
	}
	if value := strings.TrimSpace(input.QuotaURL); value != "" {
		cfg.QuotaURL = value
	}
	if !strings.HasPrefix(cfg.QuotaURL, "https://") && !strings.HasPrefix(cfg.QuotaURL, "http://") {
		return Config{}, fmt.Errorf("quota_url must be an absolute HTTP URL")
	}
	if input.FreeReserveDefault != nil {
		cfg.FreeReserveDefault = *input.FreeReserveDefault
	}
	if input.PaidReserveDefault != nil {
		cfg.PaidReserveDefault = *input.PaidReserveDefault
	}
	if input.UnknownReserveDefault != nil {
		cfg.UnknownReserveDefault = *input.UnknownReserveDefault
	}
	if len(input.CustomPlanOrder) > 0 {
		order := make([]PlanKind, 0, len(input.CustomPlanOrder))
		for _, rawPlan := range input.CustomPlanOrder {
			order = append(order, PlanKind(strings.ToLower(strings.TrimSpace(rawPlan))))
		}
		var errOrder error
		cfg.CustomPlanOrder, errOrder = normalizePlanOrder(order)
		if errOrder != nil {
			return Config{}, errOrder
		}
	}
	for name, value := range map[string]int{
		"free_reserve_default":    cfg.FreeReserveDefault,
		"paid_reserve_default":    cfg.PaidReserveDefault,
		"unknown_reserve_default": cfg.UnknownReserveDefault,
	} {
		if value < 0 || value > 100 {
			return Config{}, fmt.Errorf("%s must be between 0 and 100", name)
		}
	}
	return cfg, nil
}

func parseOptionalDuration(raw string, fallback time.Duration, name string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback, nil
	}
	parsed, errParse := time.ParseDuration(value)
	if errParse != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          "0.1.0",
			Author:           "hebi-xiaotiantian",
			GitHubRepository: "https://github.com/hebi-xiaotiantian/CLIProxyAPI",
			Logo:             "https://raw.githubusercontent.com/router-for-me/CLIProxyAPI/main/docs/logo.png",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "state_dir", Type: pluginapi.ConfigFieldTypeString, Description: "Directory for account policy and quota state."},
				{Name: "refresh_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Automatic quota refresh interval."},
				{Name: "refresh_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum concurrent quota refresh requests."},
				{Name: "snapshot_max_age", Type: pluginapi.ConfigFieldTypeString, Description: "Maximum usable quota snapshot age."},
				{Name: "stale_policy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{string(StaleExclude), string(StaleAllow)}, Description: "Routing behavior for missing or stale quota snapshots."},
				{Name: "affinity_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "Session affinity lifetime."},
			},
		},
		Capabilities: registrationCapabilities{
			Scheduler:     true,
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}
