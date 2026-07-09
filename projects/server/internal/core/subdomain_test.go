package core

import (
	"errors"
	"strings"
	"testing"
)

func testRules(t *testing.T) *SubdomainRules {
	t.Helper()
	r, err := NewSubdomainRules(3, 63, `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`,
		[]string{"www", "API"}, GeneratorRandom, 10, "abcdefghjkmnpqrstuvwxyz23456789")
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
	if _, err := NewSubdomainRules(3, 64, `^[a-z]+$`, nil, GeneratorRandom, 10, "abc"); err == nil {
		t.Fatal("expected 64-octet max length to be rejected as an illegal DNS label")
	}
}

// A rule set whose generated labels fail its own validator would hand clients
// subdomains the server then refuses.
func TestNewSubdomainRulesRejectsUngeneratableAlphabet(t *testing.T) {
	if _, err := NewSubdomainRules(3, 20, `^[a-z]+$`, nil, GeneratorRandom, 10, "0123456789"); err == nil {
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
	const base = "rift.anomaly.sh"
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		{"demo.rift.anomaly.sh", "demo", true},
		{"demo.rift.anomaly.sh:443", "demo", true},
		{"DEMO.Rift.Anomaly.SH", "demo", true},
		{"demo.rift.anomaly.sh.", "demo", true}, // trailing root dot
		{"rift.anomaly.sh", "", false},          // apex has no label
		{"a.b.rift.anomaly.sh", "", false},      // nested label is not a tunnel
		{"demo.evil.dev", "", false},            // wrong base domain
		{"evilrift.anomaly.sh", "", false},      // suffix must be dot-anchored
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
	if got := Hostname("demo", "rift.example.com"); got != "demo.rift.example.com" {
		t.Fatalf("Hostname = %q", got)
	}
}

func TestGenerateWordsSubdomain(t *testing.T) {
	r, err := NewSubdomainRules(3, 63, `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`,
		DefaultBlockNothing, GeneratorWords, 10, "abcdefghjkmnpqrstuvwxyz23456789")
	if err != nil {
		t.Fatalf("NewSubdomainRules: %v", err)
	}

	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		s, err := r.GenerateSubdomain()
		if err != nil {
			t.Fatalf("GenerateSubdomain: %v", err)
		}
		if err := r.Validate(s); err != nil {
			t.Fatalf("generated %q fails the rules: %v", s, err)
		}
		// adjective-noun-number
		parts := strings.Split(s, "-")
		if len(parts) != 3 {
			t.Fatalf("expected adjective-noun-number, got %q", s)
		}
		seen[s] = struct{}{}
	}
	if len(seen) < 180 {
		t.Fatalf("only %d/200 unique word subdomains; entropy looks weak", len(seen))
	}
}

// A word pair can exceed a small MaxLength, and the rule set must refuse at
// construction rather than hand out labels it cannot generate.
func TestWordsGeneratorRejectsTooShortMaxLength(t *testing.T) {
	if _, err := NewSubdomainRules(3, 8, `^[a-z0-9-]+$`, nil, GeneratorWords, 6, "abcdef"); err == nil {
		t.Fatal("expected construction to fail when MaxLength cannot fit a word subdomain")
	}
}

func TestRejectsUnknownGenerator(t *testing.T) {
	if _, err := NewSubdomainRules(3, 63, `^[a-z0-9-]+$`, nil, "haiku", 10, "abcdef"); err == nil {
		t.Fatal("expected an unknown generator to be rejected")
	}
}

var DefaultBlockNothing []string
