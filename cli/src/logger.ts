// Leveled logger with two faces:
//
//   - a live TUI dashboard when stdout is an interactive terminal, and
//   - plain, greppable one-line-per-event text otherwise.
//
// Diagnostics go to stderr and the tunnel banner to stdout, so `rift ... | jq`
// pipelines stay clean. When stdout is NOT a TTY, or colour is disabled
// (NO_COLOR / RIFT_NO_COLOR), or the level is `debug` (diagnostics want plain
// verbose output) or `silent`, we never emit cursor control or colour — the
// plain logger below is byte-for-byte greppable. The TUI face only activates
// for an interactive `info`/`warn`/`error` session.

import { ENV, LOG_LEVELS, type LogLevel } from "./constants.ts";
import {
  type ConnStatus,
  createStyle,
  Dashboard,
  type DashboardDeps,
  formatEvent,
  formatPlainBanner,
  type Metrics,
  type SessionInfo,
} from "./ui.ts";

const LEVEL_RANK: Record<LogLevel, number> = {
  debug: 10,
  info: 20,
  warn: 30,
  error: 40,
  silent: 100,
};

/** Default terminal size when stdout does not report it. */
const DEFAULT_COLUMNS = 80;
const DEFAULT_ROWS = 24;

export interface Logger {
  debug(message: string, ...rest: unknown[]): void;
  info(message: string, ...rest: unknown[]): void;
  warn(message: string, ...rest: unknown[]): void;
  error(message: string, ...rest: unknown[]): void;
  /** Print the tunnel URL banner to stdout, regardless of log level. */
  banner(text: string): void;
  // ---- Optional TUI surface. Both built-in loggers implement these; they are
  // optional in the type only so external Logger implementers stay valid. ----
  /** Announce an established tunnel (draws the panel / prints the banner). */
  session?(info: SessionInfo): void;
  /** Update the live connection state (spinner + label). No-op when plain. */
  status?(status: ConnStatus, detail?: string): void;
  /** Register a source the dashboard polls for the live request counter. */
  metrics?(source: () => Metrics): void;
  /** Tear the dashboard down and restore the terminal. No-op when plain. */
  close?(): void;
}

export function isLogLevel(v: string): v is LogLevel {
  return LOG_LEVELS.some((level) => level === v);
}

function format(level: LogLevel, message: string, rest: unknown[]): string {
  const ts = new Date().toISOString();
  const tag = level.toUpperCase().padEnd(5);
  const extra = rest.length > 0 ? ` ${rest.map(render).join(" ")}` : "";
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

/** Merge a message and its rest args into one line for the TUI scrollback. */
function joinMessage(message: string, rest: unknown[]): string {
  return rest.length > 0 ? `${message} ${rest.map(render).join(" ")}` : message;
}

/**
 * Decide whether the interactive dashboard may run. It requires a real TTY on
 * stdout, colour permission, and a level that is neither `debug` (wants plain
 * verbose diagnostics) nor `silent` (wants nothing at all).
 */
function shouldUseTui(level: LogLevel): boolean {
  const isTty = process.stdout.isTTY === true;
  // The NO_COLOR convention: any presence disables colour, regardless of value.
  const noColor =
    process.env[ENV.NO_COLOR] !== undefined ||
    process.env[ENV.RIFT_NO_COLOR] !== undefined;
  return isTty && !noColor && level !== "debug" && level !== "silent";
}

/** Plain, colour-free logger: one line per event, greppable, pipeline-safe. */
function createPlainLogger(level: LogLevel): Logger {
  const threshold = LEVEL_RANK[level];
  const emit = (lvl: LogLevel, message: string, rest: unknown[]): void => {
    if (LEVEL_RANK[lvl] >= threshold) {
      process.stderr.write(format(lvl, message, rest));
    }
  };
  // `silent` suppresses stdout too; every other level prints the banner.
  const stdoutAllowed = level !== "silent";
  const writeStdout = (text: string): void => {
    if (stdoutAllowed) {
      process.stdout.write(text.endsWith("\n") ? text : `${text}\n`);
    }
  };
  return {
    debug: (m, ...r) => emit("debug", m, r),
    info: (m, ...r) => emit("info", m, r),
    warn: (m, ...r) => emit("warn", m, r),
    error: (m, ...r) => emit("error", m, r),
    banner: (text) => writeStdout(text),
    session: (info) => writeStdout(formatPlainBanner(info)),
    // Live state, counters, and teardown have no meaning in plain mode.
    status: () => {},
    metrics: () => {},
    close: () => {},
  };
}

/** Production I/O bindings for the dashboard, drawn to the interactive stdout. */
function dashboardDeps(): DashboardDeps {
  return {
    write: (chunk) => process.stdout.write(chunk),
    columns: () => process.stdout.columns ?? DEFAULT_COLUMNS,
    rows: () => process.stdout.rows ?? DEFAULT_ROWS,
    style: createStyle(true),
    now: () => Date.now(),
    setInterval: (fn, ms) => {
      const handle = setInterval(fn, ms);
      // Do not let the redraw ticker keep the event loop alive on its own.
      handle.unref?.();
      return { cancel: () => clearInterval(handle) };
    },
    onExit: (fn) => {
      process.on("exit", fn);
    },
    offExit: (fn) => {
      process.removeListener("exit", fn);
    },
  };
}

/** Interactive logger: colourised scrollback above a sticky, live status panel. */
function createTuiLogger(level: LogLevel): Logger {
  const threshold = LEVEL_RANK[level];
  const deps = dashboardDeps();
  const style = deps.style;
  const dashboard = new Dashboard(deps);
  dashboard.start();

  const emit = (
    lvl: "info" | "warn" | "error",
    message: string,
    rest: unknown[],
  ): void => {
    if (LEVEL_RANK[lvl] < threshold) {
      return;
    }
    dashboard.event(
      formatEvent(lvl, joinMessage(message, rest), style, Date.now()),
    );
  };

  return {
    // `debug` is unreachable here (TUI never runs at debug level), but the
    // method must exist; keep it a no-op so a stray call cannot corrupt the
    // panel's cursor accounting.
    debug: () => {},
    info: (m, ...r) => emit("info", m, r),
    warn: (m, ...r) => emit("warn", m, r),
    error: (m, ...r) => emit("error", m, r),
    // The banner is superseded by the panel; surface any stray call as an event
    // rather than writing raw text that would desync the sticky redraw.
    banner: (text) => {
      const trimmed = text.trim();
      if (trimmed !== "") {
        dashboard.event(trimmed);
      }
    },
    session: (info) => dashboard.setSession(info),
    status: (status, detail) => dashboard.setStatus(status, detail),
    metrics: (source) => dashboard.setMetrics(source),
    close: () => dashboard.close("offline"),
  };
}

export function createLogger(level: LogLevel): Logger {
  return shouldUseTui(level)
    ? createTuiLogger(level)
    : createPlainLogger(level);
}
