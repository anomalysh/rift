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
| `<protocol>`  | yes      | Application protocol to tunnel. The only supported value is `http`. |
| `<port>`      | yes      | Local TCP port to forward to. An integer in `1..65535`.             |
| `[subdomain]` | no       | Desired subdomain. Omitted, the gateway picks one at random.        |

`http` is the only accepted protocol in this build. TCP and TLS tunnelling are
reserved in the wire protocol but not implemented; passing anything but `http`
is a usage error.

## Flags

Value flags accept both `--flag value` and `--flag=value` forms.

| Flag                 | Env var          | Meaning                                                  |
| -------------------- | ---------------- | -------------------------------------------------------- |
| `--token <t>`        | `RIFT_TOKEN`     | Gateway auth token. No default; one must be supplied.    |
| `--server <url>`     | `RIFT_SERVER`    | Gateway `ws://` / `wss://` URL. No default; required.    |
| `--host <host>`      | `RIFT_HOST`      | Local host to forward to. Default `127.0.0.1`.           |
| `--log-level <lvl>`  | `RIFT_LOG_LEVEL` | `debug`, `info`, `warn`, `error`, or `silent`. Default `info`. |
| `--insecure`         | —                | Skip TLS certificate verification (`wss` only).          |
| `--version`, `-v`    | —                | Print the version and exit.                              |
| `--help`, `-h`       | —                | Print usage and exit.                                    |

An unknown flag, a value flag with no value, or an unexpected extra positional
argument is a usage error (exit code 2).

## Examples

```sh
rift http 3000                 # tunnel localhost:3000 with a random subdomain
rift http 3000 myapp           # request the subdomain "myapp"

# Point at a specific gateway with a token and quiet logging:
rift http 8080 --server wss://gateway.example.com/tunnel --token rift_xxx --log-level warn
```

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
