package postgres

import (
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/store/storetest"
)

// TestConformance holds the Postgres adapter to the same table the memory
// adapter runs. testDB skips it when RIFT_TEST_POSTGRES_DSN is unset and
// truncates between subtests.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Stores {
		db := testDB(t)
		return storetest.Stores{Tokens: db.Tokens(), Reservations: db.Reservations(), Tunnels: db.Tunnels(), Domains: db.Domains()}
	})
}
