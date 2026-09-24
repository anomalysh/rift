package registry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// memLeases is an in-memory leaseStore. beforeCompareAndDelete, when set,
// runs at the start of compareAndDelete, so a test can interleave another
// registration exactly there.
type memLeases struct {
	mu                     sync.Mutex
	kv                     map[string]string
	beforeCompareAndDelete func()
}

func newMemLeases() *memLeases { return &memLeases{kv: map[string]string{}} }

func (m *memLeases) set(_ context.Context, key, value string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kv[key] = value
	return nil
}

func (m *memLeases) get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.kv[key]
	return v, ok, nil
}

func (m *memLeases) compareAndDelete(_ context.Context, key, value string) error {
	if hook := m.beforeCompareAndDelete; hook != nil {
		m.beforeCompareAndDelete = nil
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kv[key] == value {
		delete(m.kv, key)
	}
	return nil
}

func (m *memLeases) close() error { return nil }

type fakeSession struct{ sub string }

func (f *fakeSession) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }
func (f *fakeSession) Tunnel() core.Tunnel                             { return core.Tunnel{Subdomain: f.sub} }
func (f *fakeSession) Close(string) error                              { return nil }

const thisNode = "http://node-a.internal"

func newTestDistributed(leases leaseStore) *distributed {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newDistributedWith(NewLocal(), leases, "rift:route:", thisNode, time.Minute, logger)
}

func (m *memLeases) holder(sub string) (string, bool) {
	v, ok, _ := m.get(context.Background(), "rift:route:"+sub)
	return v, ok
}

func TestDistributedLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	leases := newMemLeases()
	d := newTestDistributed(leases)

	old, current := &fakeSession{sub: "app"}, &fakeSession{sub: "app"}
	if _, err := d.Register(ctx, old); err != nil {
		t.Fatal(err)
	}
	if v, ok := leases.holder("app"); !ok || v != thisNode {
		t.Fatalf("lease after Register = %q, %v", v, ok)
	}
	if displaced, _ := d.Register(ctx, current); displaced != old {
		t.Fatal("Register did not report the displaced session")
	}

	// The displaced session's slow teardown must leave its replacement's lease.
	if err := d.Unregister(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := leases.holder("app"); !ok {
		t.Fatal("a displaced session retracted its replacement's lease")
	}

	// The holder's own teardown retracts it.
	if err := d.Unregister(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, ok := leases.holder("app"); ok {
		t.Fatal("the lease outlived its session")
	}

	// A lease naming another node is a peer; our own is not.
	_ = leases.set(ctx, "rift:route:other", "http://node-b.internal", time.Minute)
	if url, ok, err := d.LocatePeer(ctx, "other"); err != nil || !ok || url != "http://node-b.internal" {
		t.Fatalf("LocatePeer(other) = %q, %v, %v", url, ok, err)
	}
	_ = leases.set(ctx, "rift:route:mine", thisNode, time.Minute)
	if _, ok, _ := d.LocatePeer(ctx, "mine"); ok {
		t.Fatal("LocatePeer reported this node as a peer")
	}
}

// An agent that reconnects to the same node while its old session is being
// torn down publishes a lease with the same value the old session is about to
// retract. The retraction used to delete it, leaving the live tunnel
// unroutable from peers until the next refresh.
func TestDistributedUnregisterKeepsReconnectLease(t *testing.T) {
	ctx := context.Background()
	leases := newMemLeases()
	d := newTestDistributed(leases)

	old, reconnect := &fakeSession{sub: "app"}, &fakeSession{sub: "app"}
	if _, err := d.Register(ctx, old); err != nil {
		t.Fatal(err)
	}
	// The reconnect lands after the old session left the local map but before
	// its lease retraction reaches the store.
	leases.beforeCompareAndDelete = func() {
		if _, err := d.Register(ctx, reconnect); err != nil {
			t.Errorf("reconnect Register: %v", err)
		}
	}
	if err := d.Unregister(ctx, old); err != nil {
		t.Fatal(err)
	}

	if cur, ok := d.Lookup(ctx, "app"); !ok || cur != reconnect {
		t.Fatal("the reconnected session is not the local holder")
	}
	if v, ok := leases.holder("app"); !ok || v != thisNode {
		t.Fatalf("the reconnected session's lease is gone (%q, %v)", v, ok)
	}
}
