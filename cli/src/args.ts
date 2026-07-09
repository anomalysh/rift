// Command-line argument parser. Produces a discriminated union so the caller
// handles run/help/version/error without any partial or ambiguous state.
//
//   rift <protocol> <port> [subdomain] [flags]

import {
  LOG_LEVELS,
  SUPPORTED_PROTOCOLS,
  type LogLevel,
  type SupportedProtocol,
} from "./constants.ts";
import type { PartialConfig } from "./config.ts";
import { isLogLevel } from "./logger.ts";

/** Flag values that feed configuration resolution (see config.ts). */
export interface FlagConfig {
  token?: string;
  server?: string;
  host?: string;
  logLevel?: LogLevel;
  insecure?: boolean;
}

export type ParsedArgs =
  | {
      kind: "run";
      protocol: SupportedProtocol;
      port: number;
      subdomain?: string;
      flags: FlagConfig;
    }
  | { kind: "set-config"; updates: PartialConfig }
  | { kind: "help" }
  | { kind: "version" }
  | { kind: "error"; message: string };

/** Run flags that take a value; the rest are booleans. */
const VALUE_FLAGS = new Set(["--token", "--server", "--host", "--log-level"]);

// `--set-*` flags do not open a tunnel: they persist a value to the config file
// and exit. They mirror the run flags one-for-one so `rift --set-token <t>`
// saves the same setting `--token <t>` would supply for a single run.
const SET_FLAGS = new Set([
  "--set-token",
  "--set-server",
  "--set-host",
  "--set-log-level",
]);

function parsePort(raw: string): number | null {
  // Strict integer: reject "3000.5", "0x10", " 80", "abc", "" up front.
  if (!/^\d+$/.test(raw)) {
    return null;
  }
  const port = Number.parseInt(raw, 10);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    return null;
  }
  return port;
}

function isSupportedProtocol(v: string): v is SupportedProtocol {
  return SUPPORTED_PROTOCOLS.some((p) => p === v);
}

export function parseArgs(argv: readonly string[]): ParsedArgs {
  const positionals: string[] = [];
  const flags: FlagConfig = {};
  const updates: PartialConfig = {};
  let hasSet = false;

  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === undefined) {
      continue;
    }

    if (arg === "--help" || arg === "-h") {
      return { kind: "help" };
    }
    if (arg === "--version" || arg === "-v") {
      return { kind: "version" };
    }
    if (arg === "--insecure") {
      flags.insecure = true;
      continue;
    }

    if (arg.startsWith("--")) {
      // Support both `--flag value` and `--flag=value`.
      const eq = arg.indexOf("=");
      const name = eq === -1 ? arg : arg.slice(0, eq);
      const isSet = SET_FLAGS.has(name);
      if (!VALUE_FLAGS.has(name) && !isSet) {
        return { kind: "error", message: `unknown flag: ${name}` };
      }
      let value: string | undefined;
      if (eq === -1) {
        value = argv[i + 1];
        i++;
      } else {
        value = arg.slice(eq + 1);
      }
      if (value === undefined) {
        return { kind: "error", message: `flag ${name} requires a value` };
      }
      let applied: string | null;
      if (isSet) {
        hasSet = true;
        applied = applySetFlag(updates, name, value);
      } else {
        applied = applyValueFlag(flags, name, value);
      }
      if (applied !== null) {
        return { kind: "error", message: applied };
      }
      continue;
    }

    if (arg.startsWith("-") && arg !== "-") {
      return { kind: "error", message: `unknown flag: ${arg}` };
    }

    positionals.push(arg);
  }

  // A `--set-*` invocation persists config and exits; it does not open a
  // tunnel, so it takes no positional arguments.
  if (hasSet) {
    if (positionals.length > 0) {
      return {
        kind: "error",
        message: `--set-* saves configuration and cannot be combined with a tunnel command (got ${JSON.stringify(positionals[0])})`,
      };
    }
    return { kind: "set-config", updates };
  }

  if (positionals.length === 0) {
    return { kind: "error", message: "missing <protocol> and <port>" };
  }
  const [protocol, portRaw, subdomain, ...extra] = positionals;
  if (extra.length > 0) {
    return { kind: "error", message: `unexpected argument: ${extra[0]}` };
  }
  if (protocol === undefined || !isSupportedProtocol(protocol)) {
    return {
      kind: "error",
      message: `unsupported protocol ${JSON.stringify(protocol ?? "")}; supported: ${SUPPORTED_PROTOCOLS.join(", ")}`,
    };
  }
  if (portRaw === undefined) {
    return { kind: "error", message: "missing <port>" };
  }
  const port = parsePort(portRaw);
  if (port === null) {
    return {
      kind: "error",
      message: `invalid port ${JSON.stringify(portRaw)}: expected an integer in 1..65535`,
    };
  }

  const result: ParsedArgs = { kind: "run", protocol, port, flags };
  if (subdomain !== undefined && subdomain !== "") {
    result.subdomain = subdomain;
  }
  return result;
}

/** Apply a value flag; returns an error message string, or null on success. */
function applyValueFlag(
  flags: FlagConfig,
  name: string,
  value: string,
): string | null {
  switch (name) {
    case "--token":
      flags.token = value;
      return null;
    case "--server":
      flags.server = value;
      return null;
    case "--host":
      flags.host = value;
      return null;
    case "--log-level":
      if (!isLogLevel(value)) {
        return `invalid --log-level ${JSON.stringify(value)}: expected one of ${LOG_LEVELS.join(", ")}`;
      }
      flags.logLevel = value;
      return null;
    default:
      return `unknown flag: ${name}`;
  }
}

/** Apply a `--set-*` flag into the pending config updates. */
function applySetFlag(
  updates: PartialConfig,
  name: string,
  value: string,
): string | null {
  if (value === "") {
    return `flag ${name} requires a non-empty value`;
  }
  switch (name) {
    case "--set-token":
      updates.token = value;
      return null;
    case "--set-server":
      updates.server = value;
      return null;
    case "--set-host":
      updates.host = value;
      return null;
    case "--set-log-level":
      if (!isLogLevel(value)) {
        return `invalid --set-log-level ${JSON.stringify(value)}: expected one of ${LOG_LEVELS.join(", ")}`;
      }
      updates.logLevel = value;
      return null;
    default:
      return `unknown flag: ${name}`;
  }
}

/** Usage text for `--help`. */
export function usageText(): string {
  return `rift — expose a local port through the rift gateway

USAGE
  rift <protocol> <port> [subdomain] [flags]

EXAMPLES
  rift http 3000                 open an HTTP tunnel with a random subdomain
  rift http 3000 myapp           request the subdomain "myapp"
  rift tcp 22                    expose local TCP port 22 on a gateway port
  rift tls 8443 myapp            SNI-route myapp.<domain> to a local TLS service
  rift --set-token rift_xxx      save the auth token to the config file
  rift --set-server wss://...    save the gateway URL to the config file

ARGUMENTS
  <protocol>   what to tunnel (supported: ${SUPPORTED_PROTOCOLS.join(", ")})
                 http  routed by subdomain over the shared gateway
                 tcp   raw TCP, reached on a public port the gateway allocates
                 tls   raw TLS, SNI-routed; the local service terminates TLS
  <port>       local TCP port to forward to (1..65535)
  [subdomain]  desired subdomain (http/tls); the gateway picks one if omitted

FLAGS
  --token <t>        gateway auth token        (env ${"RIFT_TOKEN"})
  --server <url>     gateway ws/wss URL        (env ${"RIFT_SERVER"})
  --host <host>      local host to forward to  (env ${"RIFT_HOST"}, default 127.0.0.1)
  --log-level <lvl>  ${LOG_LEVELS.join(" | ")}
  --insecure         skip TLS certificate verification (wss only)
  --version, -v      print version and exit
  --help, -h         print this help and exit

PERSIST CONFIG
  --set-token <t>       save the auth token, then exit
  --set-server <url>    save the gateway URL, then exit
  --set-host <host>     save the default local host, then exit
  --set-log-level <lvl> save the default log level, then exit
  Values are written to ~/.config/rift/config.json (created 0700, file 0600).

CONFIG
  Precedence (highest first): flags > env vars > ~/.config/rift/config.json > defaults.
  token and server have no default; one must be supplied or rift exits with an error.`;
}
