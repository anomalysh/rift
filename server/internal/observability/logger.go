// Package observability wires structured logging.
package observability

import (
	"io"
	"log/slog"

	"github.com/anomaly-sh/rift/server/internal/config"
)

// NewLogger builds the process logger described by cfg.
func NewLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.Log.Level}

	var h slog.Handler
	if cfg.Log.Format == config.LogFormatText {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}

	return slog.New(h).With(
		slog.String("node_id", cfg.NodeID),
		slog.String("env", cfg.Env),
	)
}
