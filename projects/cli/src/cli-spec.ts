// The single source of truth for the rift command-line surface.
//
// Every user-facing description of the CLI — the `--help` banner, the groff man
// page, and the bash/zsh/fish completion scripts — is rendered from the one
// `CLI_SPEC` value below (see docgen.ts). Hand-written docs inevitably drift
// from the parser; deriving them all from a single typed spec makes that class
// of bug impossible. When the CLI grows a flag or a protocol, it is described
// here once and the generated artifacts follow.
//
// This module is intentionally data-only: it imports the wire/config constants
// so shared facts (version, protocol list, log levels, env-var names, exit
// codes) have exactly one spelling, and it exports no logic.

import {
  DEFAULTS,
  ENV,
  EXIT,
  LOG_LEVELS,
  type LogLevel,
  SUPPORTED_PROTOCOLS,
  type SupportedProtocol,
  VERSION,
} from "./constants.ts";

/**
 * Which command surface an option belongs to. This drives both rendering
 * (persist flags get their own CONFIG section) and the runtime contract in
 * args.ts, so the two can never disagree about what a flag does.
 *
 *   run     — a flag that influences the tunnel that `rift <proto> <port>` opens
 *   persist — a `--set-*` flag that writes ~/.config/rift/config.json and exits
 *   meta    — `--help` / `--version`, which short-circuit before anything runs
 */
export type OptionKind = "run" | "persist" | "meta";

/** A single command-line option (long form, optional short alias). */
export interface CliOption {
  /** Long form including the leading dashes, e.g. "--log-level". */
  readonly long: string;
  /** Short alias including the leading dash, e.g. "-v"; omitted if none. */
  readonly short?: string;
  /** Whether the option consumes the following token as its value. */
  readonly takesValue: boolean;
  /** Value placeholder shown in docs (e.g. "token", "url", "level"). */
  readonly placeholder?: string;
  /** One-line help, reused verbatim as the completion description. */
  readonly help: string;
  /** Command surface this option belongs to (see OptionKind). */
  readonly kind: OptionKind;
  /** Environment variable this option overrides, if any. */
  readonly env?: string;
  /** Built-in default, surfaced in docs, if the option has one. */
  readonly default?: string;
  /** Enumerated legal values, used to offer value completion. */
  readonly values?: readonly string[];
}

/** An application protocol accepted as the first positional argument. */
export interface CliProtocol {
  readonly name: SupportedProtocol;
  readonly blurb: string;
}

/** A positional argument in the run form. */
export interface CliPositional {
  readonly name: string;
  readonly required: boolean;
  readonly help: string;
}

/** A worked example: the command line and what it does. */
export interface CliExample {
  readonly cmd: string;
  readonly desc: string;
}

/** An environment variable read during configuration resolution. */
export interface CliEnvVar {
  readonly name: string;
  readonly help: string;
}

/** A documented process exit code. */
export interface CliExitStatus {
  readonly code: number;
  readonly meaning: string;
}

/** The complete, render-agnostic description of the rift CLI. */
export interface CliSpec {
  readonly name: string;
  readonly version: string;
  /** One-line summary used in NAME and the help banner header. */
  readonly summary: string;
  /** DESCRIPTION paragraphs for the man page (one entry per paragraph). */
  readonly description: readonly string[];
  /** SYNOPSIS forms, most common first. */
  readonly synopsis: readonly string[];
  readonly protocols: readonly CliProtocol[];
  readonly positionals: readonly CliPositional[];
  readonly options: readonly CliOption[];
  readonly examples: readonly CliExample[];
  readonly env: readonly CliEnvVar[];
  readonly exitStatuses: readonly CliExitStatus[];
  /** Config file locations, most specific first. */
  readonly configFiles: readonly string[];
  /** Free-text SEE ALSO note. */
  readonly seeAlso: string;
}

const LEVELS_LIST = LOG_LEVELS.join(", ");

export const CLI_SPEC: CliSpec = {
  name: "rift",
  version: VERSION,
  summary: "expose a local port through the rift gateway",
  description: [
    "rift is a self-hosted, ngrok-style tunnel agent. It opens a WebSocket to " +
      "the rift gateway and forwards public traffic to a local port, streaming " +
      "both directions.",
    "The tunnel URL banner is printed to standard output; all diagnostics go " +
      "to standard error, so a pipeline such as `rift http 3000 | ...` yields " +
      "just the URL line.",
    "The completions and man subcommands, and the --set-* flags, need neither " +
      "a token nor a server: they print or persist and exit without opening a " +
      "tunnel.",
  ],
  synopsis: [
    "rift <protocol> <port> [subdomain] [options]",
    "rift --set-token <token> | --set-server <url> | --set-host <host> | --set-log-level <level>",
    "rift completions <bash|zsh|fish>",
    "rift man",
    "rift --version | -v",
    "rift --help | -h",
  ],
  protocols: [
    {
      name: "http",
      blurb:
        "forward HTTP requests; the gateway routes by subdomain and terminates TLS",
    },
    {
      name: "https",
      blurb:
        "forward HTTP to a local HTTPS upstream; the gateway still terminates edge TLS",
    },
    { name: "tcp", blurb: "forward a raw TCP stream to the local port" },
    {
      name: "tls",
      blurb: "forward a TLS stream, routed by SNI, to the local port",
    },
  ],
  positionals: [
    {
      name: "protocol",
      required: true,
      help: `application protocol to tunnel (one of ${SUPPORTED_PROTOCOLS.join(", ")})`,
    },
    {
      name: "port",
      required: true,
      help: "local TCP port to forward to (1..65535)",
    },
    {
      name: "subdomain",
      required: false,
      help: "desired subdomain; the gateway picks one at random if omitted",
    },
  ],
  options: [
    {
      long: "--token",
      takesValue: true,
      placeholder: "token",
      help: "gateway auth token",
      kind: "run",
      env: ENV.TOKEN,
    },
    {
      long: "--server",
      takesValue: true,
      placeholder: "url",
      help: "gateway ws/wss URL",
      kind: "run",
      env: ENV.SERVER,
    },
    {
      long: "--host",
      takesValue: true,
      placeholder: "host",
      help: "local host to forward to",
      kind: "run",
      env: ENV.HOST,
      default: DEFAULTS.HOST,
    },
    {
      long: "--log-level",
      takesValue: true,
      placeholder: "level",
      help: `log verbosity (${LEVELS_LIST})`,
      kind: "run",
      env: ENV.LOG_LEVEL,
      default: DEFAULTS.LOG_LEVEL,
      values: LOG_LEVELS,
    },
    {
      long: "--insecure",
      takesValue: false,
      help: "skip TLS certificate verification (wss only)",
      kind: "run",
    },
    {
      long: "--upstream-insecure",
      takesValue: false,
      help: "skip verification of the local HTTPS upstream's certificate",
      kind: "run",
    },
    {
      long: "--basic-auth",
      takesValue: true,
      placeholder: "user:pass",
      help: "require HTTP Basic auth to reach the tunnel (repeatable)",
      kind: "run",
    },
    {
      long: "--allow-ip",
      takesValue: true,
      placeholder: "cidr",
      help: "only admit visitors in this IP/CIDR (repeatable; default-deny)",
      kind: "run",
    },
    {
      long: "--deny-ip",
      takesValue: true,
      placeholder: "cidr",
      help: "reject visitors in this IP/CIDR (repeatable)",
      kind: "run",
    },
    {
      long: "--rate-limit",
      takesValue: true,
      placeholder: "n/s",
      help: "throttle visitors to N requests per second (e.g. 20/s)",
      kind: "run",
    },
    {
      long: "--ttl",
      takesValue: true,
      placeholder: "dur",
      help: "retire the tunnel after this long (e.g. 30m, 1h)",
      kind: "run",
    },
    {
      long: "--once",
      takesValue: false,
      help: "retire the tunnel after the first request",
      kind: "run",
    },
    {
      long: "--max-requests",
      takesValue: true,
      placeholder: "n",
      help: "retire the tunnel after N requests",
      kind: "run",
    },
    {
      long: "--set-token",
      takesValue: true,
      placeholder: "token",
      help: "persist token to the config file and exit",
      kind: "persist",
    },
    {
      long: "--set-server",
      takesValue: true,
      placeholder: "url",
      help: "persist server to the config file and exit",
      kind: "persist",
    },
    {
      long: "--set-host",
      takesValue: true,
      placeholder: "host",
      help: "persist host to the config file and exit",
      kind: "persist",
    },
    {
      long: "--set-log-level",
      takesValue: true,
      placeholder: "level",
      help: "persist log level to the config file and exit",
      kind: "persist",
      values: LOG_LEVELS,
    },
    {
      long: "--version",
      short: "-v",
      takesValue: false,
      help: "print version and exit",
      kind: "meta",
    },
    {
      long: "--help",
      short: "-h",
      takesValue: false,
      help: "print this help and exit",
      kind: "meta",
    },
  ],
  examples: [
    { cmd: "rift http 3000", desc: "open a tunnel with a random subdomain" },
    { cmd: "rift http 3000 myapp", desc: 'request the subdomain "myapp"' },
    {
      cmd: "rift https 8443",
      desc: "tunnel a local HTTPS server (self-signed ok)",
    },
    { cmd: "rift tcp 5432", desc: "forward a raw TCP port (e.g. Postgres)" },
    {
      cmd: "rift --set-server wss://gw.example.com",
      desc: "save a default gateway to the config file",
    },
  ],
  env: [
    { name: ENV.TOKEN, help: "gateway auth token (see --token)" },
    { name: ENV.SERVER, help: "gateway ws/wss URL (see --server)" },
    { name: ENV.HOST, help: "local host to forward to (see --host)" },
    { name: ENV.LOG_LEVEL, help: "log verbosity (see --log-level)" },
    {
      name: ENV.XDG_CONFIG_HOME,
      help: "base directory for the config file; falls back to $HOME/.config",
    },
    {
      name: ENV.HOME,
      help: "used to derive the config directory when XDG_CONFIG_HOME is unset",
    },
  ],
  exitStatuses: [
    { code: EXIT.OK, meaning: "Clean shutdown." },
    {
      code: EXIT.ERROR,
      meaning:
        "Runtime or configuration error (for example, a missing or rejected token).",
    },
    { code: EXIT.USAGE, meaning: "Usage error (invalid arguments)." },
    { code: EXIT.SIGINT, meaning: "Terminated by SIGINT (128 + 2)." },
    { code: EXIT.SIGTERM, meaning: "Terminated by SIGTERM (128 + 15)." },
  ],
  configFiles: [
    "$XDG_CONFIG_HOME/rift/config.json",
    "~/.config/rift/config.json",
  ],
  seeAlso:
    "Project documentation and the wire-protocol contract ship with the source tree (README.md, docs/PROTOCOL.md).",
};

/** All log levels, exported for renderers that offer value completion. */
export const SPEC_LOG_LEVELS: readonly LogLevel[] = LOG_LEVELS;
