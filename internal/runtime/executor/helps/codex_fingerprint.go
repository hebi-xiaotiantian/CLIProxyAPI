package helps

import (
	"strings"

	"github.com/google/uuid"
)

// CodexFingerprintMode controls per-account Codex device/session fingerprint
// convergence for shared OAuth credentials.
//
// When several users share one Codex OAuth account, every client carries its
// own installation/session/thread identifiers and the upstream provider counts
// devices and sessions from them. Convergence rewrites those identifiers to
// account-level constants so the upstream sees a stable, normal-looking device
// profile instead of many devices piling onto one account.
type CodexFingerprintMode string

const (
	// CodexFingerprintOff disables fingerprint convergence for an account.
	CodexFingerprintOff CodexFingerprintMode = "off"
	// CodexFingerprintDevice converges the installation identifier to one
	// account-level constant. The upstream sees one device with multiple
	// sessions (each client keeps its own conversation session).
	CodexFingerprintDevice CodexFingerprintMode = "device"
	// CodexFingerprintSession converges the installation and session
	// identifiers to account-level constants, while every real client session
	// still derives its own thread. The upstream sees one device, one session
	// and multiple threads, which matches a normal user spawning sub-agents.
	CodexFingerprintSession CodexFingerprintMode = "session"
	// CodexFingerprintFull converges installation, session and thread
	// identifiers to account-level constants. The upstream sees one device,
	// one session and one thread; this is the most aggressive mode.
	CodexFingerprintFull CodexFingerprintMode = "full"
)

// Auth file metadata keys controlling Codex fingerprint convergence.
// Underscore keys are canonical; hyphen variants are accepted as aliases.
const (
	CodexFingerprintModeMetadataKey           = "codex_fingerprint_mode"
	CodexFingerprintSeedMetadataKey           = "codex_fingerprint_seed"
	CodexFingerprintInstallationIDMetadataKey = "codex_fingerprint_installation_id"
)

// CodexFingerprintConfig is the resolved per-account convergence settings.
type CodexFingerprintConfig struct {
	// Mode is the convergence mode; empty means the account did not opt in
	// and the legacy per-client-value confusion applies.
	Mode CodexFingerprintMode
	// Seed is the account-level UUID all converged identifiers derive from.
	Seed string
	// InstallationID is the converged installation identifier.
	InstallationID string
	// SessionID is the converged session identifier (session/full modes only).
	SessionID string
	// ThreadID is the converged thread identifier (session/full modes only).
	ThreadID string
}

// ParseCodexFingerprintMode normalizes a metadata value into a mode.
// Invalid, empty or non-string values resolve to an empty mode, which means
// "no explicit opt-in".
func ParseCodexFingerprintMode(value any) CodexFingerprintMode {
	raw, _ := value.(string)
	normalized := CodexFingerprintMode(strings.ToLower(strings.TrimSpace(raw)))
	switch normalized {
	case CodexFingerprintOff, CodexFingerprintDevice, CodexFingerprintSession, CodexFingerprintFull:
		return normalized
	default:
		return ""
	}
}

// metadataStringValue reads a metadata key, accepting underscore and hyphen
// spellings of the same key.
func metadataStringValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		raw, ok = metadata[strings.ReplaceAll(key, "_", "-")].(string)
		if !ok {
			return ""
		}
	}
	return strings.TrimSpace(raw)
}

// DeriveCodexFingerprintUUID deterministically derives a stable UUID from a
// seed/name pair. The same inputs always produce the same identifier.
func DeriveCodexFingerprintUUID(kind string, name string) string {
	key := strings.Join([]string{"cli-proxy-api", "codex", "fingerprint", kind, strings.TrimSpace(name)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

// CodexFingerprintThreadID derives the converged thread identifier for a real
// client session (session mode): one distinct thread per client session, all
// stable across requests. Falls back to the account thread when the client
// session identifier is unknown.
func CodexFingerprintThreadID(seed string, clientSessionID string) string {
	if strings.TrimSpace(seed) == "" {
		return ""
	}
	if strings.TrimSpace(clientSessionID) == "" {
		return DeriveCodexFingerprintUUID("thread", seed)
	}
	return DeriveCodexFingerprintUUID("thread", seed+":"+strings.TrimSpace(clientSessionID))
}

// ResolveCodexFingerprintConfig computes the converged identifiers for one
// account from its auth file metadata. A valid explicit UUID seed is used
// verbatim; otherwise the seed is stably derived from the auth ID. An explicit
// installation identifier wins over the derived one, mirroring the
// "real device id" override in other gateway implementations.
//
// Convergence is strictly opt-in: accounts without codex_fingerprint_mode get
// an empty mode, and callers must keep their legacy behavior in that case.
func ResolveCodexFingerprintConfig(metadata map[string]any, authID string) CodexFingerprintConfig {
	return resolveCodexFingerprintConfigWithMode(metadata, authID, ParseCodexFingerprintMode(metadataStringValue(metadata, CodexFingerprintModeMetadataKey)))
}

// ResolveCodexFingerprintConfigWithMode is like ResolveCodexFingerprintConfig
// but forces the convergence mode instead of reading it from metadata. It backs
// the global codex.fingerprint-mode config switch, which acts as the default
// for accounts without an explicit codex_fingerprint_mode.
func ResolveCodexFingerprintConfigWithMode(metadata map[string]any, authID string, mode CodexFingerprintMode) CodexFingerprintConfig {
	return resolveCodexFingerprintConfigWithMode(metadata, authID, mode)
}

func resolveCodexFingerprintConfigWithMode(metadata map[string]any, authID string, mode CodexFingerprintMode) CodexFingerprintConfig {
	if mode == "" || mode == CodexFingerprintOff {
		return CodexFingerprintConfig{Mode: mode}
	}

	seed := metadataStringValue(metadata, CodexFingerprintSeedMetadataKey)
	if parsed, err := uuid.Parse(seed); err != nil || parsed == uuid.Nil {
		seed = DeriveCodexFingerprintUUID("seed", strings.TrimSpace(authID))
	}

	cfg := CodexFingerprintConfig{Mode: mode, Seed: seed}

	if explicit := metadataStringValue(metadata, CodexFingerprintInstallationIDMetadataKey); explicit != "" {
		if parsed, err := uuid.Parse(explicit); err == nil && parsed != uuid.Nil {
			cfg.InstallationID = parsed.String()
		}
	}
	if cfg.InstallationID == "" {
		cfg.InstallationID = DeriveCodexFingerprintUUID("installation", seed)
	}

	if mode == CodexFingerprintSession || mode == CodexFingerprintFull {
		cfg.SessionID = DeriveCodexFingerprintUUID("session", seed)
		if mode == CodexFingerprintFull {
			cfg.ThreadID = cfg.SessionID
		}
	}
	return cfg
}
