package config

import "time"

// Defaults for every optional setting. A value appears here or it is required;
// there is no third category, and no literal is duplicated in Load.
const (
	DefaultEnv       = EnvDevelopment
	DefaultLogLevel  = "info"
	DefaultLogFormat = LogFormatJSON

	DefaultIngressAddr           = ":8080"
	DefaultIngressReadTimeout    = 30 * time.Second
	DefaultIngressWriteTimeout   = 0 // 0 = no write deadline; streamed responses may run long
	DefaultIngressIdleTimeout    = 120 * time.Second
	DefaultIngressMaxHeaderBytes = 1 << 20 // 1 MiB

	DefaultGatewayAddr             = ":8081"
	DefaultGatewayPath             = "/tunnel"
	DefaultGatewayHandshakeTimeout = 10 * time.Second
	DefaultGatewayWriteTimeout     = 30 * time.Second

	DefaultAdminAddr    = ":8082"
	DefaultAdminEnabled = true

	DefaultPostgresMaxConns       = 10
	DefaultPostgresMinConns       = 2
	DefaultPostgresConnectTimeout = 10 * time.Second
	DefaultPostgresMigrateOnStart = true

	DefaultRedisEnabled = false
	DefaultRedisAddr    = "127.0.0.1:6379"
	DefaultRedisDB      = 0
	DefaultRedisPrefix  = "rift:"

	DefaultTCPEnabled    = false
	DefaultTCPListenHost = "0.0.0.0"
	// A 101-port default range; the operator must open it on the host firewall.
	DefaultTCPPortMin = 20000
	DefaultTCPPortMax = 20100
	// Nagle off by default: interactive byte streams (SSH, database wire
	// protocols) care more about latency than about coalescing tiny writes.
	DefaultTCPNoDelay = true
	// A 30s keep-alive prunes half-open connections through NAT/firewalls
	// without being so aggressive it wastes packets. 0 disables it.
	DefaultTCPKeepAliveSeconds = 30

	DefaultTLSTunnelEnabled = false
	// A dedicated port so passthrough TLS does not collide with the reverse
	// proxy on 443; the operator must open it on the host firewall.
	DefaultTLSTunnelListenAddr = ":8443"

	DefaultPublicScheme = SchemeHTTPS

	DefaultHeartbeatInterval = 15 * time.Second
	// Timeout is deliberately > 2x interval so one dropped heartbeat on a
	// congested link does not reap a healthy tunnel.
	DefaultHeartbeatTimeout = 45 * time.Second
	DefaultReaperInterval   = 30 * time.Second

	// A revoked token should stop serving traffic promptly, but re-reading it
	// on every heartbeat would multiply database load by the tunnel count.
	DefaultTokenRevalidateInterval = 30 * time.Second

	DefaultRequestTimeout      = 60 * time.Second
	DefaultMaxRequestBodyBytes = int64(32 << 20) // 32 MiB
	DefaultMaxTunnelsPerToken  = 5
	DefaultStreamBufferSize    = 32

	DefaultSubdomainMinLength = 3
	DefaultSubdomainMaxLength = 63
	// Lowercase alphanumeric with internal hyphens; no leading/trailing hyphen.
	DefaultSubdomainPattern = `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`
	// DefaultSubdomainGenerator is "words": friendly adjective-noun-number
	// labels (swift-otter-42), like Heroku and Docker. "random" gives the
	// compact alphanumeric form below. The literal mirrors core.GeneratorWords,
	// which this package must not import (defaults hold no cross-package refs).
	DefaultSubdomainGenerator = "words"
	DefaultSubdomainGenLength = 10
	// Ambiguous glyphs (0/o, 1/l/i) are omitted so a subdomain can be read
	// aloud or copied off a terminal without transcription errors.
	DefaultSubdomainGenAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
)

// DefaultSubdomainBlocklist protects labels that would shadow infrastructure or
// be mistaken for official endpoints. RIFT_SUBDOMAIN_BLOCKLIST adds to this
// list; it cannot remove from it. These entries are a safety floor, not a
// suggestion: `gateway` in particular is routed to the agent endpoint by Caddy,
// so a tunnel holding it would be both unreachable and misleading.
var DefaultSubdomainBlocklist = []string{
	"www", "api", "admin", "app", "dashboard", "console",
	"mail", "smtp", "imap", "pop", "ns", "ns1", "ns2", "dns",
	"static", "assets", "cdn", "img", "images", "media",
	"auth", "login", "signup", "account", "billing", "payment",
	"status", "health", "metrics", "internal", "gateway",
	"support", "help", "docs", "blog", "test", "staging", "dev",
	"root", "system", "security", "abuse", "postmaster", "webmaster",
	"rift", "tunnel", "localhost",
}

// Environment names.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

// Log formats.
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Public URL schemes.
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// TLS modes. Each selects a different Caddyfile snippet under deploy/caddy/modes.
const (
	// TLSModeDNS01 obtains one wildcard certificate over the DNS-01 challenge.
	// Every subdomain is then covered, so an unknown tunnel answers with a 404
	// over valid TLS rather than failing the handshake. Requires a DNS provider.
	TLSModeDNS01 = "dns01"

	// TLSModeHTTP01 issues a certificate per hostname, on first contact, over
	// HTTP-01, gated by the TLS-ask endpoint. Needs no DNS credentials, but a
	// hostname that has never served a tunnel has no certificate and so cannot
	// complete a TLS handshake at all.
	TLSModeHTTP01 = "http01"

	// TLSModeSelf serves a certificate and key the operator supplies. No ACME,
	// no renewal: both are the operator's responsibility.
	TLSModeSelf = "self"

	// TLSModeInternal signs certificates with Caddy's own CA. Nothing is
	// publicly trusted. Correct for local development and the e2e harness, and
	// for a deployment whose clients already trust an internal root.
	TLSModeInternal = "internal"
)

// DefaultTLSMode applies in development only. Production must name a mode
// explicitly; see Config.validate.
const DefaultTLSMode = TLSModeInternal

// TLSModes lists every valid mode, for error messages and validation.
var TLSModes = []string{TLSModeDNS01, TLSModeHTTP01, TLSModeSelf, TLSModeInternal}
