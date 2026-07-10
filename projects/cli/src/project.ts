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
// flag is validated and mapped in one place and the config can express anything
// the CLI can.

import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

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

/** Parse a project config file by extension. Throws Error on malformed input. */
export function parseProjectConfig(text: string, path: string): ProjectConfig {
  let doc: unknown;
  if (path.endsWith(".json")) {
    doc = JSON.parse(text);
  } else if (path.endsWith(".toml")) {
    doc = Bun.TOML.parse(text);
  } else {
    doc = Bun.YAML.parse(text);
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
 * for a structurally invalid entry.
 */
export function tunnelToArgv(
  name: string,
  entry: unknown,
): { argv: string[] } | { error: string } {
  if (!isRecord(entry)) {
    return { error: `tunnel "${name}" must be a mapping` };
  }
  const proto = entry.proto ?? entry.protocol ?? "http";
  if (typeof proto !== "string") {
    return { error: `tunnel "${name}": proto must be a string` };
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
    if (entry.subdomain !== "") {
      argv.push(entry.subdomain);
    }
  }

  for (const [key, value] of Object.entries(entry)) {
    if (POSITIONAL_KEYS.has(key)) {
      continue;
    }
    const flag = `--${key}`;
    if (typeof value === "boolean") {
      if (value) {
        argv.push(flag);
      }
      continue;
    }
    if (Array.isArray(value)) {
      for (const v of value) {
        argv.push(flag, scalarToString(v));
      }
      continue;
    }
    argv.push(flag, scalarToString(value));
  }
  return { argv };
}

function scalarToString(v: unknown): string {
  if (typeof v === "string") {
    return v;
  }
  if (typeof v === "number" || typeof v === "boolean") {
    return String(v);
  }
  // A non-scalar (nested mapping/array-of-arrays) is passed through as its
  // string form; parseArgs then rejects it with a flag-specific message.
  return String(v);
}
