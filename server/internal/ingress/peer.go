package ingress

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/anomaly-sh/rift/server/internal/config"
)

// forwardToPeer relays the request to the node whose agent holds the subdomain.
func (i *Ingress) forwardToPeer(w http.ResponseWriter, r *http.Request, nodeURL, sub string) {
	target := strings.TrimSuffix(nodeURL, "/") + config.RouteInternalProxy

	outbound, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		i.writeGatewayError(w, r, http.StatusBadGateway, "peer_forward_failed",
			"Could not reach the node serving this tunnel.")
		return
	}

	// Carry the original request verbatim; the peer reconstructs it from these.
	outbound.Header = r.Header.Clone()
	outbound.Host = r.Host
	outbound.ContentLength = r.ContentLength
	outbound.Header.Set(config.HeaderRiftSubdomain, sub)
	outbound.Header.Set(config.HeaderRiftPeerToken, i.cfg.Cluster.PeerSecret)
	outbound.Header.Set(config.HeaderForwardedHost, r.Host)
	outbound.Header.Set(config.HeaderForwardedProto, i.cfg.Tunnel.PublicScheme)
	outbound.Header.Set(config.HeaderRealIP, i.clientIP(r))

	resp, err := i.peers.Do(outbound)
	if err != nil {
		i.logger.Warn("peer forward failed",
			slog.String("subdomain", sub), slog.String("node", nodeURL), slog.Any("error", err))
		i.writeGatewayError(w, r, http.StatusBadGateway, "peer_forward_failed",
			"Could not reach the node serving this tunnel.")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	header := w.Header()
	for k, vs := range resp.Header {
		for _, v := range vs {
			header.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if err := streamBody(w, resp.Body); err != nil {
		i.logger.Debug("peer response stream ended early", slog.Any("error", err))
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
