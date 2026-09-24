// Named multi-tunnel project config (D3). A rift.yml / .yaml / .toml / .json in
// the working directory declares a map of named tunnels:
//
//   tunnels:
//     web: { port: 3000, subdomain: myweb }
//     api: { port: 4000, cors: true, basic-auth: "user:pass" }
//
// `rift start web api` opens the named tunnels (or all of them when no name is
// given). Each entry is translated into the exact argv `rift <proto> <port>
// [sub] [flags]` would use, then run through the normal parseArgs, so every
// flag is validated and mapped in one place.
//
// A project file is not the user's own configuration: it arrives with a cloned
// repository, and `rift start` in that checkout runs it with the user's token.
// Because flags beat env and the config file, an unrestricted rift.yml could
// set `server: ws://attacker/` and receive the token, disable TLS verification,
// or point the tunnel at a LAN host (`host: 192.168.1.1`) and publish the
// router's admin page. So only tunnel-shape keys are accepted (PROJECT_KEYS);
// credentials and gateway transport settings are refused with an explanation,
// and `host` is accepted only when it names this machine.

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { CLI_SPEC } from "./cli-spec.ts";
import { isLoopbackHost } from "./config.ts";
import { ENV, SUPPORTED_PROTOCOLS } from "./constants.ts";
import { errorMessage } from "./logger.ts";
import { isRecord } from "./protocol.ts";

/** Config file names tried in order in the working directory. */
export const PROJECT_CONFIG_NAMES = [
  "rift.yml",
  "rift.yaml",
  "rift.toml",
  "rift.json",
] as const;

/** Keys handled positionally; every other key becomes a `--flag`. */
const POSITIONAL_KEYS = new Set(["proto", "protocol", "port", "subdomain"]);

/**
 * Flag keys a project file may set: the shape of the tunnel, its visitor
 * policy, and its traffic policy. `host` is here but further restricted to
 * loopback (see tunnelToArgv).
 */
export const PROJECT_KEYS: ReadonlySet<string> = new Set([
  "host",
  "log-level",
  "basic-auth",
  "allow-ip",
  "deny-ip",
  "rate-limit",
  "ttl",
  "once",
  "max-requests",
  "set-request-header",
  "del-request-header",
  "set-response-header",
  "del-response-header",
  "cors",
  "cors-origin",
  "respond",
  "redirect",
  "route",
  "breaker",
  "breaker-threshold",
  "domain",
]);

/**
 * Keys refused in a project file, each with where the setting belongs instead.
 * These decide where the token goes and how it is protected in transit.
 */
export const PROJECT_FORBIDDEN_KEYS: ReadonlyMap<string, string> = new Map([
  ["token", `use --token-file, ${ENV.TOKEN}, or rift --set-token`],
  ["token-file", `use --token-file, ${ENV.TOKEN}, or rift --set-token`],
  ["server", `use ${ENV.SERVER} or rift --set-server`],
  ["insecure", "pass --insecure yourself"],
  ["upstream-insecure", "pass --upstream-insecure yourself"],
  [
    "allow-insecure-transport",
    `set ${ENV.ALLOW_INSECURE_TRANSPORT}=1 yourself`,
  ],
]);

/** Spec lookup for a flag key (without dashes). */
function specOption(key: string): { takesValue: boolean } | undefined {
  return CLI_SPEC.options.find((o) => o.long === `--${key}`);
}

function isSupportedProtocol(v: string): boolean {
  return (SUPPORTED_PROTOCOLS as readonly string[]).includes(v);
}

export interface ProjectConfig {
  /** The tunnels map, name -> raw entry (validated lazily per selection). */
  readonly tunnels: Record<string, unknown>;
  /** Absolute path the config was read from, for error messages. */
  readonly path: string;
}

/** Locate the project config in cwd, or null when none is present. */
export function findProjectConfig(cwd: string): string | null {
  for (const name of PROJECT_CONFIG_NAMES) {
    const path = join(cwd, name);
    if (existsSync(path)) {
      return path;
    }
  }
  return null;
}

/**
 * Parse a project config file by extension. Throws an Error naming the file on
 * malformed input; the parsers' own SyntaxErrors do not say which file failed.
 */
export function parseProjectConfig(text: string, path: string): ProjectConfig {
  let doc: unknown;
  try {
    if (path.endsWith(".json")) {
      doc = JSON.parse(text);
    } else if (path.endsWith(".toml")) {
      doc = Bun.TOML.parse(text);
    } else {
      doc = Bun.YAML.parse(text);
    }
  } catch (err) {
    throw new Error(`${path}: ${errorMessage(err)}`);
  }
  if (!isRecord(doc) || !isRecord(doc.tunnels)) {
    throw new Error(`${path}: expected a top-level "tunnels" mapping`);
  }
  return { tunnels: doc.tunnels, path };
}

/** Load and parse the project config in cwd, or null when none exists. */
export function loadProjectConfig(cwd: string): ProjectConfig | null {
  const path = findProjectConfig(cwd);
  if (path === null) {
    return null;
  }
  return parseProjectConfig(readFileSync(path, "utf8"), path);
}

/**
 * Choose which tunnels to run. With no names, every declared tunnel is
 * selected (sorted for stable output). Names are validated against the config.
 */
export function selectTunnels(
  config: ProjectConfig,
  requested: readonly string[],
): { names: string[] } | { error: string } {
  const declared = Object.keys(config.tunnels);
  if (declared.length === 0) {
    return { error: `${config.path}: no tunnels are declared` };
  }
  if (requested.length === 0) {
    return { names: declared.sort() };
  }
  const missing = requested.filter((n) => !(n in config.tunnels));
  if (missing.length > 0) {
    return {
      error: `${config.path}: no such tunnel(s): ${missing.join(", ")} (declared: ${declared.join(", ")})`,
    };
  }
  return { names: [...requested] };
}

/**
 * Translate one named tunnel entry into the argv `rift` would take on the
 * command line, so parseArgs can validate and map it. Returns an error string
 * for a structurally invalid entry or a key a project file may not set.
 *
 * Every value is emitted as a single `--flag=value` token and positionals are
 * validated first, so no string in the file can itself be parsed as a flag
 * (e.g. `subdomain: "--server=ws://evil"` or `cors: "--insecure"`).
 */
export function tunnelToArgv(
  name: string,
  entry: unknown,
): { argv: string[] } | { error: string } {
  if (!isRecord(entry)) {
    return { error: `tunnel "${name}" must be a mapping` };
  }
  const proto = entry.proto ?? entry.protocol ?? "http";
  if (typeof proto !== "string" || !isSupportedProtocol(proto)) {
    return {
      error: `tunnel "${name}": proto must be one of ${SUPPORTED_PROTOCOLS.join(", ")}`,
    };
  }
  const port = entry.port;
  if (typeof port !== "number" || !Number.isInteger(port)) {
    return { error: `tunnel "${name}": port must be an integer` };
  }
  const argv: string[] = [proto, String(port)];

  if (entry.subdomain !== undefined) {
    if (typeof entry.subdomain !== "string") {
      return { error: `tunnel "${name}": subdomain must be a string` };
    }
    if (entry.subdomain.startsWith("-")) {
      return { error: `tunnel "${name}": subdomain must not start with "-"` };
    }
    if (entry.subdomain !== "") {
      argv.push(entry.subdomain);
    }
  }

  for (const [key, value] of Object.entries(entry)) {
    if (POSITIONAL_KEYS.has(key)) {
      continue;
    }
    const forbidden = PROJECT_FORBIDDEN_KEYS.get(key);
    if (forbidden !== undefined) {
      return {
        error:
          `tunnel "${name}": "${key}" cannot be set in a project file -- a checked-in ` +
          "rift.yml must not redirect or weaken where your token goes; " +
          `${forbidden}`,
      };
    }
    const option = specOption(key);
    if (!PROJECT_KEYS.has(key) || option === undefined) {
      return { error: `tunnel "${name}": unknown key "${key}"` };
    }
    const flag = `--${key}`;
    if (!option.takesValue) {
      if (typeof value !== "boolean") {
        return { error: `tunnel "${name}": ${key} must be true or false` };
      }
      if (value) {
        argv.push(flag);
      }
      continue;
    }
    const values = Array.isArray(value) ? value : [value];
    for (const v of values) {
      const str = scalarToString(v);
      if (str === null) {
        return {
          error: `tunnel "${name}": ${key} must be a string, number, or list of them`,
        };
      }
      if (key === "host" && !isLoopbackHost(str)) {
        return {
          error:
            `tunnel "${name}": host ${JSON.stringify(str)} is not this machine; a project ` +
            "file may only forward to loopback (127.0.0.1, ::1, localhost). To expose " +
            `another host, pass --host or set ${ENV.HOST} yourself`,
        };
      }
      argv.push(`${flag}=${str}`);
    }
  }
  return { argv };
}

function scalarToString(v: unknown): string | null {
  if (typeof v === "string") {
    return v;
  }
  if (typeof v === "number" || typeof v === "boolean") {
    return String(v);
  }
  return null;
}
