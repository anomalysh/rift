package gateway

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

func TestDatagramRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payloads := [][]byte{
		[]byte("hello"),
		{},                               // an empty datagram is valid
		bytes.Repeat([]byte{0xAB}, 1500), // a jumbo-ish datagram
	}
	for _, p := range payloads {
		if err := writeDatagram(&buf, p); err != nil {
			t.Fatalf("writeDatagram: %v", err)
		}
	}

	// The three datagrams read back in order with their boundaries intact, even
	// though they were written into one contiguous buffer.
	out := make([]byte, maxDatagram)
	for i, want := range payloads {
		n, err := readDatagram(&buf, out)
		if err != nil {
			t.Fatalf("readDatagram %d: %v", i, err)
		}
		if !bytes.Equal(out[:n], want) {
			t.Fatalf("datagram %d = %q, want %q", i, out[:n], want)
		}
	}
	// The stream is now drained.
	if _, err := readDatagram(&buf, out); err != io.EOF {
		t.Fatalf("expected EOF after the last datagram, got %v", err)
	}
}

func TestReadDatagramRejectsOversizeLength(t *testing.T) {
	// A length prefix larger than the caller's buffer must fail closed, not read
	// unbounded.
	framed := []byte{0xFF, 0xFF} // declares 65535 bytes
	small := make([]byte, 16)
	if _, err := readDatagram(bytes.NewReader(framed), small); err == nil {
		t.Fatal("expected an error for a length exceeding the buffer")
	}
}

// New flows beyond the per-IP or per-bind cap are dropped before any tunnel
// stream is opened; established flows keep working, and retiring a flow frees
// its slot.
func TestUDPFlowCaps(t *testing.T) {
	cfg := testConfig()
	s := newSession(nil, core.Tunnel{ID: "t"}, cfg, &countingTunnels{}, nil, testLogger())
	defer func() { _ = s.Close("test") }()
	// No write loop runs; drain REQ_HEAD/RESET frames so OpenRaw never blocks.
	go func() {
		for {
			select {
			case <-s.out:
			case <-s.closing:
				return
			}
		}
	}()

	f := &udpForwarder{cfg: cfg, logger: testLogger(), maxFlows: 5, maxFlowsPerIP: 2}
	b := &udpBind{flows: map[string]*udpFlow{}, perIP: map[string]int{}}
	ctx := context.Background()
	addr := func(ip string, port int) *net.UDPAddr { return &net.UDPAddr{IP: net.ParseIP(ip), Port: port} }
	defer func() {
		b.mu.Lock()
		for _, fl := range b.flows {
			_ = fl.tconn.Close()
		}
		b.mu.Unlock()
	}()

	// Per source IP: the third port from one host is refused.
	a1 := f.flowFor(ctx, s, b, addr("198.51.100.1", 1000))
	a2 := f.flowFor(ctx, s, b, addr("198.51.100.1", 1001))
	if a1 == nil || a2 == nil {
		t.Fatal("flows under the per-ip cap were refused")
	}
	if fl := f.flowFor(ctx, s, b, addr("198.51.100.1", 1002)); fl != nil {
		t.Fatal("a flow over the per-ip cap was opened")
	}
	// An existing flow is still found, cap or not.
	if fl := f.flowFor(ctx, s, b, addr("198.51.100.1", 1000)); fl != a1 {
		t.Fatal("an established flow was not reused at the cap")
	}

	// Per bind: 5 flows in total, whatever their source.
	for i, ip := range []string{"198.51.100.2", "198.51.100.3", "198.51.100.4"} {
		if f.flowFor(ctx, s, b, addr(ip, 2000)) == nil {
			t.Fatalf("flow %d under the bind cap was refused", i)
		}
	}
	if fl := f.flowFor(ctx, s, b, addr("198.51.100.5", 2000)); fl != nil {
		t.Fatal("a flow over the per-bind cap was opened")
	}
	if got := s.streamCount(); got != 5 {
		t.Fatalf("session has %d streams, want 5 (refused flows must not open one)", got)
	}

	// Retiring a flow frees both its bind slot and its per-ip slot, so the
	// same address can open a fresh flow.
	key1 := addr("198.51.100.1", 1000).String()
	f.dropFlow(b, key1, a1)
	a1b := f.flowFor(ctx, s, b, addr("198.51.100.1", 1000))
	if a1b == nil || a1b == a1 {
		t.Fatal("a freed slot was not reusable")
	}
	b.mu.Lock()
	perIP := b.perIP["198.51.100.1"]
	b.mu.Unlock()
	if perIP != 2 {
		t.Fatalf("per-ip count = %d, want 2", perIP)
	}

	// The retired flow's return loop also calls dropFlow on its way out; it
	// must not evict the replacement now holding the same address.
	f.dropFlow(b, key1, a1)
	b.mu.Lock()
	cur, n := b.flows[key1], len(b.flows)
	b.mu.Unlock()
	if cur != a1b || n != 5 {
		t.Fatalf("stale dropFlow evicted the replacement flow (%d flows)", n)
	}
}

func TestWriteDatagramRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	if err := writeDatagram(&buf, make([]byte, maxDatagram+1)); err == nil {
		t.Fatal("expected an error for a datagram over the maximum size")
	}
}
