// Leveled logger. Diagnostics go to stderr so that stdout carries only the
// tunnel URL banner, keeping `rift ... | something` pipelines clean.

import { LOG_LEVELS, type LogLevel } from "./constants.ts";

const LEVEL_RANK: Record<LogLevel, number> = {
  debug: 10,
  info: 20,
  warn: 30,
  error: 40,
  silent: 100,
};

export interface Logger {
  debug(message: string, ...rest: unknown[]): void;
  info(message: string, ...rest: unknown[]): void;
  warn(message: string, ...rest: unknown[]): void;
  error(message: string, ...rest: unknown[]): void;
  /** Print the tunnel URL banner to stdout, regardless of log level. */
  banner(text: string): void;
}

export function isLogLevel(v: string): v is LogLevel {
  return LOG_LEVELS.some((level) => level === v);
}

function format(level: LogLevel, message: string, rest: unknown[]): string {
  const ts = new Date().toISOString();
  const tag = level.toUpperCase().padEnd(5);
  const extra = rest.length > 0 ? " " + rest.map(render).join(" ") : "";
  return `${ts} ${tag} ${message}${extra}\n`;
}

function render(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  if (value instanceof Error) {
    return value.stack ?? value.message;
  }
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

export function createLogger(level: LogLevel): Logger {
  const threshold = LEVEL_RANK[level];
  const emit = (lvl: LogLevel, message: string, rest: unknown[]): void => {
    if (LEVEL_RANK[lvl] >= threshold) {
      process.stderr.write(format(lvl, message, rest));
    }
  };
  return {
    debug: (m, ...r) => emit("debug", m, r),
    info: (m, ...r) => emit("info", m, r),
    warn: (m, ...r) => emit("warn", m, r),
    error: (m, ...r) => emit("error", m, r),
    banner: (text) => process.stdout.write(text.endsWith("\n") ? text : text + "\n"),
  };
}
