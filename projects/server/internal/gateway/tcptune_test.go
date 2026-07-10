package gateway

import (
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/anomalysh/rift/projects/server/internal/config"
)

// tuneTCPConn must apply cleanly to a real accepted *net.TCPConn and must not
// panic on a non-TCP conn (the net.Pipe used in some tests).
func TestTuneTCPConn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	srv := <-accepted
	if srv == nil {
		t.Fatal("accept failed")
	}
	defer func() { _ = srv.Close() }()

	// Keep-alive enabled and NoDelay set: no error on a real TCP conn.
	tuneTCPConn(srv, config.TCP{NoDelay: true, KeepAliveSeconds: 30}, logger)
	// Keep-alive disabled path.
	tuneTCPConn(srv, config.TCP{NoDelay: false, KeepAliveSeconds: 0}, logger)

	// A non-TCP conn is silently skipped, not a panic.
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	tuneTCPConn(a, config.TCP{NoDelay: true, KeepAliveSeconds: 15}, logger)
}
