package core

import (
	"strings"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"app.acme.com", "app.acme.com"},
		{"  App.Acme.COM. ", "app.acme.com"}, // trim, lower, trailing dot
		{"app.acme.com:8443", "app.acme.com"},
		{"a.b.c.example.org", "a.b.c.example.org"},
		{"localhost", ""},            // single label is not a valid custom domain
		{"", ""},                     // empty
		{"has space.com", ""},        // spaces are invalid
		{"under_score.com", ""},      // underscore is not a host char
		{"a..b.com", ""},             // empty label
		{".leading.com", ""},         // leading dot -> empty label
		{"https://app.acme.com", ""}, // a URL, not a host
		// RFC 1035 shape.
		{"-app.acme.com", ""},                              // leading hyphen in a label
		{"app-.acme.com", ""},                              // trailing hyphen in a label
		{"app.-acme.com", ""},                              // hyphen at a label start mid-name
		{"xn--bcher-kva.example", "xn--bcher-kva.example"}, // punycode keeps inner hyphens
		{strings.Repeat("a", 63) + ".com", strings.Repeat("a", 63) + ".com"},   // 63-char label is the max
		{strings.Repeat("a", 64) + ".com", ""},                                 // 64-char label is too long
		{strings.Repeat("a.", 126) + "com", ""},                                // 255 chars overall
		{strings.Repeat("a.", 125) + "com", strings.Repeat("a.", 125) + "com"}, // 253 chars is the max
		{"10.0.0.1", ""},             // an IPv4 literal is not a domain
		{"app.acme.123", ""},         // an all-numeric TLD does not exist
		{"1.acme.com", "1.acme.com"}, // numeric non-final labels are fine
	}
	for _, c := range cases {
		if got := NormalizeDomain(c.in); got != c.want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsServerHostname(t *testing.T) {
	const base, gw = "rift.test", "tunnel.example.org"
	cases := []struct {
		host string
		want bool
	}{
		{"rift.test", true},           // the apex
		{"app.rift.test", true},       // a subdomain
		{"a.b.rift.test", true},       // a multi-label name under the base
		{"tunnel.example.org", true},  // the gateway hostname, outside the base
		{"evilrift.test", false},      // shares a suffix but not a label boundary
		{"app.acme.com", false},       // a genuine custom domain
		{"rift.test.acme.com", false}, // the base as a prefix is not under it
	}
	for _, c := range cases {
		if got := IsServerHostname(c.host, base, gw); got != c.want {
			t.Errorf("IsServerHostname(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if IsServerHostname("anything.example", base, "") {
		t.Error("an empty gateway hostname must not match")
	}
}
