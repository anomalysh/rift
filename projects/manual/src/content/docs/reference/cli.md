---
title: CLI reference
description: Every argument, flag, environment variable and exit code of the rift agent.
---

The `rift` agent opens a WebSocket to the gateway and forwards public HTTP
requests to a local port, streaming both directions.

```text
rift <protocol> <port> [subdomain] [flags]
```

The tunnel URL banner is printed to **stdout**; all diagnostics go to **stderr**,
so `rift http 3000 | …` yields just the URL line.

## Arguments

| Argument      | Required | Meaning                                                              |
| ------------- | -------- | ------------------------------------------------------------------- |
| `<protocol>`  | yes      | Application protocol to tunnel: `http`, `https`, `tcp`, or `tls`.   |
| `<port>`      | yes      | Local TCP port to forward to. An integer in `1..65535`.             |
| `[subdomain]` | no       | Desired subdomain. Omitted, the gateway picks one at random.        |

The protocol selects how the agent reaches your local service:

| Protocol | Local service          | On the wire                                              |
| -------- | ---------------------- | ------------------------------------------------------- |
| `http`   | plain HTTP             | routed by subdomain; the gateway terminates edge TLS    |
| `https`  | a local **HTTPS** server | routed by subdomain; the gateway still terminates edge TLS |
| `tcp`    | any TCP service        | reached on a gateway-allocated public port              |
| `tls`    | a service that terminates its own TLS | SNI-routed; bytes pass through encrypted    |

`https` is identical to `http` at the edge — the public URL is still
`https://…` and the gateway terminates TLS as always. The only difference is
that the agent dials your local service over TLS, so you can point rift at a
dev server that only speaks HTTPS (a self-signed certificate is fine on
loopback; see `--upstream-insecure`).

## Flags

Value flags accept both `--flag value` and `--flag=value` forms.

| Flag                 | Env var          | Meaning                                                  |
| -------------------- | ---------------- | -------------------------------------------------------- |
| `--token <t>`        | `RIFT_TOKEN`     | Gateway auth token. No default; one must be supplied.    |
| `--server <url>`     | `RIFT_SERVER`    | Gateway `ws://` / `wss://` URL. No default; required.    |
| `--host <host>`      | `RIFT_HOST`      | Local host to forward to. Default `127.0.0.1`.           |
| `--log-level <lvl>`  | `RIFT_LOG_LEVEL` | `debug`, `info`, `warn`, `error`, or `silent`. Default `info`. |
| `--insecure`         | —                | Skip TLS certificate verification on the gateway `wss` connection. |
| `--upstream-insecure`| —                | Skip verification of the local HTTPS upstream's certificate (`https` tunnels). |
| `--version`, `-v`    | —                | Print the version and exit.                              |
| `--help`, `-h`       | —                | Print usage and exit.                                    |

An unknown flag, a value flag with no value, or an unexpected extra positional
argument is a usage error (exit code 2).

## Examples

```sh
rift http 3000                 # tunnel localhost:3000 with a random subdomain
rift http 3000 myapp           # request the subdomain "myapp"
rift https 8443                # tunnel a local HTTPS server (self-signed ok)

# Point at a specific gateway with a token and quiet logging:
rift http 8080 --server wss://gateway.example.com/tunnel --token rift_xxx --log-level warn
```

For an `https` tunnel the agent verifies the upstream certificate by default,
except on a loopback host (`127.0.0.1`, `::1`, `localhost`), where a self-signed
dev certificate is expected and verification is skipped automatically. Point
rift at an HTTPS service on another host with a self-signed certificate and pass
`--upstream-insecure` to skip verification there too. This is independent of
`--insecure`, which governs only the gateway `wss` connection.

## Environment variables

| Variable          | Purpose                                                         |
| ----------------- | -------------------------------------------------------------- |
| `RIFT_TOKEN`      | Gateway auth token (overridden by `--token`).                   |
| `RIFT_SERVER`     | Gateway `ws://` / `wss://` URL (overridden by `--server`).      |
| `RIFT_HOST`       | Local host to forward to (overridden by `--host`).              |
| `RIFT_LOG_LEVEL`  | Log verbosity (overridden by `--log-level`).                    |
| `XDG_CONFIG_HOME` | Base directory for the config file; falls back to `$HOME/.config`. |
| `HOME`            | Used to derive the config directory when `XDG_CONFIG_HOME` is unset. |

Settings resolve from flags, then environment variables, then the config file at
`~/.config/rift/config.json`, then built-in defaults. See
[Configuration](/getting-started/configuration/) for the full precedence rules.

## Exit codes

| Code | Meaning                                          |
| ---- | ------------------------------------------------ |
| `0`  | Clean shutdown.                                  |
| `1`  | Runtime or configuration error (e.g. a missing or rejected token). |
| `2`  | Usage error (invalid arguments).                 |
| `130`| Terminated by SIGINT (128 + 2).                  |
| `143`| Terminated by SIGTERM (128 + 15).                |
