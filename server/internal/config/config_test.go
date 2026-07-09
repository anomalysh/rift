package config

import (
	"strings"
	"testing"
)

// setMinimalEnv provides the values Load requires, so each test can vary one
// thing at a time.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv(KeyPostgresDSN, "postgres://u:p@localhost:5432/rift?sslmode=disable")
	t.Setenv(KeyBaseDomain, "rift.example.com")
	t.Setenv(KeyAdminToken, strings.Repeat("a", 40))
}

func TestTLSModeDefaultsToInternalInDevelopment(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeyEnv, EnvDevelopment)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.Mode != TLSModeInternal {
		t.Fatalf("TLS mode = %q, want %q", cfg.TLS.Mode, TLSModeInternal)
	}
}

// An unset mode in production must not fall back to an untrusted certificate:
// the handshake would succeed and nobody would notice for months.
func TestTLSModeIsRequiredInProduction(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeyEnv, EnvProduction)

	_, err := Load()
	if err == nil {
		t.Fatal("expected production boot to fail with no TLS mode set")
	}
	if !strings.Contains(err.Error(), KeyTLSMode) {
		t.Fatalf("error should name %s, got: %v", KeyTLSMode, err)
	}
}

func TestTLSModeRejectsUnknownValue(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeyTLSMode, "letsencrypt")

	_, err := Load()
	if err == nil {
		t.Fatal("expected an unknown TLS mode to be rejected")
	}
	for _, m := range TLSModes {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error should list valid mode %q, got: %v", m, err)
		}
	}
}

func TestDNS01RequiresAProvider(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeyTLSMode, TLSModeDNS01)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), KeyACMEDNSProvider) {
		t.Fatalf("dns01 without a provider should fail naming %s, got: %v", KeyACMEDNSProvider, err)
	}

	t.Setenv(KeyACMEDNSProvider, "rfc2136")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("dns01 with a provider: %v", err)
	}
	if !cfg.TLS.PubliclyTrusted() || !cfg.TLS.CoversUnknownSubdomains() {
		t.Fatal("dns01 should be publicly trusted and cover unknown subdomains")
	}
}

func TestSelfModeRequiresCertAndKey(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeyTLSMode, TLSModeSelf)

	err := mustFail(t)
	if !strings.Contains(err.Error(), KeyTLSCertFile) || !strings.Contains(err.Error(), KeyTLSKeyFile) {
		t.Fatalf("both %s and %s should be reported, got: %v", KeyTLSCertFile, KeyTLSKeyFile, err)
	}

	t.Setenv(KeyTLSCertFile, "/certs/fullchain.pem")
	t.Setenv(KeyTLSKeyFile, "/certs/key.pem")
	if _, err := Load(); err != nil {
		t.Fatalf("self mode with cert and key: %v", err)
	}
}

// http01 is the only mode that cannot serve a hostname which has never had a
// tunnel; that property is what the ingress and the docs rely on.
func TestHTTP01DoesNotCoverUnknownSubdomains(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeyTLSMode, TLSModeHTTP01)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TLS.PubliclyTrusted() {
		t.Fatal("http01 certificates are publicly trusted")
	}
	if cfg.TLS.CoversUnknownSubdomains() {
		t.Fatal("http01 cannot cover a subdomain that never had a tunnel")
	}
}

func TestUntrustedModeInProductionWarnsButBoots(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeyEnv, EnvProduction)
	t.Setenv(KeyTLSMode, TLSModeInternal)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("internal mode in production should boot: %v", err)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("expected a warning that internal certificates are not publicly trusted")
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, KeyTLSMode) {
			found = true
		}
	}
	if !found {
		t.Fatalf("warning should name %s, got %v", KeyTLSMode, cfg.Warnings)
	}
}

// A missing required value must be reported, not silently zeroed.
func TestRequiredValuesAreReported(t *testing.T) {
	t.Setenv(KeyBaseDomain, "rift.example.com")
	t.Setenv(KeyAdminToken, strings.Repeat("a", 40))

	err := mustFail(t)
	if !strings.Contains(err.Error(), KeyPostgresDSN) {
		t.Fatalf("error should name %s, got: %v", KeyPostgresDSN, err)
	}
}

// Load accumulates problems rather than failing on the first one.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv(KeyEnv, "staging")
	t.Setenv(KeyBaseDomain, "not-a-domain")
	t.Setenv(KeyPublicScheme, "ftp")

	err := mustFail(t)
	for _, key := range []string{KeyEnv, KeyBaseDomain, KeyPublicScheme, KeyPostgresDSN} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s; got:\n%v", key, err)
		}
	}
}

func mustFail(t *testing.T) error {
	t.Helper()
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail")
	}
	return err
}

// The built-in blocklist is a safety floor. An operator adding one label of
// their own must not thereby unblock `gateway`, which Caddy routes to the agent
// endpoint, or `api` and `www`, which look official.
func TestSubdomainBlocklistExtendsRatherThanReplaces(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeySubdomainBlocklist, "internal-only, staging2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, label := range []string{"gateway", "api", "www", "rift", "internal-only", "staging2"} {
		if !cfg.SubdomainRules.IsBlocked(label) {
			t.Errorf("%q should be blocked; the env list must add to the defaults, not replace them", label)
		}
	}
}

func TestSubdomainBlocklistDeduplicates(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv(KeySubdomainBlocklist, "gateway,gateway,newone")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.SubdomainRules.IsBlocked("gateway") || !cfg.SubdomainRules.IsBlocked("newone") {
		t.Fatal("expected both the duplicated default and the new label to be blocked")
	}
}
