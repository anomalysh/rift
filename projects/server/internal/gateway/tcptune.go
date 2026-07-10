package gateway

import (
	"log/slog"
	"net"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/config"
)

// tuneTCPConn applies the P1 socket options to an accepted public connection:
// TCP_NODELAY (Nagle off) and a keep-alive period. Both raw tcp and SNI-routed
// tls tunnels carry latency-sensitive byte streams, so both accept paths call
// this. A conn that is not a *net.TCPConn (only in tests) is left untouched.
func tuneTCPConn(conn net.Conn, cfg config.TCP, logger *slog.Logger) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	if err := tc.SetNoDelay(cfg.NoDelay); err != nil {
		logger.Debug("set TCP_NODELAY failed", slog.Any("error", err))
	}
	if cfg.KeepAliveSeconds <= 0 {
		if err := tc.SetKeepAlive(false); err != nil {
			logger.Debug("disable keep-alive failed", slog.Any("error", err))
		}
		return
	}
	if err := tc.SetKeepAlive(true); err != nil {
		logger.Debug("enable keep-alive failed", slog.Any("error", err))
		return
	}
	if err := tc.SetKeepAlivePeriod(time.Duration(cfg.KeepAliveSeconds) * time.Second); err != nil {
		logger.Debug("set keep-alive period failed", slog.Any("error", err))
	}
}
