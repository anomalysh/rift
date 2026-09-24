package config

import (
	"strings"
	"testing"
	"time"
)

// loadErr runs Load and returns its error text, failing the test when Load
// unexpectedly succeeds.
func loadErr(t *testing.T) string {
	t.Helper()
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail")
	}
	return err.Error()
}

func TestUDPPortRangeIsValidatedLikeTCP(t *testing.T) {
	for name, tc := range map[string]struct{ min, max string }{
		// An inverted range once reached the gateway's port pool, sized
		// max-min+1, and panicked at boot.
		"inverted": {"20300", "20200"},
		// Port 0 would bind an ephemeral port and advertise it as ":0".
		"zero":         {"0", "10"},
		"out of range": {"60000", "70000"},
	} {
		t.Run(name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv(KeyUDPEnabled, "true")
			t.Setenv(KeyUDPPortMin, tc.min)
			t.Setenv(KeyUDPPortMax, tc.max)
			if msg := loadErr(t); !strings.Contains(msg, KeyUDPPortMin) {
				t.Fatalf("error does not name %s: %s", KeyUDPPortMin, msg)
			}
		})
	}

	t.Run("disabled is not checked", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv(KeyUDPPortMin, "20300")
		t.Setenv(KeyUDPPortMax, "20200")
		if _, err := Load(); err != nil {
			t.Fatalf("a disabled listener's range should not block boot: %v", err)
		}
	})
	t.Run("defaults are valid", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv(KeyUDPEnabled, "true")
		t.Setenv(KeyTCPEnabled, "true")
		if _, err := Load(); err != nil {
			t.Fatalf("default port windows rejected: %v", err)
		}
	})
}

func TestGRPCListenerIsValidatedLikeTLSTunnel(t *testing.T) {
	for name, addr := range map[string]string{
		"no port":           "localhost",
		"named port":        ":http", // parses, but advertises port 0
		"port out of range": ":70000",
	} {
		t.Run(name, func(t *testing.T) {
			setMinimalEnv(t)
			t.Setenv(KeyGRPCEnabled, "true")
			t.Setenv(KeyGRPCListenAddr, addr)
			if msg := loadErr(t); !strings.Contains(msg, KeyGRPCListenAddr) {
				t.Fatalf("error does not name %s: %s", KeyGRPCListenAddr, msg)
			}
		})
	}

	t.Run("advertise port rescues a named listen port", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv(KeyGRPCEnabled, "true")
		t.Setenv(KeyGRPCListenAddr, ":http")
		t.Setenv(KeyGRPCAdvertisePort, "443")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := cfg.GRPC.Port(); got != 443 {
			t.Fatalf("GRPC.Port() = %d, want 443", got)
		}
	})
	t.Run("defaults are valid", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv(KeyGRPCEnabled, "true")
		t.Setenv(KeyTLSTunnelEnabled, "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("default listeners rejected: %v", err)
		}
		if cfg.GRPC.Port() != 8090 || cfg.TLSTunnel.Port() != 8443 {
			t.Fatalf("ports = %d/%d, want 8090/8443", cfg.GRPC.Port(), cfg.TLSTunnel.Port())
		}
	})
}

func TestTypedSettingsParse(t *testing.T) {
	t.Run("zero write timeout means no deadline", func(t *testing.T) {
		setMinimalEnv(t)
		t.Setenv(KeyIngressWriteTimeout, "0")
		t.Setenv(KeyIngressReadTimeout, "45s")
		t.Setenv(KeyMaxRequestBodyBytes, "1048576")
		t.Setenv(KeyAdminEnabled, "false")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Ingress.WriteTimeout != 0 || cfg.Ingress.ReadTimeout != 45*time.Second ||
			cfg.Tunnel.MaxRequestBodyBytes != 1<<20 || cfg.Admin.Enabled {
			t.Fatalf("parsed %+v / %+v / admin=%v", cfg.Ingress, cfg.Tunnel, cfg.Admin.Enabled)
		}
	})

	t.Run("every bad value is reported at once", func(t *testing.T) {
		setMinimalEnv(t)
		bad := map[string]string{
			KeyIngressWriteTimeout: "-1s",  // negative optional duration
			KeyIngressReadTimeout:  "0s",   // zero required-positive duration
			KeyReaperInterval:      "soon", // not a duration
			KeyMaxRequestBodyBytes: "32MB", // not an integer
			KeyRedisDB:             "one",
			KeyAdminEnabled:        "maybe",
		}
		for k, v := range bad {
			t.Setenv(k, v)
		}
		msg := loadErr(t)
		for k, v := range bad {
			if !strings.Contains(msg, k+": expected ") || !strings.Contains(msg, `"`+v+`"`) {
				t.Errorf("error does not report %s=%q: %s", k, v, msg)
			}
		}
	})
}
