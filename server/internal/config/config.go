// Package config loads and validates every tunable the server reads.
//
// Rules this package exists to enforce:
//   - no setting is spelled anywhere but keys.go
//   - no default is written anywhere but defaults.go
//   - a value is either defaulted or required; a missing required value is a
//     boot failure, never a silent zero
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/siliconcolony/tunl/server/internal/core"
)

// Config is the fully validated server configuration.
type Config struct {
	Env    string
	NodeID string

	Log      Log
	Ingress  Ingress
	Gateway  Gateway
	Admin    Admin
	Postgres Postgres
	Redis    Redis
	Cluster  Cluster
	Tunnel   Tunnel

	// SubdomainRules is derived from Tunnel settings and validated at boot.
	SubdomainRules *core.SubdomainRules
}

// Production reports whether the server runs with production guardrails.
func (c *Config) Production() bool { return c.Env == EnvProduction }

// Log controls structured logging.
type Log struct {
	Level  slog.Level
	Format string
}

// Ingress is the public HTTP listener that terminates proxied traffic.
type Ingress struct {
	Addr           string
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	IdleTimeout    time.Duration
	MaxHeaderBytes int
	// TrustedProxyIPs are the peers whose X-Forwarded-For we believe. Empty
	// means trust nobody and use the socket's remote address.
	TrustedProxyIPs []string
}

// Gateway is the WebSocket listener that tunnel agents dial.
type Gateway struct {
	Addr             string
	Path             string
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	// AllowedOrigins guards browser-originated upgrades. Agents are not
	// browsers and send no Origin, so this is empty by default.
	AllowedOrigins []string
}

// Admin is the management API listener.
type Admin struct {
	Enabled bool
	Addr    string
	// Token authenticates admin callers. Required when Enabled.
	Token string
}

// Postgres is the primary datastore.
type Postgres struct {
	DSN            string
	MaxConns       int
	MinConns       int
	ConnectTimeout time.Duration
	MigrateOnStart bool
}

// Redis is optional. When disabled the server runs single-node.
type Redis struct {
	Enabled  bool
	Addr     string
	Password string
	DB       int
	Prefix   string
}

// Cluster covers node-to-node concerns. Only meaningful when Redis is enabled.
type Cluster struct {
	// PeerSecret authenticates the internal proxy route between nodes.
	PeerSecret string
}

// Tunnel holds the behavioural knobs of the tunnelling layer.
type Tunnel struct {
	BaseDomain   string
	PublicScheme string
	// AdvertiseURL is how *other* gateway nodes reach this node's internal
	// proxy endpoint. Required only when Redis is enabled.
	AdvertiseURL string

	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	ReaperInterval    time.Duration

	RequestTimeout      time.Duration
	MaxRequestBodyBytes int64
	MaxTunnelsPerToken  int
	StreamBufferSize    int
}

// PublicURL renders the browser-visible URL for a subdomain.
func (t Tunnel) PublicURL(subdomain string) string {
	return t.PublicScheme + "://" + core.Hostname(subdomain, t.BaseDomain)
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	l := &loader{}

	cfg := &Config{
		Env:    l.str(KeyEnv, DefaultEnv),
		NodeID: l.str(KeyNodeID, ""), // generated below when unset

		Log: Log{
			Format: l.str(KeyLogFormat, DefaultLogFormat),
		},
		Ingress: Ingress{
			Addr:            l.str(KeyIngressAddr, DefaultIngressAddr),
			ReadTimeout:     l.duration(KeyIngressReadTimeout, DefaultIngressReadTimeout),
			IdleTimeout:     l.duration(KeyIngressIdleTimeout, DefaultIngressIdleTimeout),
			MaxHeaderBytes:  l.integer(KeyIngressMaxHeaderBytes, DefaultIngressMaxHeaderBytes),
			TrustedProxyIPs: l.csv(KeyIngressTrustedProxyIPs, nil),
		},
		Gateway: Gateway{
			Addr:             l.str(KeyGatewayAddr, DefaultGatewayAddr),
			Path:             l.str(KeyGatewayPath, DefaultGatewayPath),
			HandshakeTimeout: l.duration(KeyGatewayHandshakeTimeout, DefaultGatewayHandshakeTimeout),
			WriteTimeout:     l.duration(KeyGatewayWriteTimeout, DefaultGatewayWriteTimeout),
			AllowedOrigins:   l.csv(KeyGatewayAllowedOrigins, nil),
		},
		Admin: Admin{
			Enabled: l.boolean(KeyAdminEnabled, DefaultAdminEnabled),
			Addr:    l.str(KeyAdminAddr, DefaultAdminAddr),
		},
		Postgres: Postgres{
			DSN:            l.requiredStr(KeyPostgresDSN),
			MaxConns:       l.integer(KeyPostgresMaxConns, DefaultPostgresMaxConns),
			MinConns:       l.integer(KeyPostgresMinConns, DefaultPostgresMinConns),
			ConnectTimeout: l.duration(KeyPostgresConnectTimeout, DefaultPostgresConnectTimeout),
			MigrateOnStart: l.boolean(KeyPostgresMigrateOnStart, DefaultPostgresMigrateOnStart),
		},
		Redis: Redis{
			Enabled:  l.boolean(KeyRedisEnabled, DefaultRedisEnabled),
			Addr:     l.str(KeyRedisAddr, DefaultRedisAddr),
			Password: l.str(KeyRedisPass, ""),
			DB:       l.integer(KeyRedisDB, DefaultRedisDB),
			Prefix:   l.str(KeyRedisPrefix, DefaultRedisPrefix),
		},
		Cluster: Cluster{
			PeerSecret: l.str(KeyPeerSecret, ""),
		},
		Tunnel: Tunnel{
			BaseDomain:   l.requiredStr(KeyBaseDomain),
			PublicScheme: l.str(KeyPublicScheme, DefaultPublicScheme),
			AdvertiseURL: l.str(KeyNodeAdvertiseURL, ""),

			HeartbeatInterval: l.duration(KeyHeartbeatInterval, DefaultHeartbeatInterval),
			HeartbeatTimeout:  l.duration(KeyHeartbeatTimeout, DefaultHeartbeatTimeout),
			ReaperInterval:    l.duration(KeyReaperInterval, DefaultReaperInterval),

			RequestTimeout:      l.duration(KeyRequestTimeout, DefaultRequestTimeout),
			MaxRequestBodyBytes: l.integer64(KeyMaxRequestBodyBytes, DefaultMaxRequestBodyBytes),
			MaxTunnelsPerToken:  l.atLeast(KeyMaxTunnelsPerToken, l.integer(KeyMaxTunnelsPerToken, DefaultMaxTunnelsPerToken), 1),
			StreamBufferSize:    l.atLeast(KeyStreamBufferSize, l.integer(KeyStreamBufferSize, DefaultStreamBufferSize), 1),
		},
	}

	// Write timeout of 0 disables the deadline, which is what we want for
	// long-lived streamed responses, so it bypasses the positive-duration check.
	cfg.Ingress.WriteTimeout = optionalDuration(l, KeyIngressWriteTimeout, DefaultIngressWriteTimeout)

	cfg.Log.Level = parseLogLevel(l, l.str(KeyLogLevel, DefaultLogLevel))

	// A stable node ID lets a restarted process reclaim the tunnel rows it
	// owned before the crash. Operators set it explicitly in multi-node
	// deployments; a fresh random one is correct for single-node.
	if cfg.NodeID == "" {
		id, err := core.NewID(time.Now())
		if err != nil {
			l.fail(KeyNodeID, fmt.Errorf("could not generate a node id: %w", err))
		}
		cfg.NodeID = id
	}

	if cfg.Admin.Enabled {
		cfg.Admin.Token = l.requiredStr(KeyAdminToken)
	} else {
		cfg.Admin.Token = l.str(KeyAdminToken, "")
	}

	rules, err := core.NewSubdomainRules(
		l.integer(KeySubdomainMinLength, DefaultSubdomainMinLength),
		l.integer(KeySubdomainMaxLength, DefaultSubdomainMaxLength),
		l.str(KeySubdomainPattern, DefaultSubdomainPattern),
		l.csv(KeySubdomainBlocklist, DefaultSubdomainBlocklist),
		l.integer(KeySubdomainGenLength, DefaultSubdomainGenLength),
		l.str(KeySubdomainGenAlphabet, DefaultSubdomainGenAlphabet),
	)
	if err != nil {
		l.fail(KeySubdomainPattern, err)
	}
	cfg.SubdomainRules = rules

	cfg.validate(l)

	if err := l.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// optionalDuration parses a duration that may legitimately be zero.
func optionalDuration(l *loader, key string, def time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail(key, fmt.Errorf("expected a duration such as 30s, got %q", v))
		return def
	}
	if d < 0 {
		l.fail(key, fmt.Errorf("must not be negative, got %q", v))
		return def
	}
	return d
}

func parseLogLevel(l *loader, s string) slog.Level {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(s)); err != nil {
		l.fail(KeyLogLevel, fmt.Errorf("expected one of debug, info, warn, error; got %q", s))
		return slog.LevelInfo
	}
	return lvl
}

func (c *Config) validate(l *loader) {
	switch c.Env {
	case EnvDevelopment, EnvProduction:
	default:
		l.fail(KeyEnv, fmt.Errorf("expected %q or %q, got %q", EnvDevelopment, EnvProduction, c.Env))
	}

	switch c.Log.Format {
	case LogFormatJSON, LogFormatText:
	default:
		l.fail(KeyLogFormat, fmt.Errorf("expected %q or %q, got %q", LogFormatJSON, LogFormatText, c.Log.Format))
	}

	switch c.Tunnel.PublicScheme {
	case SchemeHTTP, SchemeHTTPS:
	default:
		l.fail(KeyPublicScheme, fmt.Errorf("expected %q or %q, got %q", SchemeHTTP, SchemeHTTPS, c.Tunnel.PublicScheme))
	}

	if base := c.Tunnel.BaseDomain; base != "" {
		if !strings.Contains(base, ".") || strings.HasPrefix(base, ".") || strings.HasSuffix(base, ".") {
			l.fail(KeyBaseDomain, fmt.Errorf("expected a fully qualified domain such as tunl.example.com, got %q", base))
		}
		if strings.ContainsAny(base, "/:") {
			l.fail(KeyBaseDomain, fmt.Errorf("must be a bare domain without scheme or port, got %q", base))
		}
	}

	// A heartbeat timeout at or below the interval reaps healthy tunnels the
	// moment a single heartbeat is delayed.
	if c.Tunnel.HeartbeatTimeout <= c.Tunnel.HeartbeatInterval {
		l.fail(KeyHeartbeatTimeout, fmt.Errorf(
			"must exceed %s (%s); otherwise a single late heartbeat reaps a live tunnel",
			KeyHeartbeatInterval, c.Tunnel.HeartbeatInterval))
	}

	if c.Postgres.MinConns > c.Postgres.MaxConns {
		l.fail(KeyPostgresMinConns, fmt.Errorf("must not exceed %s (%d)", KeyPostgresMaxConns, c.Postgres.MaxConns))
	}
	if c.Postgres.MaxConns < 1 {
		l.fail(KeyPostgresMaxConns, fmt.Errorf("must be >= 1, got %d", c.Postgres.MaxConns))
	}

	if c.Tunnel.MaxRequestBodyBytes < 0 {
		l.fail(KeyMaxRequestBodyBytes, fmt.Errorf("must be >= 0 (0 means unlimited), got %d", c.Tunnel.MaxRequestBodyBytes))
	}

	if !strings.HasPrefix(c.Gateway.Path, "/") {
		l.fail(KeyGatewayPath, fmt.Errorf("must begin with '/', got %q", c.Gateway.Path))
	}

	// Multi-node routing needs a reachable address for peers to forward to.
	if c.Redis.Enabled {
		if c.Tunnel.AdvertiseURL == "" {
			l.fail(KeyNodeAdvertiseURL, fmt.Errorf("is required when %s is true, so peer nodes can forward requests here", KeyRedisEnabled))
		} else if u, err := url.Parse(c.Tunnel.AdvertiseURL); err != nil || u.Scheme == "" || u.Host == "" {
			l.fail(KeyNodeAdvertiseURL, fmt.Errorf("expected an absolute URL such as http://10.0.0.4:8080, got %q", c.Tunnel.AdvertiseURL))
		}
		if c.Redis.Addr == "" {
			l.fail(KeyRedisAddr, fmt.Errorf("is required when %s is true", KeyRedisEnabled))
		}
		// Without a shared secret the internal proxy route would let anyone
		// who can reach the ingress port impersonate a peer node and reach
		// any tunnel by name.
		const minPeerSecretLen = 32
		if len(c.Cluster.PeerSecret) < minPeerSecretLen {
			l.fail(KeyPeerSecret, fmt.Errorf("is required when %s is true and must be at least %d characters", KeyRedisEnabled, minPeerSecretLen))
		}
	}

	// An admin API on a public interface with a weak token is a takeover.
	if c.Admin.Enabled && c.Production() {
		const minAdminTokenLen = 32
		if len(c.Admin.Token) < minAdminTokenLen {
			l.fail(KeyAdminToken, fmt.Errorf("must be at least %d characters in %s", minAdminTokenLen, EnvProduction))
		}
	}
}
