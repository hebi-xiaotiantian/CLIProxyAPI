package helps

import (
	"testing"

	"github.com/google/uuid"
)

func TestParseCodexFingerprintMode(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  CodexFingerprintMode
	}{
		{name: "device", value: "device", want: CodexFingerprintDevice},
		{name: "session", value: "session", want: CodexFingerprintSession},
		{name: "full", value: "full", want: CodexFingerprintFull},
		{name: "off", value: "off", want: CodexFingerprintOff},
		{name: "upper case", value: "DEVICE", want: CodexFingerprintDevice},
		{name: "spaces", value: "  full  ", want: CodexFingerprintFull},
		{name: "empty string", value: "", want: ""},
		{name: "nil", value: nil, want: ""},
		{name: "non-string", value: 42, want: ""},
		{name: "unknown", value: "aggressive", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCodexFingerprintMode(tc.value); got != tc.want {
				t.Fatalf("ParseCodexFingerprintMode(%v) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestDeriveCodexFingerprintUUIDStable(t *testing.T) {
	first := DeriveCodexFingerprintUUID("installation", "seed-1")
	second := DeriveCodexFingerprintUUID("installation", "seed-1")
	if first != second {
		t.Fatalf("derivation not stable: %q != %q", first, second)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("derived value %q is not a valid UUID: %v", first, err)
	}
	if DeriveCodexFingerprintUUID("installation", "seed-1") == DeriveCodexFingerprintUUID("session", "seed-1") {
		t.Fatal("different kinds must derive different identifiers")
	}
	if DeriveCodexFingerprintUUID("installation", "seed-1") == DeriveCodexFingerprintUUID("installation", "seed-2") {
		t.Fatal("different seeds must derive different identifiers")
	}
}

func TestCodexFingerprintThreadID(t *testing.T) {
	if got := CodexFingerprintThreadID("", "client-session"); got != "" {
		t.Fatalf("empty seed must yield empty thread, got %q", got)
	}
	first := CodexFingerprintThreadID("seed-1", "client-a")
	if first == "" {
		t.Fatal("expected a thread id")
	}
	if got := CodexFingerprintThreadID("seed-1", "client-a"); got != first {
		t.Fatalf("thread id not stable per client session: %q != %q", got, first)
	}
	if got := CodexFingerprintThreadID("seed-1", "client-b"); got == first {
		t.Fatal("different client sessions must derive different threads")
	}
	if got := CodexFingerprintThreadID("seed-2", "client-a"); got == first {
		t.Fatal("different seeds must derive different threads")
	}
	fallback := CodexFingerprintThreadID("seed-1", "")
	if fallback == "" {
		t.Fatal("expected a fallback thread id")
	}
	if got := CodexFingerprintThreadID("seed-1", " "); got != fallback {
		t.Fatalf("blank client session must use the fallback thread, got %q", got)
	}
}

func TestResolveCodexFingerprintConfigUnsetIsEmpty(t *testing.T) {
	cfg := ResolveCodexFingerprintConfig(nil, "auth-1")
	if cfg.Mode != "" {
		t.Fatalf("nil metadata mode = %q, want empty", cfg.Mode)
	}
	cfg = ResolveCodexFingerprintConfig(map[string]any{}, "auth-1")
	if cfg.Mode != "" {
		t.Fatalf("empty metadata mode = %q, want empty", cfg.Mode)
	}
	cfg = ResolveCodexFingerprintConfig(map[string]any{"codex_fingerprint_mode": "bogus"}, "auth-1")
	if cfg.Mode != "" {
		t.Fatalf("invalid mode = %q, want empty", cfg.Mode)
	}
}

func TestResolveCodexFingerprintConfigOff(t *testing.T) {
	cfg := ResolveCodexFingerprintConfig(map[string]any{"codex_fingerprint_mode": "off"}, "auth-1")
	if cfg.Mode != CodexFingerprintOff {
		t.Fatalf("mode = %q, want off", cfg.Mode)
	}
	if cfg.InstallationID != "" || cfg.SessionID != "" || cfg.ThreadID != "" {
		t.Fatalf("off mode must not resolve identifiers: %+v", cfg)
	}
}

func TestResolveCodexFingerprintConfigDevice(t *testing.T) {
	cfg := ResolveCodexFingerprintConfig(map[string]any{"codex_fingerprint_mode": "device"}, "auth-1")
	if cfg.Mode != CodexFingerprintDevice {
		t.Fatalf("mode = %q, want device", cfg.Mode)
	}
	if cfg.InstallationID == "" {
		t.Fatal("device mode must resolve an installation id")
	}
	if cfg.SessionID != "" || cfg.ThreadID != "" {
		t.Fatalf("device mode must not resolve session/thread: %+v", cfg)
	}
	if _, err := uuid.Parse(cfg.InstallationID); err != nil {
		t.Fatalf("installation id %q is not a valid UUID: %v", cfg.InstallationID, err)
	}
	// Same auth id, same installation id.
	again := ResolveCodexFingerprintConfig(map[string]any{"codex_fingerprint_mode": "device"}, "auth-1")
	if again.InstallationID != cfg.InstallationID {
		t.Fatalf("installation id not stable: %q != %q", again.InstallationID, cfg.InstallationID)
	}
	// Different auth ids, different installation ids.
	other := ResolveCodexFingerprintConfig(map[string]any{"codex_fingerprint_mode": "device"}, "auth-2")
	if other.InstallationID == cfg.InstallationID {
		t.Fatal("different auth ids must resolve different installation ids")
	}
}

func TestResolveCodexFingerprintConfigExplicitSeedAndInstallation(t *testing.T) {
	seed := uuid.NewString()
	cfg := ResolveCodexFingerprintConfig(map[string]any{
		"codex_fingerprint_mode":            "full",
		"codex_fingerprint_seed":            seed,
		"codex_fingerprint_installation_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	}, "auth-1")
	if cfg.Seed != seed {
		t.Fatalf("seed = %q, want %q", cfg.Seed, seed)
	}
	if cfg.InstallationID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Fatalf("installation id = %q, want explicit value", cfg.InstallationID)
	}
	if cfg.SessionID == "" || cfg.ThreadID == "" {
		t.Fatalf("full mode must resolve session and thread: %+v", cfg)
	}
	if cfg.ThreadID != cfg.SessionID {
		t.Fatalf("full mode thread = %q, want session %q", cfg.ThreadID, cfg.SessionID)
	}
	// Hyphen variant keys are accepted too.
	hyphen := ResolveCodexFingerprintConfig(map[string]any{
		"codex-fingerprint-mode": "session",
		"codex-fingerprint-seed": seed,
	}, "auth-1")
	if hyphen.Mode != CodexFingerprintSession {
		t.Fatalf("hyphen key mode = %q, want session", hyphen.Mode)
	}
	if hyphen.SessionID != cfg.SessionID {
		t.Fatalf("hyphen key session = %q, want %q", hyphen.SessionID, cfg.SessionID)
	}
	// Invalid seed falls back to auth-id derivation.
	badSeed := ResolveCodexFingerprintConfig(map[string]any{
		"codex_fingerprint_mode": "device",
		"codex_fingerprint_seed": "not-a-uuid",
	}, "auth-1")
	if badSeed.Seed == "not-a-uuid" {
		t.Fatal("invalid seed must fall back to auth-id derivation")
	}
	if badSeed.Seed == "" {
		t.Fatal("fallback seed must not be empty")
	}
}

func TestResolveCodexFingerprintConfigSession(t *testing.T) {
	cfg := ResolveCodexFingerprintConfig(map[string]any{"codex_fingerprint_mode": "session"}, "auth-1")
	if cfg.Mode != CodexFingerprintSession {
		t.Fatalf("mode = %q, want session", cfg.Mode)
	}
	if cfg.InstallationID == "" || cfg.SessionID == "" {
		t.Fatalf("session mode must resolve installation and session: %+v", cfg)
	}
	if cfg.ThreadID != "" {
		t.Fatalf("session mode thread must be resolved per client session at request time, got %q", cfg.ThreadID)
	}
	if cfg.ThreadID == cfg.SessionID {
		t.Fatal("session mode must not pre-converge the thread")
	}
}
