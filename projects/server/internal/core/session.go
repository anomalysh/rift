package core

import (
	"context"
	"net/http"
)

// Session is a live agent connection that can serve proxied HTTP requests.
//
// It is an http.RoundTripper so the ingress can forward a request without
// knowing that a WebSocket, a frame codec, or a stream multiplexer exists.
// The gateway implements it; the ingress consumes it; neither imports the
// other.
type Session interface {
	http.RoundTripper

	// Tunnel describes the subdomain and owner this session serves.
	Tunnel() Tunnel

	// Close terminates the session, telling the agent why.
	Close(reason string) error
}

// Registry resolves a subdomain to whatever can serve it.
type Registry interface {
	// Register makes the session routable. It replaces any session already
	// holding the same subdomain, returning the displaced one so the caller
	// can shut it down.
	Register(ctx context.Context, s Session) (displaced Session, err error)

	// Unregister removes the session if it is still the holder of its
	// subdomain. A session displaced by a newer one is a no-op, which keeps a
	// slow disconnect from evicting its own replacement.
	Unregister(ctx context.Context, s Session) error

	// Lookup returns the locally attached session for subdomain.
	Lookup(ctx context.Context, subdomain string) (Session, bool)

	// LocatePeer returns the advertised base URL of the node currently
	// serving subdomain, when that node is not this one. Single-node
	// deployments always return ok=false.
	LocatePeer(ctx context.Context, subdomain string) (nodeURL string, ok bool, err error)

	// InvalidatePeer drops the routing belief that subdomain lives at nodeURL,
	// but only if that is still what is recorded. It is called after a forward
	// to nodeURL failed at the connection level: the node is gone and its
	// lease is stale, so the next request should re-locate rather than repeat a
	// doomed hop. A compare check avoids deleting a lease a reconnected agent
	// has since republished. A local-only registry has nothing to invalidate.
	InvalidatePeer(ctx context.Context, subdomain, nodeURL string) error

	// Close releases any background resources.
	Close() error
}
