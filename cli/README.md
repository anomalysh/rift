# rift

A fully-typed Bun/TypeScript single-binary ngrok-style tunnel agent. It opens a
WebSocket to the rift gateway and forwards public HTTP requests to a local port,
streaming both directions.

## Install

Requires [Bun](https://bun.sh) 1.3+.

```sh
bun install          # install dev dependencies
bun run build        # compile a standalone binary to ./dist/rift
```

`bun run build` produces a single self-contained executable (`dist/rift`) with
an embedded source map. You can also run from source with `bun run src/index.ts`.

## Install a release build

Prebuilt binaries are published for Linux (glibc and musl, x64/arm64), macOS
(x64/arm64), and Windows (x64) on the GitHub releases page.

### `curl | sh` (recommended)

```sh
curl -fsSL https://raw.githubusercontent.com/anomaly-sh/rift/main/tools/install.sh | sh
```

The installer detects your OS, architecture, and (on Linux) glibc vs musl,
downloads the matching binary from the latest release, **verifies its SHA256
against the published `SHA256SUMS` and refuses to install on any mismatch**, then
installs to `/usr/local/bin` (or `~/.local/bin` if that is not writable). It
never runs `sudo` for you; if a chosen directory needs elevated rights it prints
the exact command to run.

Override its behaviour with environment variables:

| Variable                | Default                | Purpose                          |
| ----------------------- | ---------------------- | -------------------------------- |
| `RIFT_INSTALL_VERSION`  | latest release         | install a specific version       |
| `RIFT_INSTALL_DIR`      | `/usr/local/bin` etc.  | install into a specific directory|
| `RIFT_INSTALL_REPO`     | `anomaly-sh/rift`      | source GitHub `owner/repo`       |
| `RIFT_INSTALL_BASE_URL` | GitHub releases URL    | mirror / custom download base    |

Run `sh install.sh --help`, or add `--version <v>`, `--dir <path>`, or
`--dry-run`.

### Manual download with checksum verification

Every release ships a `SHA256SUMS` file. Always verify before running the binary.

```sh
VERSION=0.1.0
BASE="https://github.com/anomaly-sh/rift/releases/download/v${VERSION}"
# Pick the artifact for your platform, e.g. rift-linux-x64, rift-linux-x64-musl,
# rift-darwin-arm64, rift-windows-x64.exe
ARTIFACT=rift-linux-x64

curl -fsSLO "${BASE}/${ARTIFACT}"
curl -fsSLO "${BASE}/SHA256SUMS"

# GNU coreutils: verify only the file you downloaded.
sha256sum --ignore-missing -c SHA256SUMS
# macOS: compare manually —
#   grep " ${ARTIFACT}\$" SHA256SUMS | awk '{print $1}'
#   shasum -a 256 "${ARTIFACT}" | awk '{print $1}'

chmod +x "${ARTIFACT}"
sudo install -m 0755 "${ARTIFACT}" /usr/local/bin/rift
```

### Man page and shell completions

The `.tar.gz` / `.zip` archives bundle the binary, the man page, and shell
completions:

```sh
tar -xzf rift-linux-x64.tar.gz
cd rift-linux-x64

sudo install -m 0755 rift    /usr/local/bin/rift
sudo install -m 0644 rift.1  /usr/local/share/man/man1/rift.1   # man rift

# Completions (adjust paths to your shell's convention):
install -Dm 0644 completions/rift.bash ~/.local/share/bash-completion/completions/rift
install -Dm 0644 completions/rift.fish ~/.config/fish/completions/rift.fish
install -Dm 0644 completions/rift.zsh  ~/.local/share/zsh/site-functions/_rift  # ensure on $fpath
```

## Usage

```
rift <protocol> <port> [subdomain] [flags]

rift http 3000               # tunnel localhost:3000 with a random subdomain
rift http 3000 myapp         # request the subdomain "myapp"
```

Only `http` is supported today. The port must be an integer in `1..65535`.

The tunnel URL banner is printed to **stdout**; all diagnostics go to **stderr**,
so `rift http 3000 | …` yields just the URL line.

### Flags

| Flag              | Meaning                                             |
| ----------------- | --------------------------------------------------- |
| `--token <t>`     | gateway auth token                                  |
| `--server <url>`  | gateway `ws://` / `wss://` URL                      |
| `--host <host>`   | local host to forward to (default `127.0.0.1`)      |
| `--log-level <l>` | `debug` \| `info` \| `warn` \| `error` \| `silent`  |
| `--insecure`      | skip TLS certificate verification (`wss` only)      |
| `--version`, `-v` | print version and exit                              |
| `--help`, `-h`    | print help and exit                                 |

Flags accept both `--flag value` and `--flag=value` forms.

## Configuration

Settings are resolved from four layers. **Higher layers win.**

| Precedence | Source                                     | Provides                          |
| ---------- | ------------------------------------------ | --------------------------------- |
| 1 (high)   | CLI flags / positional args                | all settings                      |
| 2          | environment variables                      | token, server, host, log level    |
| 3          | config file `~/.config/rift/config.json`   | token, server, host, log level    |
| 4 (low)    | built-in defaults                          | host, log level only              |

`token` and `server` have **no default**: if neither a flag, env var, nor config
file supplies one, rift exits with a clear, actionable error (exit code 1)
naming exactly where to set it.

### Environment variables

| Variable          | Overrides    |
| ----------------- | ------------ |
| `RIFT_TOKEN`      | `--token`    |
| `RIFT_SERVER`     | `--server`   |
| `RIFT_HOST`       | `--host`     |
| `RIFT_LOG_LEVEL`  | `--log-level`|

`XDG_CONFIG_HOME` is honoured when locating the config file; it falls back to
`$HOME/.config`.

### Config file

`~/.config/rift/config.json` (or `$XDG_CONFIG_HOME/rift/config.json`):

```json
{
  "token": "rift_xxx",
  "server": "wss://gateway.example.com",
  "host": "127.0.0.1",
  "logLevel": "info"
}
```

Unknown keys are ignored; invalid types or an unknown `logLevel` are a hard error.

## Exit codes

| Code | Meaning                                        |
| ---- | ---------------------------------------------- |
| 0    | clean shutdown                                 |
| 1    | runtime / configuration error (e.g. bad token) |
| 2    | usage error (bad arguments)                    |
| 130  | terminated by SIGINT                           |
| 143  | terminated by SIGTERM                          |

## Development

```sh
bun run typecheck    # tsc --noEmit under strict flags
bun test             # unit + cross-language conformance tests
bun run build        # compile ./dist/rift
```

`test/conformance.test.ts` asserts that frames built by `src/protocol.ts` are
byte-identical to the Go reference encoder in
`server/internal/tunnelproto`, keeping the two implementations in lockstep. The
wire protocol contract is documented in `docs/PROTOCOL.md`.
