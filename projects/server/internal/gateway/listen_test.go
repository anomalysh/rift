package gateway

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// scriptedListener returns each queued Accept result in turn, then blocks
// until closed.
type scriptedListener struct {
	results chan acceptResult
	closed  chan struct{}
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func newScriptedListener(results ...acceptResult) *scriptedListener {
	l := &scriptedListener{results: make(chan acceptResult, len(results)), closed: make(chan struct{})}
	for _, r := range results {
		l.results <- r
	}
	return l
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.results:
		return r.conn, r.err
	default:
	}
	select {
	case r := <-l.results:
		return r.conn, r.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *scriptedListener) Close() error   { close(l.closed); return nil }
func (l *scriptedListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// A transient Accept failure must not end the loop. It used to: a tcp tunnel's
// port, or the whole shared tls/grpc listener, went dark on the first EMFILE
// while the tunnels behind it stayed advertised.
func TestAcceptLoopSurvivesTransientErrors(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	emfile := errors.New("accept: too many open files")
	ln := newScriptedListener(acceptResult{err: emfile}, acceptResult{err: emfile}, acceptResult{conn: c1})

	handled := make(chan net.Conn, 1)
	done := make(chan error, 1)
	go func() {
		done <- acceptLoop(context.Background(), ln, testLogger(), func(c net.Conn) { handled <- c })
	}()

	select {
	case c := <-handled:
		if c != c1 {
			t.Fatal("handled the wrong connection")
		}
		_ = c.Close()
	case err := <-done:
		t.Fatalf("accept loop ended on a transient error: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the connection after the transient errors was never handled")
	}

	// A listener closed out from under a live context is reported.
	_ = ln.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("accept loop returned %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop did not end after its listener closed")
	}
}

// Cancelling the owner's context ends the loop cleanly, even mid-backoff.
func TestAcceptLoopStopsOnCancel(t *testing.T) {
	ln := newScriptedListener(acceptResult{err: errors.New("transient")})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- acceptLoop(ctx, ln, testLogger(), func(c net.Conn) { _ = c.Close() }) }()

	cancel()
	_ = ln.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("accept loop returned %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("accept loop did not stop on cancel")
	}
}
