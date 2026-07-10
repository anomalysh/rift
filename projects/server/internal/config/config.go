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
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// Config is the fully validated server configuration.
type Config struct {
	Env    string
	NodeID string

	Log       Log
	Ingress   Ingress
	Gateway   Gateway
	Admin     Admin
	Postgres  Postgres
	Redis     Redis
	Cluster   Cluster
	TLS       TLS
	TCP       TCP
	UDP       UDP
	TLSTunnel TLSTunnel
	GRPC      GRPC
	Tunnel    Tunnel

	// SubdomainRules is derived from Tunnel settings and validated at boot.
	SubdomainRules *core.SubdomainRules

	// Warnings are legal settings worth saying out loud. The caller logs them
	// once a logger exists; they never prevent a boot.
	Warnings []string
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
	// ErrorPageDir is a directory of branded error pages (T4). Empty disables
	// the feature and the built-in plain-text/JSON error bodies are served.
	ErrorPageDir string
}

// Gateway is the WebSocket listener that tunnel agents dial.
type Gateway struct {
	Addr string
	// Hostname is the public name agents dial (e.g. gateway.rift.example.com).
	// It is not used for routing; the TLS-ask endpoint authorizes a
	// certificate for it, since it is not a tunnel subdomain and would
	// otherwise be refused.
	Hostname         string
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

// TLS describes how the reverse proxy in front of this server obtains
// certificates. This server never terminates TLS itself; it validates the
// settings so that a misconfiguration is a boot failure rather than a
// handshake failure discovered by a visitor.
type TLS struct {
	Mode string
	// ACMEDNSProvider names the Caddy DNS solver, e.g. rfc2136 or acmedns.
	// Required when Mode is dns01.
	ACMEDNSProvider string
	// CertFile and KeyFile are paths inside the proxy container.
	// Required when Mode is self.
	CertFile string
	KeyFile  string
}

// PubliclyTrusted reports whether the mode yields certificates a stock client
// trusts without extra configuration.
func (t TLS) PubliclyTrusted() bool {
	return t.Mode == TLSModeDNS01 || t.Mode == TLSModeHTTP01
}

// CoversUnknownSubdomains reports whether a hostname with no live tunnel can
// still complete a TLS handshake, and so receive a readable 404 instead of a
// protocol error.
func (t TLS) CoversUnknownSubdomains() bool {
	return t.Mode != TLSModeHTTP01
}

// TCP configures raw TCP tunnels. When enabled the gateway allocates a public
// port from [PortMin, PortMax] for each tcp tunnel, listens on ListenHost, and
// tells the agent to advertise AdvertiseHost:port (AdvertiseHost defaults to
// the base domain). The port range must be opened on the host firewall.
type TCP struct {
	Enabled       bool
	ListenHost    string
	AdvertiseHost string
	PortMin       int
	PortMax       int
	// NoDelay disables Nagle's algorithm on accepted connections (P1).
	NoDelay bool
	// KeepAliveSeconds is the TCP keep-alive period on accepted connections;
	// 0 disables keep-alives (P1).
	KeepAliveSeconds int
}

// Advertise returns the public host clients dial for a tcp tunnel, falling back
// to the base domain when no explicit advertise host is set.
func (t TCP) Advertise(baseDomain string) string {
	if t.AdvertiseHost != "" {
		return t.AdvertiseHost
	}
	return baseDomain
}

// UDP configures raw UDP tunnels (P4). When enabled the gateway allocates a
// public port from [PortMin, PortMax] for each udp tunnel, binds it on
// ListenHost, and forwards each client flow's datagrams to the agent. The port
// range must be opened on the host firewall.
type UDP struct {
	Enabled       bool
	ListenHost    string
	AdvertiseHost string
	PortMin       int
	PortMax       int
	// FlowTimeout retires a client flow after this idle period.
	FlowTimeout time.Duration
}

// Advertise returns the public host clients dial for a udp tunnel, falling back
// to the base domain when no explicit advertise host is set.
func (u UDP) Advertise(baseDomain string) string {
	if u.AdvertiseHost != "" {
		return u.AdvertiseHost
	}
	return baseDomain
}

// TLSTunnel configures raw TLS passthrough tunnels. When enabled the gateway
// listens on ListenAddr, reads each connection's ClientHello SNI to find the
// tls tunnel on that subdomain, and pipes the still-encrypted bytes through to
// the agent, whose local service terminates TLS. The listen port must be opened
// on the host firewall.
type TLSTunnel struct {
	Enabled    bool
	ListenAddr string
	// AdvertisePort is the public port clients dial (sub.<base>:port). Defaults
	// to the port in ListenAddr when zero.
	AdvertisePort int
}

// Port returns the public port a tls tunnel is advertised on, falling back to
// the port in ListenAddr.
func (t TLSTunnel) Port() int {
	if t.AdvertisePort > 0 {
		return t.AdvertisePort
	}
	if _, p, err := net.SplitHostPort(t.ListenAddr); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 0
}

// GRPC configures cleartext-HTTP/2 (h2c) gRPC tunnels (P7). When enabled the
// gateway listens on ListenAddr, reads each connection's first HTTP/2 HEADERS
// frame to route by its :authority to the grpc tunnel on that subdomain, and
// pipes the raw h2c bytes through -- so streaming and trailers (grpc-status)
// are preserved end to end. The agent relays to the local h2c gRPC server.
type GRPC struct {
	Enabled    bool
	ListenAddr string
	// AdvertisePort is the public port clients dial (sub.<base>:port). Defaults
	// to the port in ListenAddr when zero.
	AdvertisePort int
}

// Port returns the public port a grpc tunnel is advertised on, falling back to
// the port in ListenAddr.
func (g GRPC) Port() int {
	if g.AdvertisePort > 0 {
		return g.AdvertisePort
	}
	if _, p, err := net.SplitHostPort(g.ListenAddr); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	return 0
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
	// TokenRevalidateInterval bounds how long a revoked token's existing
	// tunnels keep serving before the gateway closes them.
	TokenRevalidateInterval time.Duration

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
			ErrorPageDir:    l.str(KeyErrorPageDir, ""),
		},
		Gateway: Gateway{
			Addr:             l.str(KeyGatewayAddr, DefaultGatewayAddr),
			Hostname:         core.NormalizeSubdomain(l.str(KeyGatewayHostname, "")),
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
		TCP: TCP{
			Enabled:          l.boolean(KeyTCPEnabled, DefaultTCPEnabled),
			ListenHost:       l.str(KeyTCPListenHost, DefaultTCPListenHost),
			AdvertiseHost:    l.str(KeyTCPAdvertiseHost, ""),
			PortMin:          l.integer(KeyTCPPortMin, DefaultTCPPortMin),
			PortMax:          l.integer(KeyTCPPortMax, DefaultTCPPortMax),
			NoDelay:          l.boolean(KeyTCPNoDelay, DefaultTCPNoDelay),
			KeepAliveSeconds: l.integer(KeyTCPKeepAliveSeconds, DefaultTCPKeepAliveSeconds),
		},
		UDP: UDP{
			Enabled:       l.boolean(KeyUDPEnabled, DefaultUDPEnabled),
			ListenHost:    l.str(KeyUDPListenHost, DefaultUDPListenHost),
			AdvertiseHost: l.str(KeyUDPAdvertiseHost, ""),
			PortMin:       l.integer(KeyUDPPortMin, DefaultUDPPortMin),
			PortMax:       l.integer(KeyUDPPortMax, DefaultUDPPortMax),
			FlowTimeout:   l.duration(KeyUDPFlowTimeout, DefaultUDPFlowTimeout),
		},
		TLSTunnel: TLSTunnel{
			Enabled:       l.boolean(KeyTLSTunnelEnabled, DefaultTLSTunnelEnabled),
			ListenAddr:    l.str(KeyTLSTunnelListenAddr, DefaultTLSTunnelListenAddr),
			AdvertisePort: l.integer(KeyTLSTunnelAdvertisePort, 0),
		},
		GRPC: GRPC{
			Enabled:       l.boolean(KeyGRPCEnabled, DefaultGRPCEnabled),
			ListenAddr:    l.str(KeyGRPCListenAddr, DefaultGRPCListenAddr),
			AdvertisePort: l.integer(KeyGRPCAdvertisePort, 0),
		},
		TLS: TLS{
			// Deliberately no default here: development gets one below,
			// production must say what it means.
			Mode:            l.str(KeyTLSMode, ""),
			ACMEDNSProvider: l.str(KeyACMEDNSProvider, ""),
			CertFile:        l.str(KeyTLSCertFile, ""),
			KeyFile:         l.str(KeyTLSKeyFile, ""),
		},
		Tunnel: Tunnel{
			BaseDomain:   l.requiredStr(KeyBaseDomain),
			PublicScheme: l.str(KeyPublicScheme, DefaultPublicScheme),
			AdvertiseURL: l.str(KeyNodeAdvertiseURL, ""),

			HeartbeatInterval:       l.duration(KeyHeartbeatInterval, DefaultHeartbeatInterval),
			HeartbeatTimeout:        l.duration(KeyHeartbeatTimeout, DefaultHeartbeatTimeout),
			ReaperInterval:          l.duration(KeyReaperInterval, DefaultReaperInterval),
			TokenRevalidateInterval: l.duration(KeyTokenRevalidateInterval, DefaultTokenRevalidateInterval),

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
		l.csvAppend(KeySubdomainBlocklist, DefaultSubdomainBlocklist),
		l.str(KeySubdomainGenerator, DefaultSubdomainGenerator),
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
	cfg.Warnings = l.warns
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

// validateTLS enforces the TLS mode contract.
//
// The mode is required in production and has no fallback there. A silent
// fallback to an untrusted certificate is the worst outcome available: the
// handshake succeeds, so nothing looks broken, and the operator learns months
// later that clients have been clicking through a warning. An unset mode is
// therefore a boot failure, not a default.
func (c *Config) validateTLS(l *loader) {
	if c.TLS.Mode == "" {
		if c.Production() {
			l.fail(KeyTLSMode, fmt.Errorf(
				"is required in %s; expected one of %s. There is no default: an unset mode would silently serve untrusted certificates",
				EnvProduction, strings.Join(TLSModes, ", ")))
			return
		}
		c.TLS.Mode = DefaultTLSMode
	}

	valid := false
	for _, m := range TLSModes {
		if c.TLS.Mode == m {
			valid = true
			break
		}
	}
	if !valid {
		l.fail(KeyTLSMode, fmt.Errorf("expected one of %s, got %q", strings.Join(TLSModes, ", "), c.TLS.Mode))
		return
	}

	switch c.TLS.Mode {
	case TLSModeDNS01:
		if c.TLS.ACMEDNSProvider == "" {
			l.fail(KeyACMEDNSProvider, fmt.Errorf(
				"is required when %s is %q; it names the Caddy DNS solver, e.g. rfc2136 or acmedns",
				KeyTLSMode, TLSModeDNS01))
		}
	case TLSModeSelf:
		if c.TLS.CertFile == "" {
			l.fail(KeyTLSCertFile, fmt.Errorf("is required when %s is %q", KeyTLSMode, TLSModeSelf))
		}
		if c.TLS.KeyFile == "" {
			l.fail(KeyTLSKeyFile, fmt.Errorf("is required when %s is %q", KeyTLSMode, TLSModeSelf))
		}
	}

	// A public scheme of https with an internal CA is legal — an operator may
	// distribute their own root — but it is worth saying out loud.
	if c.Production() && !c.TLS.PubliclyTrusted() {
		l.warn(KeyTLSMode, fmt.Sprintf(
			"%q does not produce publicly trusted certificates; clients must trust your CA or the handshake will fail",
			c.TLS.Mode))
	}
}

func (c *Config) validate(l *loader) {
	c.validateTLS(l)

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
			l.fail(KeyBaseDomain, fmt.Errorf("expected a fully qualified domain such as rift.example.com, got %q", base))
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

	if c.TCP.Enabled {
		if c.TCP.PortMin < 1 || c.TCP.PortMax > 65535 {
			l.fail(KeyTCPPortMin, fmt.Errorf("port range %d-%d must fall within 1-65535", c.TCP.PortMin, c.TCP.PortMax))
		} else if c.TCP.PortMin > c.TCP.PortMax {
			l.fail(KeyTCPPortMin, fmt.Errorf("must not exceed %s: got %d > %d", KeyTCPPortMax, c.TCP.PortMin, c.TCP.PortMax))
		}
	}

	if c.TLSTunnel.Enabled {
		if _, _, err := net.SplitHostPort(c.TLSTunnel.ListenAddr); err != nil {
			l.fail(KeyTLSTunnelListenAddr, fmt.Errorf("expected host:port such as :8443, got %q", c.TLSTunnel.ListenAddr))
		}
		if p := c.TLSTunnel.Port(); p < 1 || p > 65535 {
			l.fail(KeyTLSTunnelListenAddr, fmt.Errorf("could not determine a valid advertise port (set %s)", KeyTLSTunnelAdvertisePort))
		}
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
