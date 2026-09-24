package core

import (
	"context"
	"io"
	"net/http"
)

// TunnelConn is a full-duplex byte stream carrying a connection upgrade
// (WebSocket and other Upgrade-based protocols), a raw tcp/tls/grpc
// connection, or a udp flow through a tunnel. Read yields bytes coming from the local service; Write
// sends bytes to it.
type TunnelConn interface {
	io.ReadWriteCloser

	// CloseWrite half-closes the client->service direction: the local service
	// is told no more bytes will arrive from the client, while it may keep
	// sending. Closing both directions is done with Close.
	CloseWrite() error
}

// Pipe relays bytes both ways between a public client connection and a tunnel
// stream until either side ends, then closes both. fromClient is what is read
// from the client: normally client itself, or a reader that first replays
// bytes already consumed while routing (a peeked TLS ClientHello, an h2c
// preface, a buffered upgrade request).
//
// When the client stops sending, the tunnel is only half-closed (CloseWrite),
// so the local service can still finish its reply. Once either copy ends,
// closing both ends unblocks the other copy, and Pipe returns after both have
// stopped. Every raw path (tcp, tls, grpc, and HTTP upgrades) shares this one
// loop so their shutdown semantics cannot drift apart.
func Pipe(client io.WriteCloser, fromClient io.Reader, tconn TunnelConn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(tconn, fromClient)
		_ = tconn.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, tconn)
		done <- struct{}{}
	}()
	<-done
	_ = tconn.Close()
	_ = client.Close()
	<-done
}

// RawOpener is an optional capability of a Session: opening a raw full-duplex
// byte stream to the agent's local service, with no application handshake. It
// backs tcp, tls, grpc and udp tunnels; the shared tls and grpc listeners use
// it after resolving a session by ClientHello SNI or h2c :authority.
type RawOpener interface {
	OpenRaw(ctx context.Context) (TunnelConn, error)
}

// Upgrader is an optional capability of a Session: carrying a connection
// upgrade rather than a single request/response exchange. A Session that
// cannot upgrade simply does not implement it, and the ingress falls back to a
// gateway error.
type Upgrader interface {
	// Upgrade forwards an Upgrade request through the tunnel. When the local
	// service switches protocols the returned response has StatusCode 101 and
	// conn is a live full-duplex stream carrying the upgraded connection; the
	// response then has no body and its headers are the handshake response.
	//
	// When the local service declines to upgrade, conn is nil and resp is an
	// ordinary response the caller relays as usual.
	Upgrade(req *http.Request) (resp *http.Response, conn TunnelConn, err error)
}
