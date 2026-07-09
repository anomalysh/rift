package config

// Environment variable names. Every tunable the server reads is declared here
// exactly once; nothing else in the codebase may spell an env var inline.
const (
	// EnvPrefix namespaces every variable this server reads.
	EnvPrefix = "TUNL_"

	KeyEnv    = EnvPrefix + "ENV"
	KeyNodeID = EnvPrefix + "NODE_ID"

	KeyLogLevel  = EnvPrefix + "LOG_LEVEL"
	KeyLogFormat = EnvPrefix + "LOG_FORMAT"

	// Public ingress: serves proxied traffic for *.BASE_DOMAIN.
	KeyIngressAddr            = EnvPrefix + "INGRESS_ADDR"
	KeyIngressReadTimeout     = EnvPrefix + "INGRESS_READ_TIMEOUT"
	KeyIngressWriteTimeout    = EnvPrefix + "INGRESS_WRITE_TIMEOUT"
	KeyIngressIdleTimeout     = EnvPrefix + "INGRESS_IDLE_TIMEOUT"
	KeyIngressMaxHeaderBytes  = EnvPrefix + "INGRESS_MAX_HEADER_BYTES"
	KeyIngressTrustedProxyIPs = EnvPrefix + "INGRESS_TRUSTED_PROXY_IPS"

	// Gateway: WebSocket endpoint that tunnel agents dial.
	KeyGatewayAddr              = EnvPrefix + "GATEWAY_ADDR"
	KeyGatewayPath              = EnvPrefix + "GATEWAY_PATH"
	KeyGatewayHandshakeTimeout  = EnvPrefix + "GATEWAY_HANDSHAKE_TIMEOUT"
	KeyGatewayWriteTimeout      = EnvPrefix + "GATEWAY_WRITE_TIMEOUT"
	KeyGatewayAllowedOrigins    = EnvPrefix + "GATEWAY_ALLOWED_ORIGINS"

	// Admin API: token and reservation management.
	KeyAdminAddr    = EnvPrefix + "ADMIN_ADDR"
	KeyAdminToken   = EnvPrefix + "ADMIN_TOKEN"
	KeyAdminEnabled = EnvPrefix + "ADMIN_ENABLED"

	// Postgres.
	KeyPostgresDSN             = EnvPrefix + "POSTGRES_DSN"
	KeyPostgresMaxConns        = EnvPrefix + "POSTGRES_MAX_CONNS"
	KeyPostgresMinConns        = EnvPrefix + "POSTGRES_MIN_CONNS"
	KeyPostgresConnectTimeout  = EnvPrefix + "POSTGRES_CONNECT_TIMEOUT"
	KeyPostgresMigrateOnStart  = EnvPrefix + "POSTGRES_MIGRATE_ON_START"

	// Redis (optional; enables multi-node routing).
	KeyRedisEnabled = EnvPrefix + "REDIS_ENABLED"
	KeyRedisAddr    = EnvPrefix + "REDIS_ADDR"
	KeyRedisPass    = EnvPrefix + "REDIS_PASSWORD"
	KeyRedisDB      = EnvPrefix + "REDIS_DB"
	KeyRedisPrefix  = EnvPrefix + "REDIS_PREFIX"

	// KeyPeerSecret authenticates node-to-node request forwarding. Required
	// when Redis is enabled, because the internal proxy route would otherwise
	// let anyone who can reach the ingress port impersonate a peer node.
	KeyPeerSecret = EnvPrefix + "PEER_SECRET"

	// Tunnel behaviour.
	KeyBaseDomain          = EnvPrefix + "BASE_DOMAIN"
	KeyPublicScheme        = EnvPrefix + "PUBLIC_SCHEME"
	KeyNodeAdvertiseURL    = EnvPrefix + "NODE_ADVERTISE_URL"
	KeyHeartbeatInterval   = EnvPrefix + "HEARTBEAT_INTERVAL"
	KeyHeartbeatTimeout    = EnvPrefix + "HEARTBEAT_TIMEOUT"
	KeyReaperInterval      = EnvPrefix + "REAPER_INTERVAL"
	// KeyTokenRevalidateInterval controls how often a live tunnel re-checks
	// that its token is still valid, so revoking a token tears down the
	// tunnels it opened instead of only blocking new ones.
	KeyTokenRevalidateInterval = EnvPrefix + "TOKEN_REVALIDATE_INTERVAL"

	KeyRequestTimeout      = EnvPrefix + "REQUEST_TIMEOUT"
	KeyMaxRequestBodyBytes = EnvPrefix + "MAX_REQUEST_BODY_BYTES"
	KeyMaxTunnelsPerToken  = EnvPrefix + "MAX_TUNNELS_PER_TOKEN"
	KeyStreamBufferSize    = EnvPrefix + "STREAM_BUFFER_SIZE"

	// Subdomain rules.
	KeySubdomainMinLength    = EnvPrefix + "SUBDOMAIN_MIN_LENGTH"
	KeySubdomainMaxLength    = EnvPrefix + "SUBDOMAIN_MAX_LENGTH"
	KeySubdomainPattern      = EnvPrefix + "SUBDOMAIN_PATTERN"
	KeySubdomainBlocklist    = EnvPrefix + "SUBDOMAIN_BLOCKLIST"
	KeySubdomainGenLength    = EnvPrefix + "SUBDOMAIN_GENERATED_LENGTH"
	KeySubdomainGenAlphabet  = EnvPrefix + "SUBDOMAIN_GENERATED_ALPHABET"
)

// Route paths exposed by the server. Declared once so Caddy config, tests and
// handlers cannot drift apart.
const (
	// RouteHealth is the liveness probe (always 200 once the process serves).
	RouteHealth = "/healthz"
	// RouteReady is the readiness probe (200 only when dependencies are up).
	RouteReady = "/readyz"
	// RouteTLSAsk is queried by Caddy's on-demand TLS to authorize issuance.
	RouteTLSAsk = "/internal/tls-ask"
	// RouteInternalProxy receives peer-forwarded requests from another node.
	RouteInternalProxy = "/internal/proxy"

	// QueryParamDomain is the query key Caddy uses on RouteTLSAsk.
	QueryParamDomain = "domain"
)

// Admin API routes.
const (
	RouteAdminTokens       = "/v1/tokens"
	RouteAdminReservations = "/v1/reservations"
	RouteAdminTunnels      = "/v1/tunnels"
)

// HTTP header names used across ingress and peer forwarding.
const (
	HeaderForwardedFor    = "X-Forwarded-For"
	HeaderForwardedProto  = "X-Forwarded-Proto"
	HeaderForwardedHost   = "X-Forwarded-Host"
	HeaderRealIP          = "X-Real-IP"
	HeaderAuthorization   = "Authorization"
	HeaderTunlSubdomain   = "X-Tunl-Subdomain"
	HeaderTunlRequestID   = "X-Tunl-Request-Id"
	HeaderTunlPeerToken   = "X-Tunl-Peer-Token"
	BearerPrefix          = "Bearer "
)
