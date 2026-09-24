// Command-line argument parser. Produces a discriminated union so the caller
// handles run/help/version/error without any partial or ambiguous state.
//
//   rift <protocol> <port> [subdomain] [flags]

import { CLI_SPEC, type CliOption } from "./cli-spec.ts";
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
  /** Read the token from this file instead of argv (see index.ts). */
  tokenFile?: string;
  server?: string;
  host?: string;
  logLevel?: LogLevel;
  insecure?: boolean;
  upstreamInsecure?: boolean;
  allowInsecureTransport?: boolean;
  // Visitor-access policy (A2-A5). Repeatable flags accumulate; the rest are
  // single-valued. Raw strings here; buildPolicy validates and hashes them.
  basicAuth?: string[];
  allowIp?: string[];
  denyIp?: string[];
  ttl?: string;
  once?: boolean;
  maxRequests?: string;
  rateLimit?: string;
  // Traffic policy (T1-T3, T5, T6), applied agent-side by the forwarder.
  // Repeatable flags accumulate; buildTrafficPolicy parses and validates them.
  setRequestHeader?: string[];
  delRequestHeader?: string[];
  setResponseHeader?: string[];
  delResponseHeader?: string[];
  cors?: boolean;
  corsOrigin?: string[];
  respond?: string[];
  redirect?: string[];
  route?: string[];
  breaker?: boolean;
  breakerThreshold?: string;
  // BYO custom domains (E1). Repeatable; each is routed to this tunnel.
  domain?: string[];
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
  | { kind: "start"; names: string[] }
  | { kind: "man" }
  | { kind: "completions"; shell: Shell }
  | { kind: "help" }
  | { kind: "version" }
  | { kind: "error"; message: string };

function isShell(v: string): v is Shell {
  return (COMPLETION_SHELLS as readonly string[]).includes(v);
}

/**
 * Every option by its long form and short alias. The parser reads the flag
 * surface from CLI_SPEC -- the same spec the help, man page, and completions
 * are rendered from -- so a documented flag is always an accepted one.
 */
const OPTIONS: ReadonlyMap<string, CliOption> = new Map(
  CLI_SPEC.options.flatMap((o): [string, CliOption][] =>
    o.short !== undefined
      ? [
          [o.long, o],
          [o.short, o],
        ]
      : [[o.long, o]],
  ),
);

/** A run flag's FlagConfig key: its name in camelCase ("--log-level" -> logLevel). */
export function flagKey(long: string): string {
  return long.slice(2).replace(/-([a-z])/g, (_, c: string) => c.toUpperCase());
}

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

export function isSupportedProtocol(v: string): v is SupportedProtocol {
  return (SUPPORTED_PROTOCOLS as readonly string[]).includes(v);
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

    if (arg.startsWith("-") && arg !== "-") {
      // Support both `--flag value` and `--flag=value`.
      const eq = arg.startsWith("--") ? arg.indexOf("=") : -1;
      const name = eq === -1 ? arg : arg.slice(0, eq);
      const option = OPTIONS.get(name);
      // A switch takes no value, so `--cors=yes` is not a flag we know.
      if (option === undefined || (!option.takesValue && eq !== -1)) {
        return { kind: "error", message: `unknown flag: ${name}` };
      }
      if (option.kind === "meta") {
        return { kind: option.long === "--help" ? "help" : "version" };
      }
      if (!option.takesValue) {
        (flags as Record<string, unknown>)[flagKey(option.long)] = true;
        continue;
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
      if (option.kind === "persist") {
        hasSet = true;
        applied = applySetFlag(updates, name, value);
      } else {
        applied = applyValueFlag(flags, option, value);
      }
      if (applied !== null) {
        return { kind: "error", message: applied };
      }
      continue;
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

  if (flags.token !== undefined && flags.tokenFile !== undefined) {
    return {
      kind: "error",
      message: "--token and --token-file are mutually exclusive",
    };
  }

  // `rift start [name...]` opens named tunnels from the project config (D3).
  // The per-tunnel flags live in the config file, so start itself takes none.
  if (positionals[0] === "start") {
    return { kind: "start", names: positionals.slice(1) };
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

/** Apply a run value flag; returns an error message string, or null on success. */
function applyValueFlag(
  flags: FlagConfig,
  option: CliOption,
  value: string,
): string | null {
  if (option.long === "--token-file" && value === "") {
    return "flag --token-file requires a non-empty path";
  }
  if (option.long === "--log-level" && !isLogLevel(value)) {
    return `invalid --log-level ${JSON.stringify(value)}: expected one of ${LOG_LEVELS.join(", ")}`;
  }
  // FlagConfig's keys are the run flags in camelCase (a test pins the two
  // together); repeatable flags accumulate, the rest keep the last value.
  const record = flags as Record<string, unknown>;
  const key = flagKey(option.long);
  if (option.repeatable === true) {
    const prior = record[key];
    record[key] = [...(Array.isArray(prior) ? prior : []), value];
  } else {
    record[key] = value;
  }
  return null;
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
      // "-" means "read the token from stdin" (resolved in index.ts), so the
      // secret need not appear in argv, `ps`, or shell history.
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
