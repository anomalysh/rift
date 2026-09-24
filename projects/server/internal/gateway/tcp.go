package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"

	"github.com/anomalysh/rift/projects/server/internal/config"
	"github.com/anomalysh/rift/projects/server/internal/core"
)

// errNoTCPPorts means every port in the configured range is in use.
var errNoTCPPorts = errors.New("gateway: no free tcp ports in the configured range")

// tcpForwarder accepts public TCP connections on a per-tunnel port and pipes
// each one to the tunnel's agent as a raw stream. It owns allocation of the
// configured port range.
type tcpForwarder struct {
	cfg    *config.Config
	logger *slog.Logger
	ports  *portPool[*tcpBind]
}

type tcpBind struct {
	ln   net.Listener
	stop context.CancelFunc
}

// newTCPForwarder builds a forwarder for the configured range, or nil when tcp
// tunnels are disabled.
func newTCPForwarder(cfg *config.Config, logger *slog.Logger) *tcpForwarder {
	if !cfg.TCP.Enabled {
		return nil
	}
	return &tcpForwarder{
		cfg:    cfg,
		logger: logger.With(slog.String("component", "tcp")),
		ports:  newPortPool[*tcpBind](cfg.TCP.PortMin, cfg.TCP.PortMax),
	}
}

// bind allocates a port, listens on it, and starts accepting connections for
// sess. It returns the public host:port the agent should advertise. A port that
// the OS reports as unavailable is skipped rather than failing the whole bind.
func (f *tcpForwarder) bind(sess *session) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	b, port, ok := f.ports.acquire(sess, func(port int) (*tcpBind, error) {
		ln, err := net.Listen("tcp", net.JoinHostPort(f.cfg.TCP.ListenHost, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		return &tcpBind{ln: ln, stop: cancel}, nil
	}, func(port int, err error) {
		f.logger.Warn("tcp port unavailable, skipping", slog.Int("port", port), slog.Any("error", err))
	})
	if !ok {
		cancel()
		return "", errNoTCPPorts
	}

	go f.accept(ctx, sess, b.ln, port)

	host := f.cfg.TCP.Advertise(f.cfg.Tunnel.BaseDomain)
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// release closes sess's listener and returns its port to the pool. Idempotent.
func (f *tcpForwarder) release(sess *session) {
	b, ok := f.ports.release(sess)
	if !ok {
		return
	}
	b.stop()
	_ = b.ln.Close()
}

// accept serves sess's port until release cancels ctx and closes ln.
func (f *tcpForwarder) accept(ctx context.Context, sess *session, ln net.Listener, port int) {
	err := acceptLoop(ctx, ln, f.logger, func(conn net.Conn) { f.handle(ctx, sess, conn) })
	if err != nil {
		f.logger.Debug("tcp accept ended", slog.Int("port", port), slog.Any("error", err))
	}
}

func (f *tcpForwarder) handle(ctx context.Context, sess *session, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	tuneTCPConn(conn, f.cfg.TCP, f.logger)

	tconn, err := sess.OpenRaw(ctx)
	if err != nil {
		f.logger.Debug("could not open raw stream for tcp connection", slog.Any("error", err))
		return
	}
	defer func() { _ = tconn.Close() }()

	pipeRaw(conn, tconn)
}

// pipeRaw streams bytes between a public connection and a tunnel stream until
// either side closes, then tears both ends down so the other copy unblocks.
func pipeRaw(client net.Conn, tconn core.TunnelConn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(tconn, client)
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
