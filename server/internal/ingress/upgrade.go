package ingress

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/anomalysh/rift/server/internal/core"
)

// isUpgradeRequest reports whether r asks to switch protocols (WebSocket and
// other Upgrade-based schemes). Both an Upgrade header and a Connection header
// listing the "upgrade" token are required, per RFC 7230 section 6.7.
func isUpgradeRequest(r *http.Request) bool {
	if r.Header.Get("Upgrade") == "" {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// proxyUpgrade carries a connection upgrade through a locally attached tunnel.
// On a successful 101 it hijacks the public connection and pipes bytes both
// ways; if the local service declines to upgrade it relays the ordinary
// response instead.
func (i *Ingress) proxyUpgrade(w http.ResponseWriter, r *http.Request, sess core.Session, sub string) {
	up, ok := sess.(core.Upgrader)
	if !ok {
		i.writeGatewayError(w, r, http.StatusBadGateway, "upgrade_unsupported",
			"This tunnel cannot carry a connection upgrade.")
		return
	}

	// The handshake gets a deadline; the upgraded pipe that follows must not,
	// so the deadline is cancelled the moment the handshake resolves.
	hsCtx, cancel := context.WithTimeout(r.Context(), i.cfg.Tunnel.RequestTimeout)
	outbound := r.Clone(hsCtx)
	outbound.RemoteAddr = i.clientIP(r)

	resp, tconn, err := up.Upgrade(outbound)
	if err != nil {
		cancel()
		i.writeRoundTripError(w, r, sub, err)
		return
	}
	if tconn == nil {
		// The service answered without switching protocols; relay it verbatim.
		cancel()
		defer func() { _ = resp.Body.Close() }()
		header := w.Header()
		for k, vs := range resp.Header {
			for _, v := range vs {
				header.Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if err := streamBody(w, resp.Body); err != nil {
			i.logger.Debug("declined-upgrade response ended early",
				slog.String("subdomain", sub), slog.Any("error", err))
		}
		return
	}
	cancel()
	defer func() { _ = tconn.Close() }()

	rc := http.NewResponseController(w)
	clientConn, brw, err := rc.Hijack()
	if err != nil {
		// Nothing is written yet, so a normal error status is still possible.
		// This only happens if the server does not support hijacking (e.g.
		// HTTP/2), which Caddy->gateway (HTTP/1.1) never uses.
		i.logger.Warn("cannot hijack public connection for upgrade",
			slog.String("subdomain", sub), slog.Any("error", err))
		i.writeGatewayError(w, r, http.StatusInternalServerError, "upgrade_unsupported",
			"The gateway could not switch this connection.")
		return
	}
	defer func() { _ = clientConn.Close() }()

	if err := writeSwitchingProtocols(brw.Writer, resp); err != nil {
		i.logger.Debug("failed to write 101 response",
			slog.String("subdomain", sub), slog.Any("error", err))
		return
	}
	if err := brw.Writer.Flush(); err != nil {
		return
	}

	i.logger.Debug("connection upgraded through tunnel", slog.String("subdomain", sub))
	pipeUpgrade(clientConn, brw.Reader, tconn)
}

// writeSwitchingProtocols renders the 101 status line and headers onto the
// hijacked connection. The header block includes Connection and Upgrade, which
// the public client needs to complete the switch.
func writeSwitchingProtocols(w io.Writer, resp *http.Response) error {
	statusText := resp.Status
	if statusText == "" {
		statusText = http.StatusText(resp.StatusCode)
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", resp.StatusCode, statusText)
	if err := resp.Header.Write(&b); err != nil {
		return err
	}
	b.WriteString("\r\n")
	_, err := w.Write(b.Bytes())
	return err
}

// pipeUpgrade streams bytes between the public client and the tunnel until
// either side closes, then tears both ends down so the other copy unblocks.
func pipeUpgrade(client net.Conn, clientRd *bufio.Reader, tconn core.TunnelConn) {
	done := make(chan struct{}, 2)

	// client -> local service
	go func() {
		_, _ = io.Copy(tconn, clientRd)
		_ = tconn.CloseWrite()
		done <- struct{}{}
	}()
	// local service -> client
	go func() {
		_, _ = io.Copy(client, tconn)
		done <- struct{}{}
	}()

	<-done
	// One direction ended; closing both ends unblocks the still-running copy.
	_ = tconn.Close()
	_ = client.Close()
	<-done
}
