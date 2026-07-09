package ingress

import (
	"net/http"
	"strings"
	"testing"
)

// A non-idempotent method must never be silently retried against another node:
// repeating a POST could submit an order twice.
func TestCanRetryForward(t *testing.T) {
	cases := []struct {
		method string
		body   bool
		want   bool
	}{
		{http.MethodGet, false, true},
		{http.MethodHead, false, true},
		{http.MethodOptions, false, true},
		{http.MethodPost, false, false},
		{http.MethodPut, false, false},
		{http.MethodDelete, false, false},
		{http.MethodPatch, false, false},
		// A body cannot be replayed even for an otherwise-idempotent method.
		{http.MethodGet, true, false},
	}
	for _, tc := range cases {
		var body *http.Request
		if tc.body {
			r, _ := http.NewRequest(tc.method, "http://x/", strings.NewReader("payload"))
			body = r
		} else {
			r, _ := http.NewRequest(tc.method, "http://x/", nil)
			body = r
		}
		if got := canRetryForward(body); got != tc.want {
			t.Errorf("canRetryForward(%s body=%v) = %v, want %v", tc.method, tc.body, got, tc.want)
		}
	}
}
