// Command-line argument parser. Produces a discriminated union so the caller
// handles run/help/version/error without any partial or ambiguous state.
//
//   rift <protocol> <port> [subdomain] [flags]

import type { PartialConfig } from "./config.ts";
import {
  COMPLETION_SHELLS,
  LOG_LEVELS,
  type LogLevel,
  type Shell,
  SUPPORTED_PROTOCOLS,
  type SupportedProtocol,
} from "./constants.ts";
import { renderHelp } from "./docgen.ts";
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
  | { kind: "man" }
  | { kind: "completions"; shell: Shell }
  | { kind: "help" }
  | { kind: "version" }
  | { kind: "error"; message: string };

function isShell(v: string): v is Shell {
  return (COMPLETION_SHELLS as readonly string[]).includes(v);
}

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

  // Doc subcommands print and exit; they take no tunnel flags.
  if (positionals[0] === "man") {
    if (positionals.length > 1) {
      return { kind: "error", message: "man takes no arguments" };
    }
    return { kind: "man" };
  }
  if (positionals[0] === "completions") {
    const shell = positionals[1];
    if (shell === undefined) {
      return {
        kind: "error",
        message: `completions requires a shell: ${COMPLETION_SHELLS.join(" | ")}`,
      };
    }
    if (!isShell(shell)) {
      return {
        kind: "error",
        message: `unsupported shell ${JSON.stringify(shell)}; supported: ${COMPLETION_SHELLS.join(", ")}`,
      };
    }
    if (positionals.length > 2) {
      return {
        kind: "error",
        message: `unexpected argument: ${positionals[2]}`,
      };
    }
    return { kind: "completions", shell };
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

/** Usage text for `--help`, rendered from the single CLI spec so it can never
 *  drift from the man page and shell completions (see cli-spec.ts / docgen.ts). */
export function usageText(): string {
  return renderHelp();
}
