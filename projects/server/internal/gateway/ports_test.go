package gateway

import (
	"errors"
	"net"
	"strconv"
	"testing"
)

// A port that was busy when one tunnel bound must stay in the pool for the
// next. It used to be dropped for the life of the process, so every transient
// conflict permanently shrank the range until binds failed outright.
func TestPortPoolKeepsBusyPorts(t *testing.T) {
	p := newPortPool[int](1, 2)
	busy := map[int]bool{1: true, 2: true}
	open := func(port int) (int, error) {
		if busy[port] {
			return 0, errors.New("address already in use")
		}
		return port * 10, nil
	}
	var skipped []int
	skip := func(port int, _ error) { skipped = append(skipped, port) }

	a, b := &session{}, &session{}
	if _, _, ok := p.acquire(a, open, skip); ok {
		t.Fatal("acquired a port while every port was busy")
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped %v, want both ports", skipped)
	}

	// The conflict clears: the same ports must be available again.
	busy = map[int]bool{}
	bind, port, ok := p.acquire(a, open, skip)
	if !ok || bind != port*10 {
		t.Fatalf("acquire after the conflict cleared = %d, %d, %v", bind, port, ok)
	}
	if _, second, ok := p.acquire(b, open, skip); !ok || second == port {
		t.Fatalf("second acquire = %d, %v; want the other port", second, ok)
	}

	// Release returns the bind once and the port to the pool.
	if got, ok := p.release(a); !ok || got != bind {
		t.Fatalf("release = %d, %v", got, ok)
	}
	if _, ok := p.release(a); ok {
		t.Fatal("a second release reported a bind")
	}
	if _, again, ok := p.acquire(a, open, skip); !ok || again != port {
		t.Fatalf("acquire after release = %d, %v; want port %d back", again, ok, port)
	}
}

// End to end on real sockets: a tcp port another process briefly held is
// usable once it frees up.
func TestTCPForwarderRetriesPortHeldElsewhere(t *testing.T) {
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := squatter.Addr().(*net.TCPAddr).Port

	cfg := testConfig()
	cfg.TCP.Enabled = true
	cfg.TCP.ListenHost = "127.0.0.1"
	cfg.TCP.AdvertiseHost = "127.0.0.1"
	cfg.TCP.PortMin, cfg.TCP.PortMax = port, port
	f := newTCPForwarder(cfg, testLogger())
	sess := &session{}

	if _, err := f.bind(sess); !errors.Is(err, errNoTCPPorts) {
		t.Fatalf("bind while the port is held elsewhere: err = %v, want errNoTCPPorts", err)
	}
	_ = squatter.Close()

	addr, err := f.bind(sess)
	if err != nil {
		t.Fatalf("bind after the port freed up: %v", err)
	}
	defer f.release(sess)
	if want := net.JoinHostPort("127.0.0.1", strconv.Itoa(port)); addr != want {
		t.Fatalf("bound %s, want %s", addr, want)
	}
}
