package core

import (
	"errors"
	"testing"
)

func testRules(t *testing.T) *SubdomainRules {
	t.Helper()
	r, err := NewSubdomainRules(3, 63, `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`,
		[]string{"www", "API"}, 10, "abcdefghjkmnpqrstuvwxyz23456789")
	if err != nil {
		t.Fatalf("NewSubdomainRules: %v", err)
	}
	return r
}

func TestSubdomainValidate(t *testing.T) {
	r := testRules(t)
	cases := []struct {
		in   string
		want error
	}{
		{"demo", nil},
		{"my-app-1", nil},
		{"a1b", nil},
		{"ab", ErrSubdomainInvalid},             // too short
		{"-lead", ErrSubdomainInvalid},          // leading hyphen
		{"trail-", ErrSubdomainInvalid},         // trailing hyphen
		{"Upper", ErrSubdomainInvalid},          // caller must normalize first
		{"has_underscore", ErrSubdomainInvalid}, // not a legal DNS label
		{"a.b", ErrSubdomainInvalid},            // dots would nest a zone
		{"www", ErrSubdomainReserved},           // blocklisted
		{"api", ErrSubdomainReserved},           // blocklist is case-folded
	}
	for _, tc := range cases {
		err := r.Validate(tc.in)
		if tc.want == nil && err != nil {
			t.Errorf("Validate(%q) = %v, want nil", tc.in, err)
			continue
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("Validate(%q) = %v, want %v", tc.in, err, tc.want)
		}
	}
}

func TestSubdomainMaxLengthGuardsDNSLabelLimit(t *testing.T) {
	if _, err := NewSubdomainRules(3, 64, `^[a-z]+$`, nil, 10, "abc"); err == nil {
		t.Fatal("expected 64-octet max length to be rejected as an illegal DNS label")
	}
}

// A rule set whose generated labels fail its own validator would hand clients
// subdomains the server then refuses.
func TestNewSubdomainRulesRejectsUngeneratableAlphabet(t *testing.T) {
	if _, err := NewSubdomainRules(3, 20, `^[a-z]+$`, nil, 10, "0123456789"); err == nil {
		t.Fatal("expected digit-only alphabet to be rejected against a letters-only pattern")
	}
}

func TestGenerateSubdomainSatisfiesRules(t *testing.T) {
	r := testRules(t)
	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		s, err := r.GenerateSubdomain()
		if err != nil {
			t.Fatalf("GenerateSubdomain: %v", err)
		}
		if err := r.Validate(s); err != nil {
			t.Fatalf("generated %q fails own rules: %v", s, err)
		}
		seen[s] = struct{}{}
	}
	if len(seen) < 190 {
		t.Fatalf("only %d/200 generated subdomains were unique; entropy looks broken", len(seen))
	}
}

func TestSubdomainFromHost(t *testing.T) {
	const base = "tunl.siliconcolony.dev"
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"demo.tunl.siliconcolony.dev", "demo", true},
		{"demo.tunl.siliconcolony.dev:443", "demo", true},
		{"DEMO.TUNL.SiliconColony.dev", "demo", true},
		{"demo.tunl.siliconcolony.dev.", "demo", true}, // trailing root dot
		{"tunl.siliconcolony.dev", "", false},          // apex has no label
		{"a.b.tunl.siliconcolony.dev", "", false},      // nested label is not a tunnel
		{"demo.evil.dev", "", false},                   // wrong base domain
		{"eviltunl.siliconcolony.dev", "", false},      // suffix must be dot-anchored
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := SubdomainFromHost(tc.host, base)
		if ok != tc.ok || got != tc.want {
			t.Errorf("SubdomainFromHost(%q) = (%q,%v), want (%q,%v)", tc.host, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHostname(t *testing.T) {
	if got := Hostname("demo", "tunl.example.com"); got != "demo.tunl.example.com" {
		t.Fatalf("Hostname = %q", got)
	}
}
