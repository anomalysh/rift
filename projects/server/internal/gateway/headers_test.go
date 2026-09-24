package gateway

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
)

// A request head carries end-to-end headers only, lower-cased: the fixed
// hop-by-hop set and anything the request's Connection header names are
// dropped.
func TestForwardableHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/plain")
	h.Add("X-Multi", "a")
	h.Add("X-Multi", "b")
	h.Set("Connection", "keep-alive, X-Hop")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("X-Hop", "secret")
	h.Set("Transfer-Encoding", "chunked")
	h.Set("Te", "trailers")

	got := forwardableHeaders(h)
	want := map[string][]string{
		"content-type": {"text/plain"},
		"x-multi":      {"a", "b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forwardableHeaders = %v, want %v", got, want)
	}

	// An upgrade keeps everything, Connection and Upgrade included.
	if up := upgradeHeaders(h); len(up) != len(h) || up["connection"] == nil {
		t.Fatalf("upgradeHeaders dropped headers: %v", up)
	}
}

// A relayed response strips hop-by-hop headers the same way a forwarded
// request does, including those its own Connection header names. It used to
// strip only the fixed set, so a header the agent's service marked as
// connection-scoped reached the public client.
func TestBuildResponseStripsHopByHop(t *testing.T) {
	s := &session{}
	req := httptest.NewRequest(http.MethodGet, "http://app.rift.test/", nil)
	resp := s.buildResponse(req, newStream(1, 1), tunnelproto.ResponseHead{
		Status: http.StatusOK,
		Headers: map[string][]string{
			"content-length": {"5"},
			"connection":     {"X-Internal-Hop"},
			"x-internal-hop": {"from the service"},
			"keep-alive":     {"timeout=5"},
			"x-kept":         {"yes"},
		},
	})
	for _, name := range []string{"Connection", "X-Internal-Hop", "Keep-Alive"} {
		if v := resp.Header.Get(name); v != "" {
			t.Errorf("response kept hop-by-hop header %s: %q", name, v)
		}
	}
	if resp.Header.Get("X-Kept") != "yes" {
		t.Error("response lost an end-to-end header")
	}
	if resp.ContentLength != 5 {
		t.Errorf("ContentLength = %d, want 5", resp.ContentLength)
	}
}
