# rift wire protocol v1

The tunnel agent (CLI) and the tunnel gateway (server) speak a binary framing
protocol over a single WebSocket connection. One WebSocket connection carries
one tunnel, and multiplexes any number of concurrent proxied requests over it.

This document is the **contract** between `projects/server/internal/tunnelproto` (Go) and
`projects/cli/src/protocol.ts` (TypeScript). Both implementations must be changed
together, and `PROTOCOL_VERSION` must be bumped on any breaking change.

    PROTOCOL_VERSION = 1

## Transport

* WebSocket, **binary** messages only. Text messages are a protocol violation.
* One WebSocket message == exactly one frame. No frame spans messages.
* The agent dials the gateway at `GET {gateway_url}` with
  `Sec-WebSocket-Protocol: rift.v1`.

## Frame layout

Every frame is a fixed 13-byte header followed by a payload.

```
 0        1                        9                        13
 +--------+------------------------+------------------------+----------------+
 | type   | stream_id (uint64 BE)  | length (uint32 BE)     | payload...     |
 +--------+------------------------+------------------------+----------------+
   1 byte          8 bytes                  4 bytes           `length` bytes
```

* `type` — one of the frame types below.
* `stream_id` — `0` for control frames. For data frames, identifies the
  proxied request. Stream IDs are allocated by the **gateway** (the side that
  originates requests) and increase monotonically from 1 per connection.
* `length` — payload byte length. Must not exceed `MAX_PAYLOAD_BYTES`.

## Frame types

| Value  | Name       | Direction       | Payload                         |
| ------ | ---------- | --------------- | ------------------------------- |
| `0x01` | `CONTROL`  | both            | JSON control envelope           |
| `0x10` | `REQ_HEAD` | gateway → agent | JSON `RequestHead`              |
| `0x11` | `REQ_BODY` | gateway → agent | raw request body chunk          |
| `0x12` | `REQ_END`  | gateway → agent | empty                           |
| `0x20` | `RES_HEAD` | agent → gateway | JSON `ResponseHead`             |
| `0x21` | `RES_BODY` | agent → gateway | raw response body chunk         |
| `0x22` | `RES_END`  | agent → gateway | empty                           |
| `0x30` | `RESET`    | both            | JSON `StreamReset`              |

Any frame with an unknown type MUST be ignored (forward compatibility), except
that a peer MAY close the connection if unknown frames exceed a sane rate.

## Limits

| Constant             | Value       | Meaning                                    |
| -------------------- | ----------- | ------------------------------------------ |
| `MAX_PAYLOAD_BYTES`  | `1_048_576` | max bytes in a single frame payload (1 MiB)|
| `MAX_FRAME_BYTES`    | payload+13  | max whole-frame size                       |

Body chunking above `MAX_PAYLOAD_BYTES` is the sender's responsibility.

## Control envelope

Control frames (`type = 0x01`, `stream_id = 0`) carry JSON:

```json
{ "type": "hello", "payload": { } }
```

### `hello` — agent → gateway

Sent immediately after the WebSocket handshake. The gateway MUST NOT send any
data frame before it has replied to `hello`.

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

* `subdomain` is optional. When empty the gateway allocates a random one.
* `local_port` is informational; it is never used for routing.
* `protocol` is the wire protocol (`http`, `tcp`, or `tls`). Whether the agent
  dials its local upstream over plain HTTP or HTTPS is an **agent-local** choice
  (the CLI's `http` vs `https` keyword) that never appears on the wire:
  `hello.protocol` is `"http"` for both and `RequestHead.scheme` stays the public
  scheme, so an `https` tunnel is byte-identical to a plain `http` one and needs
  no `PROTOCOL_VERSION` bump.

### `hello_ok` — gateway → agent

```json
{
  "tunnel_id": "01J...",
  "subdomain": "myapp",
  "hostname": "myapp.rift.anomaly.sh",
  "url": "https://myapp.rift.anomaly.sh",
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
`unsupported_version`, `internal`.

### `ping` / `pong` — heartbeat

The agent sends `ping` every `heartbeat_interval_ms`. The gateway records the
heartbeat (refreshing `tunnels.last_seen_at`) and replies `pong`.

```json
{ "ts": 1736380800000 }
```

A gateway that receives no `ping` within its heartbeat timeout closes the
connection and reaps the tunnel. This is deliberately an *application*-level
heartbeat: WebSocket ping/pong frames are handled by intermediaries and do not
prove the agent's event loop is alive.

### `shutdown` — gateway → agent

```json
{ "reason": "server_shutdown" }
```

Reasons: `server_shutdown`, `token_revoked`, `heartbeat_timeout`,
`replaced` (same subdomain claimed by a newer connection).

The gateway re-checks each tunnel's token every
`RIFT_TOKEN_REVALIDATE_INTERVAL`. Revoking or expiring a token therefore
tears down the tunnels it already opened, within that interval, rather than
only preventing new ones. An agent that receives `token_revoked` must not
reconnect.

## Request / response lifecycle

For each inbound public HTTP request the gateway:

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
  "host": "myapp.rift.anomaly.sh",
  "scheme": "https",
  "remote_addr": "203.0.113.9",
  "has_body": true
}
```

`path` includes the query string. Header names are lowercased; values are
always arrays to preserve repeated headers.

Hop-by-hop headers (`connection`, `keep-alive`, `transfer-encoding`,
`upgrade`, `proxy-*`, `te`, `trailer`) are stripped by the gateway before
forwarding, per RFC 7230 §6.1 — **except on an upgrade request** (see below),
where `connection` and `upgrade` are the point of the request and are kept.

An optional `"upgrade": true` field marks a connection-upgrade request
(WebSocket and other `Upgrade`-based protocols). It is omitted (not `false`) on
an ordinary request, so normal frames are byte-identical to earlier versions.

## Connection upgrades (WebSocket)

An upgrade reuses the ordinary frames rather than adding new types. When the
gateway sees a public request carrying `Connection: upgrade` it:

1. allocates `stream_id = n`
2. sends `REQ_HEAD` with `"upgrade": true`, preserving the `connection` and
   `upgrade` headers (and `sec-websocket-*`). It does **not** send `REQ_END`.

The agent dials the local service over a raw socket, replays the request
verbatim (rewriting `Host` to the local target), and reads the response head:

* If the service returns **`101 Switching Protocols`**, the agent sends
  `RES_HEAD` with status `101` and the handshake headers (again not stripped),
  and the stream becomes a full-duplex byte pipe: `REQ_BODY` carries
  client→service bytes, `RES_BODY` carries service→client bytes, `REQ_END` /
  `RES_END` are half-closes, and `RESET` aborts. The gateway hijacks the public
  connection, writes the `101`, and copies bytes both ways.
* If the service returns **any other status**, the agent relays it as an
  ordinary response (`RES_HEAD` / `RES_BODY*` / `RES_END`) and no pipe is
  established; the gateway sends it back as a normal HTTP response.

Upgrades are only carried for a tunnel attached to the node the client reached;
a tunnel served by a peer node returns `502` for an upgrade request, because the
node-to-node HTTP hop cannot carry a hijacked socket.

## Raw tunnels (tcp / tls)

A `tcp` or `tls` tunnel carries a raw byte stream with no HTTP semantics. The
agent advertises the protocol in its `hello` and the gateway replies with
`hello_ok.bind_addr`, the public `host:port` the tunnel is reached on:

* **tcp** — the gateway allocates a public port from its configured range for
  the tunnel (`rift.example.com:20034`). Each inbound TCP connection opens a new
  stream to the agent.
* **tls** — the gateway peeks the ClientHello **SNI** on a shared listener,
  routes to the tls tunnel whose subdomain matches (`myapp.rift.example.com:8443`),
  and passes the still-encrypted bytes through; the agent's local service
  terminates TLS. The gateway never decrypts.

For each inbound connection the gateway allocates `stream_id = n` and sends a
`REQ_HEAD` with `"raw": true` and no other meaningful fields. There is no
`RES_HEAD`: `REQ_BODY` carries client→service bytes, `RES_BODY` service→client,
`REQ_END`/`RES_END` are half-closes, and `RESET` aborts. The agent dials its
local port and pipes. A failed local dial arrives as `RESET`.

Like upgrades, raw tunnels are served only by the node the connection reached.

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

* If the agent's local service is unreachable, the agent sends `RESET` with
  `upstream_error`; the gateway responds to the public client with `502`.
* If a stream exceeds the gateway's request timeout, the gateway sends `RESET`
  with `upstream_timeout` and responds `504`.
* If the public client disconnects, the gateway sends `RESET` with
  `client_disconnected` so the agent can cancel the local request.
* If the WebSocket drops, all in-flight streams are reset; in-flight public
  requests receive `502`.

## Reconnection

The agent reconnects with exponential backoff and full jitter. On reconnect it
repeats the `hello` handshake with the same `subdomain`. Because the previous
tunnel row may not have been reaped yet, the gateway treats a `hello` for a
subdomain owned by the **same token** as a takeover: the older connection is
sent `shutdown{reason:"replaced"}` and closed.
