---
title: How it works
description: The request path from a public browser to your local app, the Caddy/riftd/Postgres split, and the package boundary rule.
---

rift has three moving parts on the server — Caddy, riftd, and PostgreSQL (plus
optional Redis) — and a single-binary agent on your machine. This page follows a
request through them.

```text
     public client                  your laptop
          |                              |
     https://myapp.example.com           |  rift http 3000 myapp
          |                              |
          v                              v
   +-------------+               wss://gateway.example.com
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

## The components

- **Caddy** terminates TLS and reverse-proxies to riftd. Certificates are issued
  on demand, authorized one hostname at a time by riftd's TLS-ask endpoint. Its
  server timeouts are all disabled, because long-lived streams are the entire
  point — liveness is enforced by riftd's application heartbeat, not the proxy.
- **riftd** runs three listeners: the public **ingress** (`:8080`), the agent
  **gateway** (`:8081`), and an **admin** API (`:8082`) that is never exposed.
- **PostgreSQL** is the authority on which subdomain is occupied by whom.
- **Redis** is optional. Without it rift runs single-node, which is the default.

## The request path

When someone opens `https://myapp.example.com`:

1. **Caddy** accepts the TLS connection. It presents a certificate for the name
   (already held under `dns01`/`self`/`internal`, or minted on demand under
   `http01` after riftd authorizes it), then reverse-proxies the plaintext HTTP
   request to riftd's ingress on `:8080`.
2. **Ingress** extracts the subdomain from the `Host` header and looks it up in
   the registry. If a live agent session holds `myapp` on this node, ingress
   sends the request straight into that tunnel.
3. If no local session holds it, ingress asks Redis (when enabled) which node
   owns the subdomain, and forwards to that peer's internal proxy. With no live
   session and no peer, it returns a readable `404`.
4. The **gateway** carries the request to the agent as protocol frames over the
   one WebSocket for that tunnel: a `REQ_HEAD`, zero or more `REQ_BODY` chunks,
   and a `REQ_END`, multiplexed by `stream_id` alongside every other in-flight
   request.
5. The **agent** issues the request to your local app (`http://127.0.0.1:3000`
   by default) and streams the response back as `RES_HEAD`, `RES_BODY` chunks,
   and `RES_END`.
6. Ingress streams that response to the public client, flushing as it goes so
   server-sent events and other incremental responses are not buffered.

The agent's side of this is the same regardless of TLS mode or node count; all
of that is resolved before the request reaches the tunnel.

## The package boundary rule

The server's internal packages depend inward on a `core` package that defines
the domain model and storage interfaces and imports nothing but the standard
library. Everything depends on `core`; `core` depends on nothing.

The two halves of the data path — the **gateway** (which owns the WebSocket) and
the **ingress** (which owns public HTTP) — never import each other. The gateway
hands the ingress an `http.RoundTripper`, and that single interface is the whole
seam between them. The ingress does not know that tunnels are carried over
WebSockets at all; it just round-trips a request and streams the response.

This is what keeps the TLS-termination concern, the routing concern, and the
tunnel-transport concern from leaking into one another, and it is why the same
ingress code serves a local session and a peer-forwarded one identically.
