---
title: Server configuration
description: Every RIFT_* environment variable riftd reads — name, default, whether it is required, and what it does.
---

Every setting the `riftd` server reads is an environment variable prefixed
`RIFT_`, declared exactly once in `server/internal/config/keys.go` with its
default in `defaults.go`. Nothing is hardcoded elsewhere. A value is either
**defaulted** or **required**: a missing required value fails at boot with one
clear message rather than defaulting to a silent zero, and all configuration
problems are reported together in a single pass.

Copy `.env.example` to an untracked `.env` and fill it in; it documents every
variable. Booleans accept the usual `true`/`false`; durations are Go durations
such as `30s`, `2m`, `1h`; sizes are byte counts.

The three variables with no default, because no sane default exists, are
`RIFT_BASE_DOMAIN`, `RIFT_POSTGRES_DSN`, and `RIFT_ADMIN_TOKEN` (when the admin
API is enabled). `RIFT_TLS_MODE` additionally has no default in production.

## Core

| Variable       | Default          | Required | Description                                                        |
| -------------- | ---------------- | -------- | ------------------------------------------------------------------ |
| `RIFT_ENV`     | `development`    | no       | `development` or `production`. Production enables extra guardrails (a required TLS mode, a ≥32-char admin token). |
| `RIFT_NODE_ID` | auto-generated   | no       | Stable node identity, so a restarted process reclaims its own tunnel rows. Set one per node in a cluster. |

## Logging

| Variable          | Default | Required | Description                                  |
| ----------------- | ------- | -------- | -------------------------------------------- |
| `RIFT_LOG_LEVEL`  | `info`  | no       | One of `debug`, `info`, `warn`, `error`.     |
| `RIFT_LOG_FORMAT` | `json`  | no       | `json` or `text`.                            |

## Ingress (public HTTP listener)

| Variable                        | Default            | Required | Description                                                              |
| ------------------------------- | ------------------ | -------- | ----------------------------------------------------------------------- |
| `RIFT_INGRESS_ADDR`             | `:8080`            | no       | Listen address for proxied `*.RIFT_BASE_DOMAIN` traffic.                |
| `RIFT_INGRESS_READ_TIMEOUT`     | `30s`              | no       | Read-header timeout for the ingress listener.                           |
| `RIFT_INGRESS_WRITE_TIMEOUT`    | `0`                | no       | Write deadline. `0` disables it, so long streamed responses are not cut off. |
| `RIFT_INGRESS_IDLE_TIMEOUT`     | `120s`             | no       | Idle-connection timeout.                                                 |
| `RIFT_INGRESS_MAX_HEADER_BYTES` | `1048576` (1 MiB)  | no       | Maximum request header size.                                            |
| `RIFT_INGRESS_TRUSTED_PROXY_IPS`| (empty)            | no       | Comma-separated peers whose `X-Forwarded-For` is trusted. Empty trusts nobody and uses the socket peer. In production this is Caddy. |

## Gateway (agent WebSocket listener)

| Variable                          | Default   | Required | Description                                                                 |
| --------------------------------- | --------- | -------- | --------------------------------------------------------------------------- |
| `RIFT_GATEWAY_ADDR`               | `:8081`   | no       | Listen address for the WebSocket endpoint agents dial.                      |
| `RIFT_GATEWAY_HOSTNAME`           | (empty)   | no       | Public hostname agents dial. Used only so the TLS-ask endpoint can authorize a certificate for it. Empty in local dev. |
| `RIFT_GATEWAY_PATH`               | `/tunnel` | no       | WebSocket path. Must begin with `/`.                                        |
| `RIFT_GATEWAY_HANDSHAKE_TIMEOUT`  | `10s`     | no       | Timeout for the WebSocket upgrade handshake.                                |
| `RIFT_GATEWAY_WRITE_TIMEOUT`      | `30s`     | no       | Per-frame write timeout on the tunnel socket.                               |
| `RIFT_GATEWAY_ALLOWED_ORIGINS`    | (empty)   | no       | Comma-separated browser origins allowed to upgrade. Agents send no `Origin`, so empty is correct. |

## Admin API

| Variable            | Default | Required                  | Description                                                              |
| ------------------- | ------- | ------------------------- | ----------------------------------------------------------------------- |
| `RIFT_ADMIN_ENABLED`| `true`  | no                        | Whether the admin listener runs.                                        |
| `RIFT_ADMIN_ADDR`   | `:8082` | no                        | Admin listener address. Never published publicly.                       |
| `RIFT_ADMIN_TOKEN`  | (none)  | **when admin enabled**    | Bearer token for the admin API. Must be **≥32 characters in production**. |

## Postgres

| Variable                        | Default | Required | Description                                            |
| ------------------------------- | ------- | -------- | ------------------------------------------------------ |
| `RIFT_POSTGRES_DSN`             | (none)  | **yes**  | Connection string. The password component is a secret. |
| `RIFT_POSTGRES_MAX_CONNS`       | `10`    | no       | Max pool connections (must be ≥1 and ≥ min).           |
| `RIFT_POSTGRES_MIN_CONNS`       | `2`     | no       | Min pool connections (must not exceed max).            |
| `RIFT_POSTGRES_CONNECT_TIMEOUT` | `10s`   | no       | Connection timeout.                                    |
| `RIFT_POSTGRES_MIGRATE_ON_START`| `true`  | no       | Run migrations on boot.                                |

## Redis (optional; enables multi-node routing)

| Variable              | Default            | Required          | Description                                            |
| --------------------- | ------------------ | ----------------- | ------------------------------------------------------ |
| `RIFT_REDIS_ENABLED`  | `false`            | no                | Turns on multi-node routing.                           |
| `RIFT_REDIS_ADDR`     | `127.0.0.1:6379`   | when Redis enabled| Redis address.                                         |
| `RIFT_REDIS_PASSWORD` | (empty)            | no                | Redis auth, if required. Secret.                       |
| `RIFT_REDIS_DB`       | `0`                | no                | Redis logical database.                                |
| `RIFT_REDIS_PREFIX`   | `rift:`            | no                | Key prefix for rift's Redis keys.                      |

## Cluster

| Variable           | Default | Required                              | Description                                                            |
| ------------------ | ------- | ------------------------------------- | --------------------------------------------------------------------- |
| `RIFT_PEER_SECRET` | (empty) | **when Redis enabled (≥32 chars)**    | Authenticates node-to-node request forwarding on the internal proxy route. |

## TLS

These configure the Caddy reverse proxy in front of riftd, not riftd's own
listeners (which never terminate TLS). riftd validates them so a broken TLS
configuration fails at boot rather than as a later handshake error. See
[TLS modes](/guides/tls-modes/).

| Variable                 | Default                 | Required            | Description                                                    |
| ------------------------ | ----------------------- | ------------------- | -------------------------------------------------------------- |
| `RIFT_TLS_MODE`          | `internal` (dev only)   | **in production**   | One of `dns01`, `http01`, `self`, `internal`. No production default. |
| `RIFT_ACME_DNS_PROVIDER` | (empty)                 | **when `dns01`**    | Names the Caddy DNS solver, e.g. `rfc2136` or `acmedns`.       |
| `RIFT_TLS_CERT_FILE`     | (empty)                 | **when `self`**     | Certificate path inside the Caddy container.                  |
| `RIFT_TLS_KEY_FILE`      | (empty)                 | **when `self`**     | Private-key path inside the Caddy container.                  |

## Tunnel behaviour

| Variable                        | Default              | Required          | Description                                                                 |
| ------------------------------- | -------------------- | ----------------- | --------------------------------------------------------------------------- |
| `RIFT_BASE_DOMAIN`              | (none)               | **yes**           | Tunnels are served at `<subdomain>.<this>`. A fully qualified bare domain.   |
| `RIFT_PUBLIC_SCHEME`            | `https`              | no                | `http` or `https`; the scheme in the tunnel URL shown to agents.            |
| `RIFT_NODE_ADVERTISE_URL`       | (empty)              | **when Redis enabled** | Absolute URL peers use to reach this node's internal proxy.            |
| `RIFT_HEARTBEAT_INTERVAL`       | `15s`                | no                | How often an agent sends an application heartbeat.                          |
| `RIFT_HEARTBEAT_TIMEOUT`        | `45s`                | no                | No heartbeat within this window reaps the tunnel. **Must exceed the interval.** |
| `RIFT_REAPER_INTERVAL`          | `30s`                | no                | How often the reaper collects tunnels whose agents stopped heartbeating.   |
| `RIFT_TOKEN_REVALIDATE_INTERVAL`| `30s`                | no                | How often a live tunnel re-checks its token. Bounds how long a revoked token keeps serving. |
| `RIFT_REQUEST_TIMEOUT`          | `60s`                | no                | Per-request timeout through the tunnel.                                     |
| `RIFT_MAX_REQUEST_BODY_BYTES`   | `33554432` (32 MiB)  | no                | Max request body. `0` means unlimited.                                      |
| `RIFT_MAX_TUNNELS_PER_TOKEN`    | `5`                  | no                | Default concurrent-tunnel cap per token (must be ≥1). Overridable per token. |
| `RIFT_STREAM_BUFFER_SIZE`       | `32`                 | no                | Per-stream buffer depth (must be ≥1).                                       |

## Subdomain rules

| Variable                            | Default                              | Required | Description                                                        |
| ----------------------------------- | ------------------------------------ | -------- | ------------------------------------------------------------------ |
| `RIFT_SUBDOMAIN_MIN_LENGTH`         | `3`                                  | no       | Minimum subdomain length.                                          |
| `RIFT_SUBDOMAIN_MAX_LENGTH`         | `63`                                 | no       | Maximum subdomain length (the DNS label limit).                    |
| `RIFT_SUBDOMAIN_PATTERN`            | `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`  | no       | Regex a subdomain must match.                                      |
| `RIFT_SUBDOMAIN_BLOCKLIST`          | built-in list                        | no       | Comma-separated reserved labels. **Setting this replaces the built-in list; it does not extend it** — see the [caution here](/guides/subdomains-and-tokens/#the-blocklist). |
| `RIFT_SUBDOMAIN_GENERATED_LENGTH`   | `10`                                 | no       | Length of a randomly generated subdomain.                          |
| `RIFT_SUBDOMAIN_GENERATED_ALPHABET` | `abcdefghjkmnpqrstuvwxyz23456789`    | no       | Alphabet for generated subdomains (ambiguous glyphs omitted).      |

## Deployment-only variables

The following are **not read by riftd**. They drive Caddy, the `tools/` scripts,
and the compose files, and are listed here so the full `.env` surface is in one
place. The TLS provider credentials (`RIFT_DNS_*`, `RIFT_ACMEDNS_*`,
`RIFT_PDNS_*`, etc.) are covered under [DNS-01 providers](/guides/dns-providers/).

| Variable                  | Read by            | Purpose                                                            |
| ------------------------- | ------------------ | ----------------------------------------------------------------- |
| `RIFT_ACME_EMAIL`         | Caddy              | ACME account contact. Production Caddy fails to start if empty.    |
| `RIFT_ACME_CA_PROFILE`    | Caddy              | `public` (Let's Encrypt) or `internal-ca` (your own ACME server).  |
| `RIFT_ACME_CA_URL`        | Caddy              | ACME directory URL when `internal-ca`.                            |
| `RIFT_ACME_CA_ROOT`       | Caddy              | PEM signing the ACME server's own HTTPS cert when `internal-ca`.   |
| `RIFT_CADDY_IMAGE`        | compose            | Caddy image tag (`dns01` needs a plugin-built image).             |
| `RIFT_CADDY_DNS_PLUGINS`  | `build-caddy.sh`   | Space-separated Go module paths to compile into the Caddy image.  |
| `RIFT_CADDY_VERSION`      | `build-caddy.sh`   | Caddy major version or tag to build.                              |
| `RIFT_UPSTREAM_HOST`      | Caddy              | Compose service name Caddy proxies to (default `riftd`).          |
| `RIFT_INGRESS_PORT`       | Caddy              | Upstream ingress port Caddy dials. Must match `RIFT_INGRESS_ADDR`. |
| `RIFT_GATEWAY_PORT`       | Caddy              | Upstream gateway port Caddy dials. Must match `RIFT_GATEWAY_ADDR`. |
| `RIFT_TLS_CERT_DIR`       | compose            | Host directory bind-mounted to `/certs` for the `self` mode.      |
| `RIFT_VPS_HOST`           | `tools/`           | VPS IP or hostname for deploy/provision.                          |
| `RIFT_VPS_USER`           | `tools/`           | SSH user (default `root`).                                        |
| `RIFT_VPS_PORT`           | `tools/`           | SSH port (default `22`).                                          |
| `RIFT_VPS_PASSWORD`       | `tools/`           | Bootstrap-only SSH password; rotate after provisioning. Secret.   |
| `RIFT_ADMIN_URL`          | `mint-token.sh`    | Admin API base URL (default `http://127.0.0.1:8082`).             |
| `RIFT_INSTALL_*`          | `install.sh`       | CLI installer overrides — see [Installation](/getting-started/installation/). |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | compose | Throwaway credentials for the **local** dev Postgres container only. |
