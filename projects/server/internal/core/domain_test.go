package core

import "testing"

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
	}
	for _, c := range cases {
		if got := NormalizeDomain(c.in); got != c.want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
