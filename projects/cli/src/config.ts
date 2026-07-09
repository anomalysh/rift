// Layered configuration resolution.
//
// Precedence, highest wins:
//   1. CLI flags / positional args
//   2. environment variables (RIFT_TOKEN, RIFT_SERVER, RIFT_HOST, RIFT_LOG_LEVEL)
//   3. config file ~/.config/rift/config.json  (honours XDG_CONFIG_HOME)
//   4. built-in defaults (host, log level only)
//
// `token` and `server` have no default: a missing one is a clear, actionable
// error rather than a crash.

import {
  chmodSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import { dirname, join } from "node:path";
import type { FlagConfig } from "./args.ts";
import {
  CONFIG_DIR_NAME,
  CONFIG_FILE_NAME,
  DEFAULTS,
  ENV,
  LOG_LEVELS,
  type LogLevel,
  XDG_CONFIG_FALLBACK,
} from "./constants.ts";
import { isLogLevel } from "./logger.ts";
import { isRecord } from "./protocol.ts";

/** Fully resolved, immutable runtime configuration. */
export interface ResolvedConfig {
  readonly token: string;
  readonly server: string;
  readonly host: string;
  readonly logLevel: LogLevel;
  readonly insecure: boolean;
  readonly upstreamInsecure: boolean;
}

/** A subset of settings, as loaded from a config file. */
export interface PartialConfig {
  token?: string;
  server?: string;
  host?: string;
  logLevel?: LogLevel;
}

export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ConfigError";
  }
}

export interface ResolveInput {
  flags: FlagConfig;
  env: Record<string, string | undefined>;
  file: PartialConfig | null;
  /** Config file path, named in "missing token/server" errors. */
  configPath: string;
}

function nonEmpty(value: string | undefined): string | undefined {
  return value !== undefined && value !== "" ? value : undefined;
}

/** Extract and validate the env-var layer. Throws on an invalid log level. */
function configFromEnv(env: Record<string, string | undefined>): PartialConfig {
  const out: PartialConfig = {};
  const token = nonEmpty(env[ENV.TOKEN]);
  if (token !== undefined) {
    out.token = token;
  }
  const server = nonEmpty(env[ENV.SERVER]);
  if (server !== undefined) {
    out.server = server;
  }
  const host = nonEmpty(env[ENV.HOST]);
  if (host !== undefined) {
    out.host = host;
  }
  const level = nonEmpty(env[ENV.LOG_LEVEL]);
  if (level !== undefined) {
    if (!isLogLevel(level)) {
      throw new ConfigError(
        `invalid ${ENV.LOG_LEVEL} ${JSON.stringify(level)}: expected one of ${LOG_LEVELS.join(", ")}`,
      );
    }
    out.logLevel = level;
  }
  return out;
}

/** Resolve the layered configuration. Pure: all inputs are passed in. */
export function resolveConfig(input: ResolveInput): ResolvedConfig {
  const { flags, file, configPath } = input;
  const env = configFromEnv(input.env);

  const token = flags.token ?? env.token ?? file?.token;
  if (token === undefined) {
    throw new ConfigError(missingMessage("token", ENV.TOKEN, configPath));
  }
  const server = flags.server ?? env.server ?? file?.server;
  if (server === undefined) {
    throw new ConfigError(missingMessage("server", ENV.SERVER, configPath));
  }

  const host = flags.host ?? env.host ?? file?.host ?? DEFAULTS.HOST;
  const logLevel =
    flags.logLevel ?? env.logLevel ?? file?.logLevel ?? DEFAULTS.LOG_LEVEL;

  return {
    token,
    server,
    host,
    logLevel,
    insecure: flags.insecure ?? false,
    upstreamInsecure: flags.upstreamInsecure ?? false,
  };
}

function missingMessage(
  field: "token" | "server",
  envVar: string,
  configPath: string,
): string {
  return (
    `missing ${field}: provide --${field}, set ${envVar}, ` +
    `or add "${field}" to ${configPath}`
  );
}

/** Compute the config file path, honouring XDG_CONFIG_HOME then HOME. */
export function configFilePath(
  env: Record<string, string | undefined>,
): string {
  const xdg = nonEmpty(env[ENV.XDG_CONFIG_HOME]);
  const home = nonEmpty(env[ENV.HOME]);
  const base =
    xdg ??
    (home !== undefined
      ? join(home, XDG_CONFIG_FALLBACK)
      : XDG_CONFIG_FALLBACK);
  return join(base, CONFIG_DIR_NAME, CONFIG_FILE_NAME);
}

/** Validate parsed config-file JSON into a PartialConfig. Throws on bad shape. */
export function parseConfigFile(text: string, path: string): PartialConfig {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (err) {
    throw new ConfigError(
      `invalid JSON in ${path}: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
  if (!isRecord(parsed)) {
    throw new ConfigError(`invalid config in ${path}: expected a JSON object`);
  }
  const out: PartialConfig = {};
  const strField = (key: "token" | "server" | "host"): void => {
    const value = parsed[key];
    if (value === undefined) {
      return;
    }
    if (typeof value !== "string") {
      throw new ConfigError(`invalid "${key}" in ${path}: expected a string`);
    }
    if (value !== "") {
      out[key] = value;
    }
  };
  strField("token");
  strField("server");
  strField("host");

  const level = parsed.logLevel;
  if (level !== undefined) {
    if (typeof level !== "string" || !isLogLevel(level)) {
      throw new ConfigError(
        `invalid "logLevel" in ${path}: expected one of ${LOG_LEVELS.join(", ")}`,
      );
    }
    out.logLevel = level;
  }
  return out;
}

/** Load and parse the config file, or return null if it does not exist. */
export function loadConfigFile(
  env: Record<string, string | undefined>,
): PartialConfig | null {
  const path = configFilePath(env);
  if (!existsSync(path)) {
    return null;
  }
  return parseConfigFile(readFileSync(path, "utf8"), path);
}

/**
 * Merge `updates` into the config file, preserving any keys already present
 * (including ones this version does not know about), and return the path plus
 * the keys written. The file holds a secret token, so the directory is created
 * 0700 and the file forced to 0600 even if it already existed.
 */
export function writeConfigValues(
  env: Record<string, string | undefined>,
  updates: PartialConfig,
): { path: string; keys: string[] } {
  const path = configFilePath(env);

  let current: Record<string, unknown> = {};
  if (existsSync(path)) {
    const text = readFileSync(path, "utf8");
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch (err) {
      throw new ConfigError(
        `invalid JSON in ${path}: ${err instanceof Error ? err.message : String(err)}`,
      );
    }
    if (!isRecord(parsed)) {
      throw new ConfigError(
        `invalid config in ${path}: expected a JSON object`,
      );
    }
    current = parsed;
  }

  const merged = { ...current, ...updates };
  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  writeFileSync(path, `${JSON.stringify(merged, null, 2)}\n`, { mode: 0o600 });
  // writeFileSync only applies mode on creation; enforce it for an existing
  // file that may have been created with looser permissions.
  chmodSync(path, 0o600);

  return { path, keys: Object.keys(updates) };
}
