// Command riftd is the rift gateway: it terminates agent WebSocket
// connections, serves public traffic for *.BASE_DOMAIN, and exposes an admin
// API on a separate, non-public listener.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/anomalysh/rift/server/internal/adminapi"
	"github.com/anomalysh/rift/server/internal/config"
	"github.com/anomalysh/rift/server/internal/gateway"
	"github.com/anomalysh/rift/server/internal/ingress"
	"github.com/anomalysh/rift/server/internal/observability"
	"github.com/anomalysh/rift/server/internal/reaper"
	"github.com/anomalysh/rift/server/internal/registry"
	"github.com/anomalysh/rift/server/internal/store/postgres"
)

// shutdownGrace bounds how long in-flight public requests may finish before
// the process exits.
const shutdownGrace = 20 * time.Second

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when config loading fails.
		fmt.Fprintf(os.Stderr, "riftd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := observability.NewLogger(cfg, os.Stdout)
	slog.SetDefault(logger)

	for _, w := range cfg.Warnings {
		logger.Warn("configuration warning", slog.String("detail", w))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer db.Close()

	if cfg.Postgres.MigrateOnStart {
		if err := db.Migrate(ctx); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
		logger.Info("migrations applied")
	}

	// Rows this node owned before a crash name subdomains no live session
	// holds. Clearing them on boot returns those subdomains immediately
	// instead of waiting out a heartbeat timeout.
	if n, err := db.Tunnels().DeleteByNode(ctx, cfg.NodeID); err != nil {
		logger.Warn("could not clear tunnels left by a previous run", slog.Any("error", err))
	} else if n > 0 {
		logger.Info("cleared tunnels left by a previous run", slog.Int("count", n))
	}

	reg, err := registry.New(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer func() { _ = reg.Close() }()

	gw := gateway.New(cfg, logger, db.Tokens(), db.Reservations(), db.Tunnels(), reg)
	ing := ingress.New(cfg, logger, reg, db.Tunnels(), db.Reservations())
	// A node that cannot reach Postgres cannot authorize a handshake or claim a
	// subdomain. Readiness says so; liveness deliberately does not.
	ing.SetReadyCheck(db.Ping)

	gwMux := http.NewServeMux()
	gwMux.Handle(cfg.Gateway.Path, gw.Handler())
	gwMux.HandleFunc(config.RouteHealth, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	servers := []*namedServer{
		{
			name: "ingress",
			srv: &http.Server{
				Addr:              cfg.Ingress.Addr,
				Handler:           ing.Handler(),
				ReadHeaderTimeout: cfg.Ingress.ReadTimeout,
				// No ReadTimeout or WriteTimeout by default: a tunnelled
				// upload or a server-sent-event stream may legitimately run
				// far longer than any fixed deadline.
				WriteTimeout:   cfg.Ingress.WriteTimeout,
				IdleTimeout:    cfg.Ingress.IdleTimeout,
				MaxHeaderBytes: cfg.Ingress.MaxHeaderBytes,
			},
		},
		{
			name: "gateway",
			srv: &http.Server{
				Addr:              cfg.Gateway.Addr,
				Handler:           gwMux,
				ReadHeaderTimeout: cfg.Gateway.HandshakeTimeout,
				// A tunnel is a long-lived connection; no read or write
				// deadline may apply to it.
			},
		},
	}

	if cfg.Admin.Enabled {
		servers = append(servers, &namedServer{
			name: "admin",
			srv: &http.Server{
				Addr:              cfg.Admin.Addr,
				Handler:           adminapi.New(cfg, db.Tokens(), db.Reservations(), db.Tunnels(), logger),
				ReadHeaderTimeout: cfg.Ingress.ReadTimeout,
				WriteTimeout:      cfg.Ingress.ReadTimeout,
			},
		})
	}

	rp := reaper.New(cfg, logger, db.Tunnels())
	go rp.Run(ctx)

	errCh := make(chan error, len(servers))
	var wg sync.WaitGroup
	for _, ns := range servers {
		ns := ns
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("listening", slog.String("server", ns.name), slog.String("addr", ns.srv.Addr))
			if err := ns.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server: %w", ns.name, err)
			}
		}()
	}

	logger.Info("riftd ready",
		slog.String("base_domain", cfg.Tunnel.BaseDomain),
		slog.String("gateway_path", cfg.Gateway.Path),
		slog.String("tls_mode", cfg.TLS.Mode),
		slog.Bool("tls_publicly_trusted", cfg.TLS.PubliclyTrusted()),
		slog.Bool("redis", cfg.Redis.Enabled),
		slog.Bool("admin", cfg.Admin.Enabled))

	select {
	case err := <-errCh:
		stop()
		shutdown(logger, servers, gw)
		wg.Wait()
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdown(logger, servers, gw)
		wg.Wait()
		return nil
	}
}

type namedServer struct {
	name string
	srv  *http.Server
}

// shutdown drains public traffic first, then closes the tunnels. Closing
// tunnels first would fail the very requests we are trying to drain.
func shutdown(logger *slog.Logger, servers []*namedServer, gw *gateway.Gateway) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	for _, ns := range servers {
		if ns.name == "gateway" {
			continue
		}
		if err := ns.srv.Shutdown(ctx); err != nil {
			logger.Warn("server did not drain cleanly", slog.String("server", ns.name), slog.Any("error", err))
		}
	}

	if err := gw.Shutdown(ctx); err != nil {
		logger.Warn("could not close tunnels", slog.Any("error", err))
	}
	for _, ns := range servers {
		if ns.name != "gateway" {
			continue
		}
		if err := ns.srv.Shutdown(ctx); err != nil {
			logger.Warn("gateway did not drain cleanly", slog.Any("error", err))
		}
	}
	logger.Info("riftd stopped")
}
