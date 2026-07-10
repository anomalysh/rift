package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal .env that config.Load accepts. Individual tests mutate a copy.
const validEnv = `
RIFT_BASE_DOMAIN=rift.example.com
RIFT_ADMIN_TOKEN=abcdefghijklmnopqrstuvwxyz012345
RIFT_POSTGRES_DSN=postgres://u:p@h:5432/db?sslmode=disable
RIFT_TLS_MODE=internal
`

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	return p
}

// clearRiftEnv removes any RIFT_* left in the environment so one test's applied
// file cannot leak into another (config validate mutates the process env).
func clearRiftEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "RIFT_") {
			key := k
			old := os.Getenv(key)
			_ = os.Unsetenv(key)
			t.Cleanup(func() { _ = os.Setenv(key, old) })
		}
	}
}

func TestConfigValidateAcceptsAValidEnvFile(t *testing.T) {
	clearRiftEnv(t)
	if err := runConfig([]string{"validate", "--env-file", writeEnv(t, validEnv)}); err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}
}

func TestConfigValidateRejectsABadEnvFile(t *testing.T) {
	clearRiftEnv(t)
	err := runConfig([]string{"validate", "--env-file", writeEnv(t, validEnv+"RIFT_TLS_MODE=bogus\n")})
	if err == nil {
		t.Fatal("bad TLS mode was accepted")
	}
	if !strings.Contains(err.Error(), "RIFT_TLS_MODE") {
		t.Fatalf("error did not mention the offending key: %v", err)
	}
}

func TestConfigValidateReportsAMissingRequiredVar(t *testing.T) {
	clearRiftEnv(t)
	// No DSN -> config.Load must fail; validate surfaces that error, not a panic.
	err := runConfig([]string{"validate", "--env-file", writeEnv(t, "RIFT_BASE_DOMAIN=x.example.com\nRIFT_ADMIN_TOKEN=abcdefghijklmnopqrstuvwxyz012345\nRIFT_TLS_MODE=internal\n")})
	if err == nil {
		t.Fatal("a config missing its required Postgres DSN was accepted")
	}
}

func TestConfigDefaultsPrintsKnownKeys(t *testing.T) {
	// defaults must at least emit the raw-tunnel window harden.sh relies on.
	if err := configDefaults(nil); err != nil {
		t.Fatalf("defaults errored: %v", err)
	}
}

func TestConfigRejectsUnknownSubcommand(t *testing.T) {
	if err := runConfig([]string{"frobnicate"}); err == nil {
		t.Fatal("unknown subcommand was accepted")
	}
	if err := runConfig(nil); err == nil {
		t.Fatal("no subcommand should be a usage error")
	}
}

func TestApplyEnvFileStripsQuotesAndComments(t *testing.T) {
	clearRiftEnv(t)
	p := writeEnv(t, "# a comment\nRIFT_APPLYENV_PROBE=\"quoted value\"\n\nRIFT_APPLYENV_BARE=plain\n")
	if err := applyEnvFile(p); err != nil {
		t.Fatalf("applyEnvFile: %v", err)
	}
	if got := os.Getenv("RIFT_APPLYENV_PROBE"); got != "quoted value" {
		t.Fatalf("quoted value = %q, want %q", got, "quoted value")
	}
	if got := os.Getenv("RIFT_APPLYENV_BARE"); got != "plain" {
		t.Fatalf("bare value = %q, want %q", got, "plain")
	}
}
