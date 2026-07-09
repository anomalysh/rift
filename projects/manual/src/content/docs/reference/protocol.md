---
title: Wire protocol
description: The rift v1 framing protocol — frame layout, frame types, control messages, request lifecycle, and failure semantics.
---

The agent (CLI) and the gateway (server) speak a binary framing protocol over a
single WebSocket connection. One WebSocket connection carries one tunnel and
multiplexes any number of concurrent proxied requests over it.

This is the contract between `server/internal/tunnelproto` (Go) and
`cli/src/protocol.ts` (TypeScript); both implementations must change together,
and `PROTOCOL_VERSION` must be bumped on any breaking change. A cross-language
conformance test asserts the two encoders byte-for-byte.

```text
PROTOCOL_VERSION = 1
```

## Transport

- WebSocket, **binary** messages only. A text message is a protocol violation.
- One WebSocket message is exactly one frame. No frame spans messages.
- The agent dials `GET {gateway_url}` with `Sec-WebSocket-Protocol: rift.v1`.

## Frame layout

Every frame is a fixed 13-byte header followed by a payload.

```text
 0        1                        9                        13
 +--------+------------------------+------------------------+----------------+
 | type   | stream_id (uint64 BE)  | length (uint32 BE)     | payload...     |
 +--------+------------------------+------------------------+----------------+
   1 byte          8 bytes                  4 bytes           `length` bytes
```

- `type` — one of the frame types below.
- `stream_id` — `0` for control frames. For data frames it identifies the
  proxied request. Stream IDs are allocated by the **gateway** (the side that
  originates requests) and increase monotonically from 1 per connection.
- `length` — payload byte length; must not exceed `MAX_PAYLOAD_BYTES`.

`stream_id` is a wire `uint64`; the TypeScript side handles it as a `bigint`
everywhere, since a JavaScript `number` cannot hold the full range without loss.

## Frame types

| Value  | Name       | Direction       | Payload                    |
| ------ | ---------- | --------------- | -------------------------- |
| `0x01` | `CONTROL`  | both            | JSON control envelope      |
| `0x10` | `REQ_HEAD` | gateway → agent | JSON `RequestHead`         |
| `0x11` | `REQ_BODY` | gateway → agent | raw request body chunk     |
| `0x12` | `REQ_END`  | gateway → agent | empty                      |
| `0x20` | `RES_HEAD` | agent → gateway | JSON `ResponseHead`        |
| `0x21` | `RES_BODY` | agent → gateway | raw response body chunk    |
| `0x22` | `RES_END`  | agent → gateway | empty                      |
| `0x30` | `RESET`    | both            | JSON `StreamReset`         |

A frame with an unknown type MUST be ignored (forward compatibility), except that
a peer MAY close the connection if unknown frames exceed a sane rate.

## Limits

| Constant            | Value       | Meaning                                     |
| ------------------- | ----------- | ------------------------------------------- |
| `MAX_PAYLOAD_BYTES` | `1048576`   | max bytes in a single frame payload (1 MiB) |
| `MAX_FRAME_BYTES`   | payload+13  | max whole-frame size                        |

Body chunking above `MAX_PAYLOAD_BYTES` is the sender's responsibility.

## Control envelope

Control frames (`type = 0x01`, `stream_id = 0`) carry JSON:

```json
{ "type": "hello", "payload": { } }
```

### `hello` — agent → gateway

Sent immediately after the WebSocket handshake. The gateway MUST NOT send any
data frame before replying to `hello`.

```json
{
  "protocol_version": 1,
  "token": "rift_...",
  "protocol": "http",
  "subdomain": "myapp",
  "local_port": 3000,
  "client_version": "0.1.0"
}
```

`subdomain` is optional — when empty the gateway allocates a random one.
`local_port` is informational and never used for routing.

### `hello_ok` — gateway → agent

```json
{
  "tunnel_id": "01J...",
  "subdomain": "myapp",
  "hostname": "myapp.example.com",
  "url": "https://myapp.example.com",
  "heartbeat_interval_ms": 15000
}
```

### `hello_error` — gateway → agent

Sent, then the connection is closed.

```json
{ "code": "subdomain_taken", "message": "subdomain \"myapp\" is in use" }
```

Codes: `unauthorized`, `subdomain_taken`, `subdomain_reserved`,
`subdomain_invalid`, `tunnel_limit`, `unsupported_protocol`,
`unsupported_version`, `internal`. See
[Troubleshooting](/operations/troubleshooting/#handshake-was-rejected) for what
each means and what to do about it.

### `ping` / `pong` — heartbeat

The agent sends `ping` every `heartbeat_interval_ms`; the gateway records it
(refreshing `last_seen_at`) and replies `pong`.

```json
{ "ts": 1736380800000 }
```

A gateway that receives no `ping` within its heartbeat timeout closes the
connection and reaps the tunnel. This is deliberately an **application**-level
heartbeat: WebSocket ping/pong frames are answered by intermediaries and prove
only that a proxy is alive, not that the agent's event loop is.

### `shutdown` — gateway → agent

```json
{ "reason": "server_shutdown" }
```

Reasons: `server_shutdown`, `token_revoked`, `heartbeat_timeout`, `replaced`
(the same subdomain was claimed by a newer connection). The gateway re-checks
each tunnel's token every `RIFT_TOKEN_REVALIDATE_INTERVAL`, so revoking or
expiring a token tears down the tunnels it opened within that interval. An agent
that receives `token_revoked` must not reconnect.

## Request / response lifecycle

For each inbound public request the gateway:

1. allocates `stream_id = n`
2. sends `REQ_HEAD` with the request head
3. streams `REQ_BODY` chunks (zero or more)
4. sends `REQ_END`

The agent replies on the same `stream_id`:

1. `RES_HEAD` with status and headers
2. zero or more `RES_BODY` chunks
3. `RES_END`

Either side may send `RESET` at any time to abort a stream. After `RESET` or
`RES_END`, the `stream_id` is retired and MUST NOT be reused.

### `RequestHead`

```json
{
  "method": "POST",
  "path": "/v1/items?page=2",
  "headers": { "content-type": ["application/json"] },
  "host": "myapp.example.com",
  "scheme": "https",
  "remote_addr": "203.0.113.9",
  "has_body": true
}
```

`path` includes the query string. Header names are lowercased; values are always
arrays, to preserve repeated headers. Hop-by-hop headers (`connection`,
`keep-alive`, `transfer-encoding`, `upgrade`, `proxy-*`, `te`, `trailer`) are
stripped by the gateway before forwarding, per RFC 7230 §6.1.

### `ResponseHead`

```json
{ "status": 200, "headers": { "content-type": ["application/json"] } }
```

### `StreamReset`

```json
{ "code": "upstream_error", "message": "ECONNREFUSED 127.0.0.1:3000" }
```

Codes: `upstream_error`, `upstream_timeout`, `client_disconnected`,
`payload_too_large`, `canceled`, `internal`.

## Failure semantics

- Local service unreachable: the agent sends `RESET` with `upstream_error`; the
  gateway answers the public client `502`.
- Stream exceeds the gateway's request timeout: the gateway sends `RESET` with
  `upstream_timeout` and answers `504`.
- Public client disconnects: the gateway sends `RESET` with
  `client_disconnected` so the agent can cancel the local request.
- WebSocket drops: all in-flight streams are reset; in-flight public requests
  receive `502`.

## Reconnection

The agent reconnects with exponential backoff and full jitter. On reconnect it
repeats the `hello` handshake with the same `subdomain`. Because the previous
tunnel row may not have been reaped yet, the gateway treats a `hello` for a
subdomain owned by the **same token** as a takeover: the older connection is sent
`shutdown{reason:"replaced"}` and closed. This is why a reconnect reclaims its
own row before it counts against the token's tunnel limit.
