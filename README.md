# rift

A self-hosted ngrok: expose a local port on a public HTTPS URL under a domain
you own.

```console
$ rift http 3000 myapp

  rift  https://myapp.rift.example.com
    ->  http://127.0.0.1:3000
```

A Go gateway terminates agent WebSocket connections and routes public traffic
for `*.<base-domain>` into them. A Caddy instance in front handles wildcard TLS.
The agent is a single Bun binary.

## How it fits together

```
     public client                  your laptop
          |                              |
     https://myapp.rift.example.com      |  rift http 3000 myapp
          |                              |
          v                              v
   +-------------+               wss://gateway.rift.example.com
   |    Caddy    |  <--------------------+
   |  :80 :443   |     (one WebSocket per tunnel, frames multiplexed)
   +------+------+
          | :8080 ingress          +-------------+
          +----------------------> |    riftd    | :8081 gateway
                                   |             | :8082 admin (never public)
                                   +------+------+
                                          |
                                   +------+------+
                                   | PostgreSQL  |   Redis (optional,
                                   +-------------+    multi-node routing)
```

* **Caddy** terminates TLS and reverse-proxies to riftd. Certificates are issued
  **on demand**, authorized one hostname at a time by riftd.
* **riftd** runs three listeners: the public ingress, the agent gateway, and an
  admin API that is never exposed.
* **PostgreSQL** is the authority on which subdomain is occupied by whom.
* **Redis** is optional. Without it rift runs single-node, which is the default.

## Repository layout

| Path                        | What lives there                                   |
| --------------------------- | -------------------------------------------------- |
| `docs/PROTOCOL.md`          | The wire protocol. The contract between both sides. |
| `server/internal/core`      | Domain model and storage ports. Imports only stdlib. |
| `server/internal/tunnelproto` | Frame codec (Go side)                            |
| `server/internal/gateway`   | Agent WebSocket endpoint, stream multiplexing        |
| `server/internal/ingress`   | Public HTTP routing, Caddy TLS authorization         |
| `server/internal/adminapi`  | Token and reservation management                     |
| `server/internal/store`     | PostgreSQL adapter, migrations, in-memory adapter     |
| `server/internal/e2e`       | Integration tests: real gateway + real agent          |
| `cli/`                      | The Bun agent (`rift`)                               |
| `deploy/`                   | Caddyfile, Dockerfiles, compose stacks               |
| `tools/`                    | Operator scripts (ssh, deploy, mint-token)           |

`core` defines the interfaces; everything depends on `core` and `core` depends
on nothing. The gateway and the ingress never import each other: the gateway
hands the ingress an `http.RoundTripper` and that is the whole seam.

## Configuration

Every setting is an environment variable, declared exactly once in
`server/internal/config/keys.go`, with its default in `defaults.go`. Nothing is
hardcoded and nothing is spelled inline anywhere else. A value is either
defaulted or required; a missing required value fails at boot rather than
defaulting to a silent zero.

Copy `.env.example` to `.env` and fill it in. It documents every variable.

The ones with no default, because no sane default exists:

| Variable            | Meaning                                        |
| ------------------- | ---------------------------------------------- |
| `RIFT_BASE_DOMAIN`  | Tunnels are served at `<subdomain>.<this>`      |
| `RIFT_POSTGRES_DSN` | Database connection string                      |
| `RIFT_ADMIN_TOKEN`  | Bearer token for the admin API (≥32 chars in production) |

## Running locally

No public DNS, so there is no TLS and you address tunnels with a `Host` header.

```console
$ cp .env.example .env            # then fill in the required values
$ make up                         # postgres + riftd via docker compose
$ cd cli && bun install && bun run build && cd ..   # produces cli/dist/rift

# mint a token; the admin API listens on :8082 locally
$ RIFT_ADMIN_TOKEN=<your admin token> tools/mint-token.sh laptop

$ ./cli/dist/rift http 3000 demo --token rift_... --server ws://127.0.0.1:8081/tunnel
$ curl -H 'Host: demo.rift.localtest' http://127.0.0.1:8080/
```

`make help` lists the rest. `make build-cli` builds the CLI inside Docker
instead, if you would rather not install Bun.

Run the tests:

```console
$ cd server && go test ./...          # postgres tests skip without a database
$ cd cli && bun test
```

The Postgres store tests run against a real database when you point
`RIFT_TEST_POSTGRES_DSN` at one, and skip otherwise, so `go test ./...` stays
green on a laptop with nothing installed.

## Deploying

DNS must already resolve every label to the host:

```
A     *.rift.example.com  ->  <server ipv4>
AAAA  *.rift.example.com  ->  <server ipv6>
```

Then:

```console
$ tools/ssh-provision-key.sh                       # key auth, then disable passwords
$ tools/scp.sh .env /opt/rift/deploy/.env          # secrets live only here
$ tools/remote-deploy.sh
```

Secrets are read from the environment at runtime. `.env` is gitignored and
`.env.example` contains no credential — only empty placeholders.

### About TLS

Wildcard certificates require a DNS-01 challenge, which needs DNS provider API
credentials. rift does not assume you have them. Instead Caddy issues a
certificate **on demand**, the first time a hostname is requested, validated
over HTTP-01. This works because the wildcard A/AAAA records already point every
label at the host.

On-demand TLS without an authorization endpoint is an open certificate-issuance
relay: anyone who points a hostname at your IP could make you request
certificates on their behalf and burn your Let's Encrypt rate limit. Caddy is
therefore configured to `ask` riftd before issuing, and riftd approves only:

1. a subdomain with a live tunnel,
2. a subdomain someone has reserved, and
3. `RIFT_GATEWAY_HOSTNAME` itself.

If you set `RIFT_GATEWAY_HOSTNAME` in Caddy's environment but not riftd's, the
gateway gets no certificate and every agent connection fails at the handshake.
The two must agree.

## Admin API

Not exposed publicly. Reach it over SSH.

| Method   | Route                        | Purpose                          |
| -------- | ---------------------------- | -------------------------------- |
| `POST`   | `/v1/tokens`                 | Mint a token (plaintext returned **once**) |
| `GET`    | `/v1/tokens`                 | List tokens (never the secret)   |
| `DELETE` | `/v1/tokens/{id}`            | Revoke                           |
| `POST`   | `/v1/reservations`           | Pin a subdomain to a token       |
| `GET`    | `/v1/reservations`           | List reservations                |
| `DELETE` | `/v1/reservations/{subdomain}` | Release a reservation          |
| `GET`    | `/v1/tunnels`                | List live tunnels                |

A token's plaintext appears in the create response and nowhere else, ever.
Only its SHA-256 hash is stored.

Revoking a token tears down the tunnels it already opened, within
`RIFT_TOKEN_REVALIDATE_INTERVAL` — it does not merely stop it opening new ones.

## Design notes

Things that are easy to get wrong, and why they are the way they are.

**Subdomain claims are leases, not locks.** A tunnel row in Postgres is a claim
on a subdomain. A gateway that crashes without releasing its rows would hold
those subdomains forever, so a reaper collects rows whose agents stopped
heartbeating. On boot, riftd also clears the rows its own previous run left
behind, returning those subdomains immediately.

**Heartbeats are application-level.** WebSocket ping/pong is answered by
intermediaries and proves only that a proxy is alive. An application heartbeat
proves the agent's event loop is.

**Reconnect takes over before counting.** An agent whose socket dropped still
owns a tunnel row until the reaper runs. If reconnecting counted that row
against the per-token limit, a token with `max_tunnels=1` could never reconnect
to its own subdomain. So a reconnect reclaims its own row first, then counts.

**One writer per socket.** A WebSocket permits exactly one concurrent writer, so
every frame — including the final shutdown frame — goes through a single writer
goroutine.

**Routing stops when close is requested, not when the socket finishes closing.**
A well-behaved agent stops reading the moment it receives a shutdown frame, so
it never sends a close frame back and the graceful WebSocket close handshake
stalls. Waiting for it would leave the subdomain answering 502 instead of 404,
and unable to be reclaimed.

**Two implementations of one protocol drift.** `cli/test/conformance.test.ts`
asserts frames byte-for-byte against fixtures generated by the Go encoder. That
test is what keeps `protocol.ts` and `tunnelproto` honest.

## Status

HTTP tunnels, token auth, reserved subdomains, heartbeats, automatic cleanup,
and single-binary CLI all work and are covered by tests. TCP and TLS tunnelling
are reserved in the protocol but not implemented. Multi-node routing via Redis
is implemented but has only been exercised single-node.
