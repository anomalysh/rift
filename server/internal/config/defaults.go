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
	DefaultRedisPrefix  = "tunl:"

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
	DefaultSubdomainPattern   = `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`
	DefaultSubdomainGenLength = 10
	// Ambiguous glyphs (0/o, 1/l/i) are omitted so a subdomain can be read
	// aloud or copied off a terminal without transcription errors.
	DefaultSubdomainGenAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"
)

// DefaultSubdomainBlocklist protects labels that would shadow infrastructure
// or be mistaken for official endpoints. Operators extend this via config.
var DefaultSubdomainBlocklist = []string{
	"www", "api", "admin", "app", "dashboard", "console",
	"mail", "smtp", "imap", "pop", "ns", "ns1", "ns2", "dns",
	"static", "assets", "cdn", "img", "images", "media",
	"auth", "login", "signup", "account", "billing", "payment",
	"status", "health", "metrics", "internal", "gateway",
	"support", "help", "docs", "blog", "test", "staging", "dev",
	"root", "system", "security", "abuse", "postmaster", "webmaster",
	"tunl", "tunnel", "localhost",
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
