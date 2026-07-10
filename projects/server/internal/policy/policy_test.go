package policy

import (
	"net"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return string(h)
}

func TestAllowsIP(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		deny  []string
		ip    string
		want  bool
	}{
		{"no rules admits everyone", nil, nil, "203.0.113.9", true},
		{"deny match rejects", nil, []string{"203.0.113.0/24"}, "203.0.113.9", false},
		{"deny miss admits", nil, []string{"203.0.113.0/24"}, "198.51.100.1", true},
		{"allow set defaults to deny", []string{"10.0.0.0/8"}, nil, "203.0.113.9", false},
		{"allow match admits", []string{"10.0.0.0/8"}, nil, "10.1.2.3", true},
		{"deny wins over allow", []string{"10.0.0.0/8"}, []string{"10.9.0.0/16"}, "10.9.1.1", false},
		{"bare ip is a host rule", []string{"10.1.2.3"}, nil, "10.1.2.3", true},
		{"bare ip excludes others", []string{"10.1.2.3"}, nil, "10.1.2.4", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Compile(core.Policy{AllowIPs: tc.allow, DenyIPs: tc.deny})
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if got := c.AllowsIP(net.ParseIP(tc.ip)); got != tc.want {
				t.Fatalf("AllowsIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestAllowsIPFailsClosedOnNil(t *testing.T) {
	c, _ := Compile(core.Policy{DenyIPs: []string{"10.0.0.0/8"}})
	if c.AllowsIP(nil) {
		t.Fatal("a nil (unresolved) IP must be rejected, not admitted")
	}
}

func TestCheckBasicAuth(t *testing.T) {
	c, err := Compile(core.Policy{BasicAuth: []core.BasicAuthCred{
		{User: "alice", Hash: mustHash(t, "s3cret")},
		{User: "bob", Hash: mustHash(t, "hunter2")},
	}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !c.RequiresBasicAuth() {
		t.Fatal("RequiresBasicAuth = false, want true")
	}
	if !c.CheckBasicAuth("alice", "s3cret") {
		t.Fatal("correct credential rejected")
	}
	if !c.CheckBasicAuth("bob", "hunter2") {
		t.Fatal("second credential rejected")
	}
	if c.CheckBasicAuth("alice", "wrong") {
		t.Fatal("wrong password accepted")
	}
	if c.CheckBasicAuth("carol", "s3cret") {
		t.Fatal("unknown user accepted")
	}
}

func TestCompileRejectsBadCIDR(t *testing.T) {
	if err := Validate(core.Policy{AllowIPs: []string{"not-a-cidr"}}); err == nil {
		t.Fatal("a malformed CIDR was accepted")
	}
	if err := Validate(core.Policy{DenyIPs: []string{"10.0.0.0/8", "10.0.0.0/999"}}); err == nil {
		t.Fatal("an out-of-range CIDR was accepted")
	}
}

func TestEmptyAndZero(t *testing.T) {
	c, _ := Compile(core.Policy{})
	if !c.Empty() {
		t.Fatal("a zero policy compiled non-empty")
	}
	if !(core.Policy{}).IsZero() {
		t.Fatal("zero policy IsZero=false")
	}
	if (core.Policy{Once: true}).IsZero() {
		t.Fatal("a policy with a lifetime bound is not zero")
	}
}
