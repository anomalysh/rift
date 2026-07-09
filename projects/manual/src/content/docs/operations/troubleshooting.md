---
title: Troubleshooting
description: Real symptoms and their real causes — TLS handshake errors, empty POST bodies, 502 vs 504, rejected handshakes, and the Caddy-reload gotcha.
---

Concrete symptoms an operator actually sees, and what each one means.

## `ERR_SSL_PROTOCOL_ERROR` on a never-used subdomain

**Symptom.** A browser opens `https://neverused.example.com` and shows
`ERR_SSL_PROTOCOL_ERROR` — not a 404 page.

**Cause.** This is **expected under the `http01` TLS mode**. `http01` mints a
certificate per hostname on first contact, so a name that has never served a
tunnel has no certificate. The TLS handshake happens before any HTTP request, so
with no certificate the handshake cannot complete, and there is no way to return
a 404 (which is an HTTP response). It is not a misconfiguration.

**Fix.** If users open tunnel URLs directly — including for subdomains that are
not currently connected — switch to `dns01`, which obtains a wildcard certificate
that covers every subdomain, used or not. `self` and `internal` (with a wildcard)
cover unknown subdomains too. See
[TLS modes](/guides/tls-modes/#why-an-unused-subdomain-fails-under-http01) and
the [migration steps](/guides/tls-modes/#migrating-from-http01-to-dns01).

## A POST arrives with an empty body

**Symptom.** A `POST` (or `PUT`) reaches the local service with the correct
headers but an **empty body**, typically against a simple development server such
as Python's `http.server`.

**Cause and fix.** This was a bug, now fixed. The agent re-frames the request
body as a stream; a stream with no declared length makes `fetch` fall back to
`Transfer-Encoding: chunked`. Many simple local servers never implement chunked
**request** decoding and silently hand the application an empty body. The agent
now preserves the incoming `content-length`, keeping identity framing — the same
framing the public client sent in the first place — so the body arrives intact.
If you still see this, make sure the agent is built from a version that includes
the fix (`rift --version`), and check that the public client actually sent a
`Content-Length`.

## `502` vs `504`

Both are gateway errors, but they mean different things:

| Status | Reset code                         | Meaning                                                        |
| ------ | ---------------------------------- | -------------------------------------------------------------- |
| `502`  | `upstream_error`                   | The local service behind the tunnel refused the connection (e.g. nothing is listening on the port). |
| `502`  | `tunnel_unavailable` / dropped WS  | The tunnel disconnected while the request was in flight.       |
| `504`  | `upstream_timeout`                 | The local service did not respond within `RIFT_REQUEST_TIMEOUT` (default 60s). |
| `413`  | `payload_too_large`                | The request body exceeded `RIFT_MAX_REQUEST_BODY_BYTES` (default 32 MiB; `0` disables the limit). |
| `404`  | —                                  | No tunnel is currently serving that hostname, or the host is not under the base domain. |

A `502` points at your local app or the tunnel connection; a `504` points at your
local app being **slow**. If a legitimately long request is being cut off at 60
seconds, raise `RIFT_REQUEST_TIMEOUT`.

## Handshake was rejected

When the gateway rejects a tunnel, the agent receives a `hello_error` with one of
these codes:

| Code                   | What it means                                              | What to do                                                                 |
| ---------------------- | --------------------------------------------------------- | -------------------------------------------------------------------------- |
| `unauthorized`         | The token is missing the `rift_` prefix, unknown, revoked, or expired. | Mint a fresh token; check it is passed correctly. All auth failures collapse to this one code by design. |
| `subdomain_taken`      | Another live tunnel (a different token) already holds the subdomain. | Pick another subdomain, or reserve it to the token that should own it.     |
| `subdomain_reserved`   | The subdomain is reserved for a different token, or is on the blocklist. | Use the token it is reserved for, choose another name, or adjust the reservation/blocklist. |
| `subdomain_invalid`    | The subdomain fails the length or pattern rules.          | Use 3–63 lowercase alphanumerics with internal hyphens only.               |
| `tunnel_limit`         | The token already holds its `max_tunnels` tunnels.        | Close an existing tunnel, or raise the token's limit / `RIFT_MAX_TUNNELS_PER_TOKEN`. |
| `unsupported_protocol` | A protocol other than `http` was requested.               | Only `http` is implemented; use it.                                        |
| `unsupported_version`  | The agent and gateway protocol versions differ.           | Update the agent (or gateway) so both speak the same `PROTOCOL_VERSION`.    |
| `internal`             | A server-side fault during the handshake.                 | Check the gateway logs.                                                     |

Note that a reconnect by the **same** token to the **same** subdomain is not a
`subdomain_taken` error — it is treated as a takeover, and the older connection
is closed with `shutdown{reason:"replaced"}`.

## A Caddyfile edit did not take effect

**Symptom.** You changed Caddy configuration and redeployed, but the old
behaviour persists.

**Cause.** The Caddyfile is a **bind mount**, so `docker compose up` sees an
unchanged service spec and leaves Caddy running with its old configuration in
memory. The edited file is on disk but never loaded.

**Fix.** Reload Caddy explicitly (this keeps the certificate cache and drops no
connections):

```sh
docker exec rift-caddy-1 caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

`tools/remote-deploy.sh` does this automatically after every deploy, falling back
to a container restart if the admin API is unreachable. If you edit config by
hand, run the reload yourself.

## Running the e2e harness

The black-box suite brings up Postgres, riftd, and a real Caddy using the
production Caddyfile, then drives the whole stack with the compiled CLI over
HTTPS — validating the certificate chain with no `-k` anywhere. It is hermetic:
nothing reaches the internet.

```sh
mise run e2e                   # default: the internal and self TLS modes
mise run e2e -- --mode internal
mise run e2e -- --keep         # leave the stack up afterwards to poke at it
```

It uses `curl --resolve`, so nothing is written to `/etc/hosts` and it needs no
root. `http01` is deliberately not covered, because on-demand issuance needs a
real ACME server; its authorization logic (the ask endpoint) is asserted
directly in every mode and again in the Go suite. Both TLS bugs this project has
shipped would have failed this harness.

## The gateway hostname gets no certificate

**Symptom.** Agents cannot connect: the WebSocket dial fails at the TLS
handshake against `RIFT_GATEWAY_HOSTNAME`.

**Cause.** The gateway hostname is not a tunnel subdomain, so riftd's TLS-ask
endpoint must explicitly authorize a certificate for it — and it authorizes
exactly the value of `RIFT_GATEWAY_HOSTNAME`. If the value Caddy sees and the
value riftd sees disagree, riftd refuses the certificate and every agent
connection fails at the handshake.

**Fix.** Ensure `RIFT_GATEWAY_HOSTNAME` is set to the same value for both the
Caddy and riftd containers (the compose files read it from one `.env`), that it
resolves to the host, and that its first label is on the subdomain blocklist so
no tunnel can claim it. See
[Deploying to a server](/guides/deploying/#configure-secrets).
