package ingress

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
)

// fakeSession is a core.Session that only carries a Tunnel (with its policy);
// enforce() never proxies, so RoundTrip is unused.
type fakeSession struct{ tunnel core.Tunnel }

func (f fakeSession) Tunnel() core.Tunnel                             { return f.tunnel }
func (f fakeSession) Close(string) error                              { return nil }
func (f fakeSession) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

func enforceTestIngress() *Ingress {
	return &Ingress{
		cfg:      &config.Config{Tunnel: config.Tunnel{PublicScheme: "https"}},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		trusted:  parseTrusted(nil),
		policies: newPolicyCache(),
		limiter:  newRateLimiter(),
	}
}

func bcryptHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func requestFrom(ip string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://app.rift.test/x", nil)
	r.RemoteAddr = ip + ":40000"
	return r
}

func TestEnforceAllowsUnpolicied(t *testing.T) {
	i := enforceTestIngress()
	w := httptest.NewRecorder()
	sess := fakeSession{tunnel: core.Tunnel{ID: "t1"}}
	if !i.enforce(w, requestFrom("203.0.113.9"), sess, "app") {
		t.Fatal("an unpolicied tunnel must admit the request")
	}
}

func TestEnforceIPPolicy(t *testing.T) {
	i := enforceTestIngress()
	sess := fakeSession{tunnel: core.Tunnel{ID: "ip", Policy: core.Policy{
		AllowIPs: []string{"10.0.0.0/8"},
		DenyIPs:  []string{"10.9.0.0/16"},
	}}}

	// In the allow range, not denied -> admitted.
	w := httptest.NewRecorder()
	if !i.enforce(w, requestFrom("10.1.2.3"), sess, "ip") {
		t.Fatal("an allowed IP was blocked")
	}
	// Outside the allow range -> 403.
	w = httptest.NewRecorder()
	if i.enforce(w, requestFrom("203.0.113.9"), sess, "ip") {
		t.Fatal("an out-of-range IP was admitted")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	// In allow but explicitly denied -> 403.
	w = httptest.NewRecorder()
	if i.enforce(w, requestFrom("10.9.1.1"), sess, "ip") {
		t.Fatal("a denied IP was admitted")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403", w.Code)
	}
}

func TestEnforceBasicAuth(t *testing.T) {
	i := enforceTestIngress()
	sess := fakeSession{tunnel: core.Tunnel{ID: "auth", Policy: core.Policy{
		BasicAuth: []core.BasicAuthCred{{User: "alice", Hash: bcryptHash(t, "s3cret")}},
	}}}

	// No credential -> 401 with a challenge.
	w := httptest.NewRecorder()
	if i.enforce(w, requestFrom("203.0.113.9"), sess, "auth") {
		t.Fatal("a request with no credential was admitted")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("401 is missing the WWW-Authenticate challenge")
	}

	// Wrong password -> 401.
	w = httptest.NewRecorder()
	r := requestFrom("203.0.113.9")
	r.SetBasicAuth("alice", "wrong")
	if i.enforce(w, r, sess, "auth") {
		t.Fatal("a wrong password was admitted")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-password status = %d, want 401", w.Code)
	}

	// Correct credential -> admitted.
	w = httptest.NewRecorder()
	r = requestFrom("203.0.113.9")
	r.SetBasicAuth("alice", "s3cret")
	if !i.enforce(w, r, sess, "auth") {
		t.Fatalf("the correct credential was rejected (status %d)", w.Code)
	}
}

func TestEnforceRateLimit(t *testing.T) {
	i := enforceTestIngress()
	// Freeze the clock so no tokens refill mid-test.
	i.limiter.now = func() time.Time { return time.Unix(0, 0) }
	sess := fakeSession{tunnel: core.Tunnel{ID: "rl", Policy: core.Policy{
		RateLimit: &core.RateLimit{RPS: 1, Burst: 2},
	}}}

	// Burst of 2 is admitted, the third is throttled with Retry-After.
	for n := 1; n <= 2; n++ {
		w := httptest.NewRecorder()
		if !i.enforce(w, requestFrom("203.0.113.9"), sess, "rl") {
			t.Fatalf("request %d within the burst was throttled", n)
		}
	}
	w := httptest.NewRecorder()
	if i.enforce(w, requestFrom("203.0.113.9"), sess, "rl") {
		t.Fatal("a request past the burst was admitted")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 is missing the Retry-After header")
	}
}

func TestEnforceRateLimitPerIP(t *testing.T) {
	i := enforceTestIngress()
	i.limiter.now = func() time.Time { return time.Unix(0, 0) }
	sess := fakeSession{tunnel: core.Tunnel{ID: "rlip", Policy: core.Policy{
		RateLimit: &core.RateLimit{RPS: 1, Burst: 1, PerIP: true},
	}}}

	// Each IP gets its own bucket: A exhausts its token, B is still admitted.
	if !i.enforce(httptest.NewRecorder(), requestFrom("203.0.113.1"), sess, "rlip") {
		t.Fatal("IP A first request throttled")
	}
	if i.enforce(httptest.NewRecorder(), requestFrom("203.0.113.1"), sess, "rlip") {
		t.Fatal("IP A second request should be throttled")
	}
	if !i.enforce(httptest.NewRecorder(), requestFrom("203.0.113.2"), sess, "rlip") {
		t.Fatal("IP B must have its own bucket and be admitted")
	}
}

func TestEnforceIPBeforeAuth(t *testing.T) {
	// A denied IP is rejected even with a valid credential (defence in depth).
	i := enforceTestIngress()
	sess := fakeSession{tunnel: core.Tunnel{ID: "both", Policy: core.Policy{
		DenyIPs:   []string{"203.0.113.0/24"},
		BasicAuth: []core.BasicAuthCred{{User: "alice", Hash: bcryptHash(t, "s3cret")}},
	}}}
	w := httptest.NewRecorder()
	r := requestFrom("203.0.113.9")
	r.SetBasicAuth("alice", "s3cret")
	if i.enforce(w, r, sess, "both") {
		t.Fatal("a denied IP was admitted because it had a valid credential")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (IP check precedes auth)", w.Code)
	}
}
