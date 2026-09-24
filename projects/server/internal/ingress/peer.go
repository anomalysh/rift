package ingress

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/tunnelproto"
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
					i.relayResponse(w, resp2, sub, "peer")
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
	i.relayResponse(w, resp, sub, "peer")
}

// errInvalidPeerURL means a routing lease named something other than a plain
// http(s) base URL. Leases come from Redis; forwarding the peer secret and a
// visitor's request to whatever a corrupted or hostile entry names would leak
// both, so such a lease is treated exactly like an unreachable node.
var errInvalidPeerURL = errors.New("ingress: peer lease is not an http(s) URL")

// validPeerURL reports whether nodeURL is an absolute http or https URL with a
// host and nothing a node advertise URL never carries (credentials, a query,
// a fragment).
func validPeerURL(nodeURL string) bool {
	u, err := url.Parse(nodeURL)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

// doPeerForward performs one forward attempt. A non-nil error is a transport
// failure (the node is unreachable, or its lease is unusable); an HTTP error
// status comes back as a normal response for the caller to relay.
func (i *Ingress) doPeerForward(r *http.Request, nodeURL, sub string) (*http.Response, error) {
	if !validPeerURL(nodeURL) {
		return nil, fmt.Errorf("%w: %q", errInvalidPeerURL, nodeURL)
	}
	target := strings.TrimSuffix(nodeURL, "/") + config.RouteInternalProxy

	outbound, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		return nil, err
	}

	// Carry the original request verbatim; the peer reconstructs it from these.
	// The forwarding request is addressed to RouteInternalProxy, so the peer
	// cannot recover the original path from its own URL -- it comes over in a
	// header instead. The X-Forwarded-* / X-Real-IP headers describing the
	// public client were already stamped at the edge (servePublic ->
	// annotateForwarded) and ride along in this clone, so the receiving node
	// must not re-derive them from this internal hop.
	outbound.Header = r.Header.Clone()
	outbound.Host = r.Host
	outbound.ContentLength = r.ContentLength
	outbound.Header.Set(config.HeaderRiftSubdomain, sub)
	outbound.Header.Set(config.HeaderRiftForwardedURI, r.URL.RequestURI())
	outbound.Header.Set(config.HeaderRiftPeerToken, i.cfg.Cluster.PeerSecret)
	outbound.Header.Set(config.HeaderRiftProtoVersion, strconv.Itoa(tunnelproto.Version))
	// The receiving node's socket peer is this node, not the visitor, so the
	// address its IP policy and rate limit must judge travels explicitly.
	outbound.Header.Set(config.HeaderRiftClientIP, i.clientIP(r))

	return i.peers.Do(outbound)
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

// clientIPKey is the request-context key holding the resolved client address.
type clientIPKey struct{}

// withClientIP pins the resolved client address to the request, so every
// later consumer (policy, rate limit, X-Real-IP, the agent's RemoteAddr)
// agrees on it and nothing re-derives it from headers a later hop may carry.
func withClientIP(r *http.Request, ip string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip))
}

// clientIP returns the public client's address: the value pinned by
// withClientIP when there is one, otherwise resolveClientIP.
func (i *Ingress) clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok && ip != "" {
		return ip
	}
	return i.resolveClientIP(r)
}

// resolveClientIP works out the public client's address from the socket and,
// when the socket peer is a trusted proxy, the forwarding headers.
//
// X-Forwarded-For is trivially spoofable: each proxy APPENDS the address it
// received the request from, so only entries added by proxies we trust are
// believable, and those are the right-most ones. The chain is therefore walked
// right to left, skipping trusted proxies, and the first untrusted address is
// the client. Taking the left-most entry instead would return whatever the
// client wrote into the header before the first proxy appended to it.
//
// An entry that is not an IP address means the trusted part of the chain
// ended in something no proxy would write; the nearest address we can vouch
// for (the last trusted hop) is returned rather than attacker-chosen text, so
// such requests share one rate-limit bucket instead of minting fresh ones.
//
// X-Real-IP is consulted only when the socket peer is trusted and sent no
// X-Forwarded-For at all.
func (i *Ingress) resolveClientIP(r *http.Request) string {
	host := socketIP(r)
	if !i.isTrustedProxy(host) {
		return host
	}
	entries := forwardedForEntries(r.Header)
	if len(entries) == 0 {
		if real := parseIPEntry(r.Header.Get(config.HeaderRealIP)); real != "" {
			return real
		}
		return host
	}
	nearest := host
	for k := len(entries) - 1; k >= 0; k-- {
		ip := parseIPEntry(entries[k])
		if ip == "" {
			return nearest
		}
		if !i.isTrustedProxy(ip) {
			return ip
		}
		nearest = ip
	}
	// Every hop is a trusted proxy: the request originated at one of them.
	return nearest
}

// socketIP is the immediate peer's address without its port.
func socketIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedForEntries flattens every X-Forwarded-For header line into its
// comma-separated entries, in order. RFC 9110 lets a proxy send the list as
// repeated lines as well as one comma-joined line; both mean the same chain.
func forwardedForEntries(h http.Header) []string {
	var out []string
	for _, line := range h.Values(config.HeaderForwardedFor) {
		for _, e := range strings.Split(line, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
	}
	return out
}

// parseIPEntry parses one forwarding-header entry, tolerating the host:port
// and [v6]:port forms some proxies write, and returns the canonical address or
// "" when it is not an IP.
func parseIPEntry(e string) string {
	e = strings.TrimSpace(e)
	if ip := net.ParseIP(e); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(e); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
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
