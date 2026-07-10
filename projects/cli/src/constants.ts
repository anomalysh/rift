// Single source of truth for wire-protocol constants, environment variable
// names, configuration defaults, and process exit codes.
//
// Nothing outside this file may spell an env-var name, a default value, or a
// protocol constant inline. The wire-protocol values here MUST match
// server/internal/tunnelproto (frame.go, control.go) byte-for-byte; see
// docs/PROTOCOL.md.

/** Agent version, advertised as `client_version` in the hello handshake. */
export const VERSION = "0.1.0";

/** Protocol version advertised in the hello handshake (tunnelproto.Version). */
export const PROTOCOL_VERSION = 1;

/** WebSocket subprotocol both peers negotiate (tunnelproto.Subprotocol). */
export const SUBPROTOCOL = "rift.v1";

/** Fixed frame header length: type(1) + stream_id(8) + length(4). */
export const HEADER_SIZE = 13;

/** Max bytes in a single frame payload (1 MiB). Senders chunk above this. */
export const MAX_PAYLOAD_BYTES = 1 << 20;

/** Largest legal whole frame on the wire. */
export const MAX_FRAME_BYTES = HEADER_SIZE + MAX_PAYLOAD_BYTES;

// stream_id is a wire uint64. A JS `number` only holds integers up to 2^53-1,
// so a large stream_id (the gateway allocates monotonically and the field is
// 64-bit) cannot round-trip through `number` without silent loss. It is a
// `bigint` everywhere it is decoded, compared, or used as a map key.
export const CONTROL_STREAM_ID = 0n;

/** Frame type discriminants (tunnelproto FrameType). Values are stable. */
export const FrameType = {
  CONTROL: 0x01,
  REQ_HEAD: 0x10,
  REQ_BODY: 0x11,
  REQ_END: 0x12,
  RES_HEAD: 0x20,
  RES_BODY: 0x21,
  RES_END: 0x22,
  RESET: 0x30,
} as const;

export type FrameTypeName = keyof typeof FrameType;
export type FrameTypeValue = (typeof FrameType)[FrameTypeName];

/** Every frame type this protocol version understands. */
export const KNOWN_FRAME_TYPES: ReadonlySet<number> = new Set<number>(
  Object.values(FrameType),
);

/** Control message discriminators (tunnelproto ControlType). */
export const ControlType = {
  HELLO: "hello",
  HELLO_OK: "hello_ok",
  HELLO_ERROR: "hello_error",
  PING: "ping",
  PONG: "pong",
  SHUTDOWN: "shutdown",
} as const;

export type ControlTypeValue = (typeof ControlType)[keyof typeof ControlType];

/** Handshake rejection codes (tunnelproto ErrorCode). */
export const HelloErrorCode = {
  UNAUTHORIZED: "unauthorized",
  SUBDOMAIN_TAKEN: "subdomain_taken",
  SUBDOMAIN_RESERVED: "subdomain_reserved",
  SUBDOMAIN_INVALID: "subdomain_invalid",
  TUNNEL_LIMIT: "tunnel_limit",
  UNSUPPORTED_PROTOCOL: "unsupported_protocol",
  UNSUPPORTED_VERSION: "unsupported_version",
  INTERNAL: "internal",
} as const;

/** Tunnel shutdown reasons (tunnelproto ShutdownReason). */
export const ShutdownReason = {
  SERVER_SHUTDOWN: "server_shutdown",
  TOKEN_REVOKED: "token_revoked",
  HEARTBEAT_TIMEOUT: "heartbeat_timeout",
  REPLACED: "replaced",
} as const;

export type ShutdownReasonValue =
  (typeof ShutdownReason)[keyof typeof ShutdownReason];

/** Stream abort reasons (tunnelproto ResetCode). */
export const ResetCode = {
  UPSTREAM_ERROR: "upstream_error",
  UPSTREAM_TIMEOUT: "upstream_timeout",
  CLIENT_DISCONNECTED: "client_disconnected",
  PAYLOAD_TOO_LARGE: "payload_too_large",
  CANCELED: "canceled",
  INTERNAL: "internal",
} as const;

export type ResetCodeValue = (typeof ResetCode)[keyof typeof ResetCode];

/** Application protocols this build tunnels. `http` is routed by subdomain;
 *  `https` forwards to a local HTTPS upstream (the edge still terminates TLS,
 *  so it is `http` on the wire); `tcp` is reached on a gateway-allocated port;
 *  `tls` is SNI-routed and passed through to a local service that terminates
 *  TLS. */
export const SUPPORTED_PROTOCOLS = [
  "http",
  "https",
  "tcp",
  "tls",
  "udp",
] as const;
export type SupportedProtocol = (typeof SUPPORTED_PROTOCOLS)[number];

/** How each CLI protocol keyword is dialed: the value it carries on the wire and
 *  whether the agent dials its local upstream over TLS. The keyword is a local
 *  convenience — `https` and `http` are the same `http` tunnel to the gateway
 *  (see docs/PROTOCOL.md), differing only in the upstream scheme — so deriving
 *  both facts here keeps the wire value and the TLS intent from ever being
 *  spelled inline. */
export const PROTOCOL_DIALER = {
  http: { wire: "http", upstreamTls: false },
  https: { wire: "http", upstreamTls: true },
  tcp: { wire: "tcp", upstreamTls: false },
  tls: { wire: "tls", upstreamTls: false },
  udp: { wire: "udp", upstreamTls: false },
} as const satisfies Record<
  SupportedProtocol,
  { wire: string; upstreamTls: boolean }
>;

/** Shells for which `rift completions <shell>` can emit a completion script. */
export const COMPLETION_SHELLS = ["bash", "zsh", "fish"] as const;
export type Shell = (typeof COMPLETION_SHELLS)[number];

/** Log levels, ordered from most to least verbose. */
export const LOG_LEVELS = ["debug", "info", "warn", "error", "silent"] as const;
export type LogLevel = (typeof LOG_LEVELS)[number];

/** Environment variable names read during configuration resolution. */
export const ENV = {
  TOKEN: "RIFT_TOKEN",
  SERVER: "RIFT_SERVER",
  HOST: "RIFT_HOST",
  LOG_LEVEL: "RIFT_LOG_LEVEL",
  XDG_CONFIG_HOME: "XDG_CONFIG_HOME",
  HOME: "HOME",
  // Colour opt-out for the interactive TUI. Either the cross-tool NO_COLOR
  // convention (https://no-color.org) or the rift-specific override disables
  // colour and cursor control, falling back to plain text.
  NO_COLOR: "NO_COLOR",
  RIFT_NO_COLOR: "RIFT_NO_COLOR",
} as const;

/**
 * Built-in defaults. Only values with a sane default appear here: `token` and
 * `server` deliberately have none, so a missing one is an actionable error.
 */
export const DEFAULTS = {
  HOST: "127.0.0.1",
  LOG_LEVEL: "info",
} as const satisfies { HOST: string; LOG_LEVEL: LogLevel };

/** Config file location relative to the XDG config home. */
export const CONFIG_DIR_NAME = "rift";
export const CONFIG_FILE_NAME = "config.json";
/** Fallback config home under $HOME when XDG_CONFIG_HOME is unset. */
export const XDG_CONFIG_FALLBACK = ".config";

/** Process exit codes. */
export const EXIT = {
  OK: 0,
  ERROR: 1,
  USAGE: 2,
  /** Conventional 128 + SIGINT(2). */
  SIGINT: 130,
  /** Conventional 128 + SIGTERM(15). */
  SIGTERM: 143,
} as const;

// Hop-by-hop headers are meaningful only for a single transport hop and must
// not be forwarded end to end (RFC 7230 §6.1). The agent strips these from the
// local service's response before handing it back to the gateway. `proxy-*` is
// matched by prefix, not present in this set.
export const HOP_BY_HOP_HEADERS: ReadonlySet<string> = new Set([
  "connection",
  "keep-alive",
  "transfer-encoding",
  "upgrade",
  "proxy-connection",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
]);

/** Reconnection backoff bounds. The policy is decorrelated jitter; see
 *  backoff.ts. BASE_MS is the floor for every retry, CAP_MS the ceiling. */
export const RECONNECT = {
  BASE_MS: 500,
  CAP_MS: 30_000,
} as const;

/**
 * When the socket's bufferedAmount exceeds this, the forwarder pauses reading
 * the local response until the buffer drains, bounding memory under a slow
 * gateway link.
 */
export const BACKPRESSURE_THRESHOLD_BYTES = 4 * MAX_PAYLOAD_BYTES;

/** How often to poll bufferedAmount while waiting for the socket to drain. */
export const DRAIN_POLL_INTERVAL_MS = 5;

/**
 * Upper bound on the response header block the agent buffers while proxying a
 * connection upgrade (WebSocket etc.) over a raw socket. A local service that
 * never terminates its headers must not grow this without limit.
 */
export const MAX_UPGRADE_HEAD_BYTES = 64 * 1024;
