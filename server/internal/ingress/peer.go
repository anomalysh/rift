package ingress

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/anomalysh/rift/server/internal/config"
	"github.com/anomalysh/rift/server/internal/tunnelproto"
)

// forwardToPeer relays the request to the node whose agent holds the subdomain.
//
// If that node has died abruptly its Redis lease lingers until TTL, so the
// first forward fails at the connection level. Rather than return 502 and make
// the client wait out the TTL, this drops the stale lease and, for a request
// that is safe to repeat, re-locates and tries once more — the agent may have
// reconnected to another node in the meantime.
//
// A request is only retried when it is idempotent AND carries no body: HTTP
// forbids silently repeating a POST (it could submit an order twice), and a
// body stream cannot be replayed once partially read anyway.
func (i *Ingress) forwardToPeer(w http.ResponseWriter, r *http.Request, nodeURL, sub string) {
	if i.breaker.isOpen(nodeURL) {
		// The node has failed repeatedly and recently. Skip the doomed dial,
		// drop the belief, and answer now instead of after another timeout.
		_ = i.registry.InvalidatePeer(r.Context(), sub, nodeURL)
		i.writeGatewayError(w, r, http.StatusBadGateway, "peer_unavailable",
			"The node serving this tunnel is unavailable.")
		return
	}

	resp, err := i.doPeerForward(r, nodeURL, sub)
	if err != nil {
		i.breaker.recordFailure(nodeURL)
		i.logger.Warn("peer forward failed",
			slog.String("subdomain", sub), slog.String("node", nodeURL), slog.Any("error", err))

		// The node is gone; its lease is stale. Drop it so we do not keep
		// forwarding into a black hole.
		_ = i.registry.InvalidatePeer(r.Context(), sub, nodeURL)

		if canRetryForward(r) {
			if next, ok, lerr := i.registry.LocatePeer(r.Context(), sub); lerr == nil && ok && next != nodeURL {
				if resp2, err2 := i.doPeerForward(r, next, sub); err2 == nil {
					i.breaker.recordSuccess(next)
					i.relayPeerResponse(w, r, resp2, sub)
					return
				}
				i.breaker.recordFailure(next)
				_ = i.registry.InvalidatePeer(r.Context(), sub, next)
			}
		}

		i.writeGatewayError(w, r, http.StatusBadGateway, "peer_forward_failed",
			"Could not reach the node serving this tunnel.")
		return
	}

	i.breaker.recordSuccess(nodeURL)
	i.relayPeerResponse(w, r, resp, sub)
}

// doPeerForward performs one forward attempt. A non-nil error is a transport
// failure (the node is unreachable); an HTTP error status comes back as a
// normal response for the caller to relay.
func (i *Ingress) doPeerForward(r *http.Request, nodeURL, sub string) (*http.Response, error) {
	target := strings.TrimSuffix(nodeURL, "/") + config.RouteInternalProxy

	outbound, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		return nil, err
	}

	// Carry the original request verbatim; the peer reconstructs it from these.
	// The forwarding request is addressed to RouteInternalProxy, so the peer
	// cannot recover the original path from its own URL -- it comes over in a
	// header instead. The X-Forwarded-* / X-Real-IP headers describing the
	// public client were already stamped at the edge (handlePublic ->
	// annotateForwarded) and ride along in this clone, so the receiving node
	// must not re-derive them from this internal hop.
	outbound.Header = r.Header.Clone()
	outbound.Host = r.Host
	outbound.ContentLength = r.ContentLength
	outbound.Header.Set(config.HeaderRiftSubdomain, sub)
	outbound.Header.Set(config.HeaderRiftForwardedURI, r.URL.RequestURI())
	outbound.Header.Set(config.HeaderRiftPeerToken, i.cfg.Cluster.PeerSecret)
	outbound.Header.Set(config.HeaderRiftProtoVersion, strconv.Itoa(tunnelproto.Version))

	return i.peers.Do(outbound)
}

func (i *Ingress) relayPeerResponse(w http.ResponseWriter, _ *http.Request, resp *http.Response, sub string) {
	defer func() { _ = resp.Body.Close() }()

	header := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if err := streamBody(w, resp.Body); err != nil {
		i.logger.Debug("peer response stream ended early",
			slog.String("subdomain", sub), slog.Any("error", err))
	}
}

// canRetryForward reports whether a failed forward may be repeated against a
// different node. Only idempotent, body-less requests qualify: RFC 7231 §4.2.2
// forbids automatically retrying a non-idempotent method, and a request body
// stream cannot be replayed.
func canRetryForward(r *http.Request) bool {
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// authenticatePeer checks the shared secret in constant time.
func authenticatePeer(r *http.Request, secret string) bool {
	if secret == "" {
		return false
	}
	got := r.Header.Get(config.HeaderRiftPeerToken)
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// clientIP resolves the public client's address.
//
// X-Forwarded-For is trivially spoofable, so it is honoured only when the
// immediate peer is a configured trusted proxy. Otherwise the socket address
// wins, even though it will be the reverse proxy's own address.
func (i *Ingress) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !i.isTrustedProxy(host) {
		return host
	}
	if xff := r.Header.Get(config.HeaderForwardedFor); xff != "" {
		// The left-most entry is the original client, per convention. Entries
		// to its left cannot exist; entries to its right were added by proxies.
		if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
			return first
		}
	}
	if real := r.Header.Get(config.HeaderRealIP); real != "" {
		return strings.TrimSpace(real)
	}
	return host
}

func (i *Ingress) isTrustedProxy(host string) bool {
	if len(i.trusted) == 0 {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, t := range i.trusted {
		if t.net != nil && t.net.Contains(ip) {
			return true
		}
		if t.ip != nil && t.ip.Equal(ip) {
			return true
		}
	}
	return false
}

// parseTrusted accepts bare IPs and CIDR blocks. Unparseable entries are
// dropped rather than silently trusted.
func parseTrusted(entries []string) []netAddr {
	out := make([]netAddr, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(e); err == nil {
			out = append(out, netAddr{net: ipnet})
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			out = append(out, netAddr{ip: ip})
		}
	}
	return out
}

type jsonError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	var body jsonError
	body.Error.Code = code
	body.Error.Message = message

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		_, _ = io.WriteString(w, `{"error":{"code":"internal","message":"encoding failed"}}`)
	}
}
