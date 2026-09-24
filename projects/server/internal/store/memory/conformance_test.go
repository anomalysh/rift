package memory

import (
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) storetest.Stores {
		s := New()
		return storetest.Stores{Tokens: s.Tokens(), Reservations: s.Reservations(), Tunnels: s.Tunnels(), Domains: s.Domains()}
	})
}
