---
title: Admin API
description: Every admin route — method, request body, response shape, and status codes — plus the internal ingress endpoints.
---

The admin API manages tokens, reservations, and the view of live tunnels. It is
authenticated by a single shared bearer token (`RIFT_ADMIN_TOKEN`) and is
**never exposed publicly** — no site block proxies it and its port is never
published. Reach it from the host itself or over an SSH tunnel.

```text
Authorization: Bearer <RIFT_ADMIN_TOKEN>
```

The token is compared in constant time. A `401` carries a
`WWW-Authenticate: Bearer realm="rift admin"` challenge and never echoes the
expected token. The one exception to authentication is `GET /healthz`, so a
liveness probe needs no credentials.

Request bodies are JSON, capped at 64 KiB, and decoded with **unknown fields
rejected**; a body must be exactly one JSON object. All responses are JSON.

## Error envelope

Every error returns the same envelope. Clients should branch on `code`, not on
the human-readable `message`:

```json
{ "error": { "code": "not_found", "message": "resource not found" } }
```

| Code                  | HTTP status | When                                             |
| --------------------- | ----------- | ------------------------------------------------ |
| `bad_request`         | 400         | Malformed body, missing required field, or a negative `max_tunnels`. |
| `invalid_subdomain`   | 400         | A subdomain that fails the length/pattern rules. |
| `unauthorized`        | 401         | Missing or wrong bearer token.                   |
| `not_found`           | 404         | Unknown token id, reservation, or route.         |
| `method_not_allowed`  | 405         | Wrong method (response carries an `Allow` header).|
| `conflict`            | 409         | Blocklisted or already-taken/already-reserved subdomain. |
| `payload_too_large`   | 413         | Request body over 64 KiB.                        |
| `internal_error`      | 500         | Server-side fault (detail is logged, never returned). |

## Tokens

### `POST /v1/tokens` — mint a token

Request:

```json
{ "name": "my-laptop", "max_tunnels": 5, "expires_at": "2027-01-01T00:00:00Z" }
```

- `name` (string, **required**).
- `max_tunnels` (int, optional): defaults to `RIFT_MAX_TUNNELS_PER_TOKEN`; must
  not be negative.
- `expires_at` (RFC 3339 timestamp, optional): after this the token is inactive.

Response `201 Created` — this is the **only** place the plaintext token is ever
returned:

```json
{
  "id": "01J...",
  "name": "my-laptop",
  "token": "rift_...",
  "max_tunnels": 5,
  "created_at": "2026-07-09T12:00:00Z",
  "expires_at": null
}
```

The plaintext is never persisted, never logged, and never appears in any other
response. Only its SHA-256 hash is stored.

### `GET /v1/tokens` — list tokens

Response `200 OK`. The projection never includes the hash or the plaintext:

```json
{
  "tokens": [
    {
      "id": "01J...",
      "name": "my-laptop",
      "max_tunnels": 5,
      "created_at": "2026-07-09T12:00:00Z",
      "last_used_at": null,
      "revoked_at": null,
      "expires_at": null
    }
  ]
}
```

### `DELETE /v1/tokens/{id}` — revoke a token

Response `204 No Content`. Revoking tears down the token's **live** tunnels
within `RIFT_TOKEN_REVALIDATE_INTERVAL`, not merely blocking new ones. An unknown
id returns `404`.

## Reservations

### `POST /v1/reservations` — pin a subdomain to a token

Request:

```json
{ "subdomain": "myapp", "token_id": "01J...", "note": "prod webhook" }
```

- `subdomain` (string, **required**): normalized, then validated against the
  subdomain rules. An invalid shape is `400 invalid_subdomain`; a blocklisted
  label is `409 conflict`.
- `token_id` (string, **required**): must reference an existing token, else
  `404`.
- `note` (string, optional).

Response `201 Created`:

```json
{ "subdomain": "myapp", "token_id": "01J...", "note": "prod webhook", "created_at": "2026-07-09T12:00:00Z" }
```

An already-reserved subdomain returns `409 conflict`.

### `GET /v1/reservations` — list reservations

Response `200 OK`:

```json
{ "reservations": [ { "subdomain": "myapp", "token_id": "01J...", "note": "", "created_at": "2026-07-09T12:00:00Z" } ] }
```

### `DELETE /v1/reservations/{subdomain}` — release a reservation

Response `204 No Content`. The subdomain in the path is normalized first.

## Tunnels

### `GET /v1/tunnels` — list live tunnels

Response `200 OK`:

```json
{
  "tunnels": [
    {
      "id": "01J...",
      "subdomain": "myapp",
      "token_id": "01J...",
      "protocol": "http",
      "local_port": 3000,
      "node_id": "01J...",
      "client_addr": "203.0.113.9",
      "connected_at": "2026-07-09T12:00:00Z",
      "last_seen_at": "2026-07-09T12:00:30Z"
    }
  ]
}
```

## Health

### `GET /healthz`

Unauthenticated liveness probe. Returns `200 OK` with `{"status":"ok"}`. It is
served on all three listeners (ingress, gateway, admin).

## Internal ingress endpoints

These live on the **ingress** listener (`RIFT_INGRESS_ADDR`, default `:8080`),
not the admin listener. They are called by Caddy and by peer nodes, never by an
operator directly, but understanding them helps when debugging.

### TLS-ask authorization

```text
GET /internal/tls-ask?domain=<sni>
```

Caddy's on-demand TLS (the `http01` mode) queries this before issuing a
certificate for an SNI. riftd replies:

| Status | Meaning                                                                 |
| ------ | ----------------------------------------------------------------------- |
| `200`  | Authorized — the name is the gateway hostname, the base domain, a subdomain with a live tunnel or existing tunnel row, or a reserved subdomain. |
| `400`  | No `domain` query parameter.                                            |
| `403`  | The name is not served by this host (not under the base domain).        |
| `404`  | A subdomain under the base domain with no live tunnel, tunnel row, or reservation. |
| `500`  | A store lookup failed.                                                  |

Approving broadly would turn the server into an open certificate-issuance relay,
so only names riftd can vouch for are authorized. See
[TLS modes](/guides/tls-modes/#why-the-ask-endpoint-is-mandatory-under-http01).

### Peer proxy

```text
POST /internal/proxy    (header X-Rift-Subdomain, authenticated by the peer secret)
```

Serves a request another node forwarded when Redis routing is enabled. It is
`404` when Redis is disabled, `403` without a valid peer token, and `503` when
the lease was stale and no local session holds the subdomain. It never forwards
onward, so a stale lease cannot create a routing loop. See
[Multi-node with Redis](/guides/multi-node/).
