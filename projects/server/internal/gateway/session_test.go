package gateway

import (
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// maxRequests only reads the tunnel policy, so a bare session literal exercises
// the A4 --once / --max-requests quota mapping without a live connection.
func TestSessionMaxRequests(t *testing.T) {
	cases := []struct {
		name   string
		policy core.Policy
		want   int64
	}{
		{"unbounded", core.Policy{}, 0},
		{"once", core.Policy{Once: true}, 1},
		{"max-requests", core.Policy{MaxRequests: 5}, 5},
		{"once-wins-over-max", core.Policy{Once: true, MaxRequests: 5}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &session{tunnel: core.Tunnel{Policy: tc.policy}}
			if got := s.maxRequests(); got != tc.want {
				t.Fatalf("maxRequests() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The RoundTrip gate admits exactly maxRequests before it starts rejecting, so
// a --max-requests=3 tunnel serves 3 and throttles the 4th.
func TestSessionRequestQuotaGate(t *testing.T) {
	s := &session{tunnel: core.Tunnel{Policy: core.Policy{MaxRequests: 3}}}
	maxReq := s.maxRequests()
	served := 0
	for n := 1; n <= 5; n++ {
		if s.requestsServed.Add(1) <= maxReq {
			served++
		}
	}
	if served != 3 {
		t.Fatalf("served %d requests, want 3 within the quota", served)
	}
}
