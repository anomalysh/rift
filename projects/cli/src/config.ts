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

import { randomBytes } from "node:crypto";
import {
  chmodSync,
  closeSync,
  existsSync,
  fsyncSync,
  mkdirSync,
  openSync,
  readFileSync,
  renameSync,
  statSync,
  unlinkSync,
  writeSync,
} from "node:fs";
import { basename, dirname, join } from "node:path";
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
  /** Dial a non-loopback gateway over cleartext ws:// (explicit opt-in). */
  readonly allowInsecureTransport: boolean;
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

/** Truthy spellings accepted for boolean environment variables. */
function envFlag(value: string | undefined): boolean {
  return (
    value !== undefined && ["1", "true", "yes"].includes(value.toLowerCase())
  );
}

/**
 * Whether a host name or address refers to this machine: `localhost` (and its
 * `*.localhost` subdomains, RFC 6761), 127.0.0.0/8, or ::1. Accepts IPv6 with
 * or without the URL brackets.
 */
export function isLoopbackHost(host: string): boolean {
  const h = host.toLowerCase().replace(/^\[(.*)\]$/, "$1");
  if (h === "localhost" || h.endsWith(".localhost")) {
    return true;
  }
  if (h === "::1" || h === "0:0:0:0:0:0:0:1") {
    return true;
  }
  return /^127(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}$/.test(h);
}

/**
 * Why the gateway URL must not be dialed, or null if it may. The token travels
 * in the first frame, so a cleartext `ws://` connection hands it to anyone on
 * the path. Plain ws:// is therefore only accepted for a loopback gateway (a
 * local dev stack, an SSH port-forward) or with the explicit opt-in.
 */
export function gatewayTransportProblem(
  server: string,
  allowInsecureTransport: boolean,
): string | null {
  let url: URL;
  try {
    url = new URL(server);
  } catch {
    return `invalid server URL ${JSON.stringify(server)}: expected wss://host/path`;
  }
  if (url.protocol === "wss:") {
    return null;
  }
  if (url.protocol !== "ws:") {
    return `invalid server URL ${JSON.stringify(server)}: the scheme must be wss:// (or ws:// for a loopback gateway)`;
  }
  if (isLoopbackHost(url.hostname) || allowInsecureTransport) {
    return null;
  }
  return (
    `refusing to send the token in cleartext to ${url.host}: ${JSON.stringify(server)} is ws://, not wss://. ` +
    `Use a wss:// URL, or pass --allow-insecure-transport (or set ${ENV.ALLOW_INSECURE_TRANSPORT}=1) ` +
    "if this network path is trusted"
  );
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
    allowInsecureTransport:
      flags.allowInsecureTransport === true ||
      envFlag(input.env[ENV.ALLOW_INSECURE_TRANSPORT]),
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

/** Where non-fatal configuration warnings go; stderr unless a test injects. */
export type WarnFn = (message: string) => void;

const stderrWarn: WarnFn = (message) => {
  process.stderr.write(`rift: warning: ${message}\n`);
};

/**
 * Warn if a file holding the token is readable or writable by anyone but its
 * owner. Only meaningful where POSIX permission bits are (not on Windows).
 */
export function warnIfExposed(path: string, warn: WarnFn = stderrWarn): void {
  if (process.platform === "win32") {
    return;
  }
  let mode: number;
  try {
    mode = statSync(path).mode;
  } catch {
    return;
  }
  if ((mode & 0o077) !== 0) {
    const perms = (mode & 0o777).toString(8).padStart(3, "0");
    warn(
      `${path} holds your token but is accessible to other users (mode ${perms}); run: chmod 600 ${path}`,
    );
  }
}

/** Load and parse the config file, or return null if it does not exist. */
export function loadConfigFile(
  env: Record<string, string | undefined>,
  warn: WarnFn = stderrWarn,
): PartialConfig | null {
  const path = configFilePath(env);
  if (!existsSync(path)) {
    return null;
  }
  const parsed = parseConfigFile(readFileSync(path, "utf8"), path);
  if (parsed.token !== undefined) {
    warnIfExposed(path, warn);
  }
  return parsed;
}

/**
 * Read a token from a file (`--token-file`), trimming surrounding whitespace so
 * a trailing newline from `echo` or an editor is not part of the secret.
 */
export function readTokenFile(path: string, warn: WarnFn = stderrWarn): string {
  let text: string;
  try {
    text = readFileSync(path, "utf8");
  } catch (err) {
    throw new ConfigError(
      `cannot read --token-file ${path}: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
  const token = text.trim();
  if (token === "") {
    throw new ConfigError(`--token-file ${path} is empty`);
  }
  warnIfExposed(path, warn);
  return token;
}

/**
 * Merge `updates` into the config file, preserving any keys already present
 * (including ones this version does not know about), and return the path plus
 * the keys written. The file holds a secret token, so the rift directory is
 * kept 0700 and the new contents are written to a 0600 temporary file in the
 * same directory and renamed over the old one: the token is never briefly
 * readable under looser permissions, and a crash mid-write cannot leave a
 * truncated config behind.
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
  const dir = dirname(path);
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  // mkdir's mode is masked by the umask and ignored for an existing directory;
  // enforce it on the rift directory itself (never on its parents).
  if (process.platform !== "win32") {
    chmodSync(dir, 0o700);
  }
  const tmp = join(
    dir,
    `.${basename(path)}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`,
  );
  // "wx": fail rather than follow a pre-planted file or symlink at that name.
  const fd = openSync(tmp, "wx", 0o600);
  try {
    writeSync(fd, `${JSON.stringify(merged, null, 2)}\n`);
    fsyncSync(fd);
    closeSync(fd);
    renameSync(tmp, path);
  } catch (err) {
    try {
      closeSync(fd);
    } catch {
      // Already closed.
    }
    try {
      unlinkSync(tmp);
    } catch {
      // Never created or already renamed.
    }
    throw err;
  }

  return { path, keys: Object.keys(updates) };
}
