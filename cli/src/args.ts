// Command-line argument parser. Produces a discriminated union so the caller
// handles run/help/version/error without any partial or ambiguous state.
//
//   tunl <protocol> <port> [subdomain] [flags]

import {
  LOG_LEVELS,
  SUPPORTED_PROTOCOLS,
  type LogLevel,
  type SupportedProtocol,
} from "./constants.ts";
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
  | { kind: "help" }
  | { kind: "version" }
  | { kind: "error"; message: string };

/** Flags that take a value; the rest are booleans. */
const VALUE_FLAGS = new Set(["--token", "--server", "--host", "--log-level"]);

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
      if (!VALUE_FLAGS.has(name)) {
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
      const applied = applyValueFlag(flags, name, value);
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

/** Usage text for `--help`. */
export function usageText(): string {
  return `tunl — expose a local port through the tunl gateway

USAGE
  tunl <protocol> <port> [subdomain] [flags]

EXAMPLES
  tunl http 3000                 open a tunnel with a random subdomain
  tunl http 3000 myapp           request the subdomain "myapp"

ARGUMENTS
  <protocol>   application protocol to tunnel (supported: ${SUPPORTED_PROTOCOLS.join(", ")})
  <port>       local TCP port to forward to (1..65535)
  [subdomain]  desired subdomain; the gateway picks one at random if omitted

FLAGS
  --token <t>        gateway auth token        (env ${"TUNL_TOKEN"})
  --server <url>     gateway ws/wss URL        (env ${"TUNL_SERVER"})
  --host <host>      local host to forward to  (env ${"TUNL_HOST"}, default 127.0.0.1)
  --log-level <lvl>  ${LOG_LEVELS.join(" | ")}
  --insecure         skip TLS certificate verification (wss only)
  --version, -v      print version and exit
  --help, -h         print this help and exit

CONFIG
  Precedence (highest first): flags > env vars > ~/.config/tunl/config.json > defaults.
  token and server have no default; one must be supplied or tunl exits with an error.`;
}
