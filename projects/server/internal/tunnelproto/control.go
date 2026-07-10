package tunnelproto

import (
	"encoding/json"
	"fmt"

	"github.com/anomalysh/rift/projects/server/internal/core"
)

// ControlType names a control message. Values are wire-visible.
type ControlType string

const (
	ControlHello      ControlType = "hello"
	ControlHelloOK    ControlType = "hello_ok"
	ControlHelloError ControlType = "hello_error"
	ControlPing       ControlType = "ping"
	ControlPong       ControlType = "pong"
	ControlShutdown   ControlType = "shutdown"
)

// ErrorCode is the machine-readable reason a handshake was rejected.
type ErrorCode string

const (
	ErrCodeUnauthorized        ErrorCode = "unauthorized"
	ErrCodeSubdomainTaken      ErrorCode = "subdomain_taken"
	ErrCodeSubdomainReserved   ErrorCode = "subdomain_reserved"
	ErrCodeSubdomainInvalid    ErrorCode = "subdomain_invalid"
	ErrCodeTunnelLimit         ErrorCode = "tunnel_limit"
	ErrCodeUnsupportedProtocol ErrorCode = "unsupported_protocol"
	ErrCodeInvalidPolicy       ErrorCode = "invalid_policy"
	ErrCodeInvalidDomain       ErrorCode = "invalid_domain"
	ErrCodeDomainOwned         ErrorCode = "domain_owned"
	ErrCodeUnsupportedVersion  ErrorCode = "unsupported_version"
	ErrCodeInternal            ErrorCode = "internal"
)

// ShutdownReason explains why the gateway is closing a tunnel.
type ShutdownReason string

const (
	ShutdownServerShutdown   ShutdownReason = "server_shutdown"
	ShutdownTokenRevoked     ShutdownReason = "token_revoked"
	ShutdownHeartbeatTimeout ShutdownReason = "heartbeat_timeout"
	ShutdownReplaced         ShutdownReason = "replaced"
	ShutdownPolicyExpired    ShutdownReason = "policy_expired"
)

// ResetCode explains why a stream was aborted.
type ResetCode string

const (
	ResetUpstreamError      ResetCode = "upstream_error"
	ResetUpstreamTimeout    ResetCode = "upstream_timeout"
	ResetClientDisconnected ResetCode = "client_disconnected"
	ResetPayloadTooLarge    ResetCode = "payload_too_large"
	ResetCanceled           ResetCode = "canceled"
	ResetInternal           ResetCode = "internal"
)

// ControlEnvelope wraps every control message so the type can be read before
// the payload is understood.
type ControlEnvelope struct {
	Type    ControlType     `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Hello is the agent's opening message.
type Hello struct {
	ProtocolVersion int    `json:"protocol_version"`
	Token           string `json:"token"`
	Protocol        string `json:"protocol"`
	Subdomain       string `json:"subdomain,omitempty"`
	LocalPort       int    `json:"local_port,omitempty"`
	ClientVersion   string `json:"client_version,omitempty"`
	// Policy is the optional per-tunnel visitor-access policy the agent
	// attaches (basic-auth, IP rules, rate limit, lifetime bounds). Additive
	// and omitempty, so an older agent that never sends it is unaffected and no
	// protocol-version bump is needed.
	Policy *core.Policy `json:"policy,omitempty"`
	// Domains are BYO custom hostnames the agent wants routed to this tunnel
	// (E1). Each is registered against the claimed subdomain at connect time.
	// Additive and omitempty, so an older agent is unaffected.
	Domains []string `json:"domains,omitempty"`
}

// HelloOK confirms the tunnel is live and routable.
type HelloOK struct {
	TunnelID            string `json:"tunnel_id"`
	Subdomain           string `json:"subdomain"`
	Hostname            string `json:"hostname"`
	URL                 string `json:"url"`
	HeartbeatIntervalMS int64  `json:"heartbeat_interval_ms"`
	// BindAddr is the public host:port a raw tunnel (tcp/tls) is reached on.
	// Empty for http tunnels, which are reached by URL.
	BindAddr string `json:"bind_addr,omitempty"`
	// ProtocolVersion is the current protocol version the gateway speaks (its
	// maximum). An agent behind this can warn that a newer rift is available
	// without the handshake failing.
	ProtocolVersion int `json:"protocol_version,omitempty"`
}

// HelloError rejects the handshake; the connection closes after it is sent.
type HelloError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

// Heartbeat is the payload of both ping and pong.
type Heartbeat struct {
	TS int64 `json:"ts"`
}

// Shutdown tells the agent the gateway is terminating the tunnel.
type Shutdown struct {
	Reason ShutdownReason `json:"reason"`
}

// RequestHead is the head of a proxied public request (gateway -> agent).
type RequestHead struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Headers    map[string][]string `json:"headers"`
	Host       string              `json:"host"`
	Scheme     string              `json:"scheme"`
	RemoteAddr string              `json:"remote_addr"`
	HasBody    bool                `json:"has_body"`
	// Raw marks the stream as a raw byte pipe with no HTTP semantics, used by
	// tcp/tls tunnels: the agent dials its local port and pipes bytes both ways
	// (REQ_BODY in, RES_BODY out) without a RequestHead/ResponseHead exchange.
	Raw bool `json:"raw,omitempty"`
	// Upgrade marks a connection-upgrade request (WebSocket and other
	// Upgrade-based protocols). The agent then dials the local service over a
	// raw socket and, once it switches protocols, the stream becomes a
	// full-duplex byte pipe: REQ_BODY carries client->service bytes, RES_BODY
	// service->client, and REQ_END/RES_END a half-close. Omitted (false) for an
	// ordinary HTTP request so existing frames are byte-identical.
	Upgrade bool `json:"upgrade,omitempty"`
}

// ResponseHead is the head of the local service's response (agent -> gateway).
type ResponseHead struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
}

// StreamReset aborts a single stream.
type StreamReset struct {
	Code    ResetCode `json:"code"`
	Message string    `json:"message,omitempty"`
}

// EncodeControl builds a CONTROL frame carrying the given message.
func EncodeControl(t ControlType, payload any) ([]byte, error) {
	env := ControlEnvelope{Type: t}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("tunnelproto: marshal %s payload: %w", t, err)
		}
		env.Payload = raw
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("tunnelproto: marshal %s envelope: %w", t, err)
	}
	return Encode(FrameControl, ControlStreamID, body)
}

// DecodeControl parses a CONTROL frame payload into its envelope.
func DecodeControl(payload []byte) (ControlEnvelope, error) {
	var env ControlEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return ControlEnvelope{}, fmt.Errorf("tunnelproto: decode control envelope: %w", err)
	}
	if env.Type == "" {
		return ControlEnvelope{}, fmt.Errorf("tunnelproto: control envelope missing type")
	}
	return env, nil
}

// UnmarshalPayload decodes an envelope payload into v.
func UnmarshalPayload(env ControlEnvelope, v any) error {
	if len(env.Payload) == 0 {
		return fmt.Errorf("tunnelproto: control %s has empty payload", env.Type)
	}
	if err := json.Unmarshal(env.Payload, v); err != nil {
		return fmt.Errorf("tunnelproto: decode %s payload: %w", env.Type, err)
	}
	return nil
}

// EncodeJSONFrame builds a non-control frame whose payload is JSON.
func EncodeJSONFrame(t FrameType, streamID uint64, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tunnelproto: marshal %s: %w", t, err)
	}
	return Encode(t, streamID, raw)
}
