package policy

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

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

// A hostile agent must not be able to make every visitor request run an
// arbitrarily expensive bcrypt comparison, so the hash cost is bounded at
// connect time.
func TestValidateRejectsExcessiveBcryptCost(t *testing.T) {
	// A syntactically valid cost-31 hash: prefix, cost, then 53 chars of salt
	// and digest. Generating a real one would take days, which is the point.
	cost31 := "$2a$31$" + strings.Repeat("a", 53)
	if c, err := bcrypt.Cost([]byte(cost31)); err != nil || c != 31 {
		t.Fatalf("fixture is not a parseable cost-31 hash: cost=%d err=%v", c, err)
	}
	err := Validate(core.Policy{BasicAuth: []core.BasicAuthCred{{User: "alice", Hash: cost31}}})
	if err == nil {
		t.Fatal("a cost-31 hash was accepted")
	}

	// The upper bound itself is accepted.
	ok := "$2a$" + strconv.Itoa(MaxBcryptCost) + "$" + strings.Repeat("a", 53)
	if err := Validate(core.Policy{BasicAuth: []core.BasicAuthCred{{User: "alice", Hash: ok}}}); err != nil {
		t.Fatalf("a cost-%d hash was rejected: %v", MaxBcryptCost, err)
	}
}

func TestValidateRejectsNonBcryptHash(t *testing.T) {
	for _, h := range []string{"", "plaintext", "$1$abc$def"} {
		if err := Validate(core.Policy{BasicAuth: []core.BasicAuthCred{{User: "alice", Hash: h}}}); err == nil {
			t.Fatalf("non-bcrypt hash %q was accepted", h)
		}
	}
}

func TestValidateBoundsListSizes(t *testing.T) {
	h := mustHash(t, "pw")
	creds := make([]core.BasicAuthCred, MaxBasicAuthCreds+1)
	for i := range creds {
		creds[i] = core.BasicAuthCred{User: "u" + strconv.Itoa(i), Hash: h}
	}
	if err := Validate(core.Policy{BasicAuth: creds}); err == nil {
		t.Fatal("too many basic-auth credentials were accepted")
	}
	if err := Validate(core.Policy{BasicAuth: creds[:MaxBasicAuthCreds]}); err != nil {
		t.Fatalf("credentials at the limit were rejected: %v", err)
	}

	ips := make([]string, MaxIPRules+1)
	for i := range ips {
		ips[i] = "10.0.0.1"
	}
	if err := Validate(core.Policy{AllowIPs: ips}); err == nil {
		t.Fatal("too many allow_ips entries were accepted")
	}
	if err := Validate(core.Policy{DenyIPs: ips}); err == nil {
		t.Fatal("too many deny_ips entries were accepted")
	}
	if err := Validate(core.Policy{AllowIPs: ips[:MaxIPRules]}); err != nil {
		t.Fatalf("allow_ips at the limit were rejected: %v", err)
	}
}

// An unknown user must cost a bcrypt comparison just like a known one, so the
// response time does not reveal which user names are configured.
func TestCheckBasicAuthUnknownUserPaysBcrypt(t *testing.T) {
	// A cost high enough that one comparison (milliseconds) clearly dominates
	// a skipped one (microseconds), yet cheap under the race detector.
	h, err := bcrypt.GenerateFromPassword([]byte("s3cret"), 6)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c, err := Compile(core.Policy{BasicAuth: []core.BasicAuthCred{{User: "alice", Hash: string(h)}}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	_ = c.CheckBasicAuth("warmup", "x") // generate the dummy hash outside the timing

	timed := func(user string) time.Duration {
		start := time.Now()
		_ = c.CheckBasicAuth(user, "wrong")
		return time.Since(start)
	}
	known := timed("alice")
	unknown := timed("mallory")
	// Both run one cost-6 comparison (the dummy matches the configured cost);
	// without the dummy compare the unknown user would return in microseconds.
	if unknown < known/4 {
		t.Fatalf("unknown user took %v vs known %v: the miss skipped bcrypt", unknown, known)
	}
}
