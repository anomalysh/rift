// Terminal UI toolkit for the interactive `rift` dashboard.
//
// This module is deliberately dependency-free: every colour, box-drawing, and
// layout primitive is a hand-written ANSI/escape helper so the agent stays a
// single self-contained Bun binary. Nothing here reads process state or emits
// output on its own; the imperative shell (`Dashboard`) is the only stateful
// piece and it takes all of its I/O through injected callbacks so the pure
// formatting helpers below can be unit-tested without a TTY.
//
// Two audiences consume this file:
//   - logger.ts, which wires the palette + `Dashboard` into the TUI logger and
//     falls back to plain text (this module untouched) when stdout is not a TTY.
//   - client.ts, which builds a `SessionInfo` to hand to `logger.session`.

// ---------------------------------------------------------------------------
// ANSI SGR palette.
// ---------------------------------------------------------------------------

/** Select Graphic Rendition escape codes; content-only, never cursor control. */
const SGR = {
  reset: "\x1b[0m",
  bold: "\x1b[1m",
  dim: "\x1b[2m",
  green: "\x1b[32m",
  yellow: "\x1b[33m",
  red: "\x1b[31m",
  cyan: "\x1b[36m",
  magenta: "\x1b[35m",
  gray: "\x1b[90m",
} as const;

/**
 * A colour palette. When `enabled` is false every method is the identity
 * function, so the same rendering code produces plain, escape-free text for
 * non-colour contexts (this is how renderPanel stays testable).
 */
export interface Style {
  readonly enabled: boolean;
  bold(s: string): string;
  dim(s: string): string;
  green(s: string): string;
  yellow(s: string): string;
  red(s: string): string;
  cyan(s: string): string;
  magenta(s: string): string;
  gray(s: string): string;
}

/** Build a palette. `enabled: false` yields an identity (plain-text) palette. */
export function createStyle(enabled: boolean): Style {
  const wrap =
    (code: string) =>
    (s: string): string =>
      enabled ? `${code}${s}${SGR.reset}` : s;
  return {
    enabled,
    bold: wrap(SGR.bold),
    dim: wrap(SGR.dim),
    green: wrap(SGR.green),
    yellow: wrap(SGR.yellow),
    red: wrap(SGR.red),
    cyan: wrap(SGR.cyan),
    magenta: wrap(SGR.magenta),
    gray: wrap(SGR.gray),
  };
}

// ---------------------------------------------------------------------------
// Visible-width helpers. All box layout measures printed columns, not string
// length, so SGR sequences never throw off alignment or truncation.
// ---------------------------------------------------------------------------

// Only SGR sequences (ending in `m`) ever appear inside content strings; cursor
// control is emitted by the Dashboard separately and never measured here.
// biome-ignore lint/suspicious/noControlCharactersInRegex: matching the ANSI ESC (0x1b) is the whole point of stripping SGR codes.
const ANSI_SGR = /\x1b\[[0-9;]*m/g;

/** Strip SGR colour codes, leaving the printable text. */
export function stripAnsi(s: string): string {
  return s.replace(ANSI_SGR, "");
}

/**
 * Printed column count of a string, ignoring colour codes. Every glyph this UI
 * uses (box-drawing, braille spinner, arrows) is single-width, so code-unit
 * length of the stripped string is an accurate column count.
 */
export function visibleWidth(s: string): number {
  return stripAnsi(s).length;
}

/**
 * Truncate to at most `max` printed columns, appending an ellipsis. Escape
 * sequences are copied through without counting toward the width and are never
 * cut mid-sequence; a reset is appended only if the input actually carried
 * colour, so plain text truncates to plain text.
 */
export function truncateVisible(s: string, max: number): string {
  if (max <= 0) {
    return "";
  }
  if (visibleWidth(s) <= max) {
    return s;
  }
  const limit = max - 1; // reserve a column for the ellipsis
  let out = "";
  let count = 0;
  let i = 0;
  while (i < s.length) {
    if (s[i] === "\x1b") {
      const start = i;
      i++;
      if (s[i] === "[") {
        i++;
        while (i < s.length && !/[a-zA-Z]/.test(s[i] ?? "")) {
          i++;
        }
        if (i < s.length) {
          i++; // include the terminating letter
        }
      }
      out += s.slice(start, i);
      continue;
    }
    if (count >= limit) {
      break;
    }
    out += s[i];
    count++;
    i++;
  }
  const hadColor = ANSI_SGR.test(s);
  ANSI_SGR.lastIndex = 0; // `test` on a /g regex is stateful; reset it
  return `${out}…${hadColor ? SGR.reset : ""}`;
}

/** Right-pad with spaces to `width` printed columns, truncating if too long. */
export function padEndVisible(s: string, width: number): string {
  const w = visibleWidth(s);
  if (w === width) {
    return s;
  }
  if (w < width) {
    return s + " ".repeat(width - w);
  }
  return truncateVisible(s, width);
}

/**
 * Lay `left` and `right` on one line of `width` columns, flush to each edge,
 * with at least one space between them. `left` is truncated first if the two
 * cannot both fit.
 */
export function justify(left: string, right: string, width: number): string {
  const rw = visibleWidth(right);
  let l = left;
  if (visibleWidth(l) + rw + 1 > width) {
    l = truncateVisible(l, Math.max(0, width - rw - 1));
  }
  const gap = Math.max(1, width - visibleWidth(l) - rw);
  return l + " ".repeat(gap) + right;
}

// ---------------------------------------------------------------------------
// Duration formatting.
// ---------------------------------------------------------------------------

const MS_PER_SECOND = 1000;
const SECONDS_PER_MINUTE = 60;
const SECONDS_PER_HOUR = 3600;

function pad2(n: number): string {
  return n < 10 ? `0${n}` : String(n);
}

/** Humanise a millisecond duration: `0s`, `45s`, `1m 05s`, `2h 09m`. */
export function formatDuration(ms: number): string {
  const total = Math.max(0, Math.floor(ms / MS_PER_SECOND));
  if (total < SECONDS_PER_MINUTE) {
    return `${total}s`;
  }
  if (total < SECONDS_PER_HOUR) {
    const m = Math.floor(total / SECONDS_PER_MINUTE);
    const s = total % SECONDS_PER_MINUTE;
    return `${m}m ${pad2(s)}s`;
  }
  const h = Math.floor(total / SECONDS_PER_HOUR);
  const m = Math.floor((total % SECONDS_PER_HOUR) / SECONDS_PER_MINUTE);
  return `${h}h ${pad2(m)}m`;
}

/**
 * Format a short backoff delay for the reconnecting status: sub-second delays
 * read as `820ms`, longer ones as `1.5s`. Kept distinct from formatDuration,
 * which is second-granular and would collapse these to "0s".
 */
export function formatRetryDelay(ms: number): string {
  const clamped = Math.max(0, Math.round(ms));
  if (clamped < MS_PER_SECOND) {
    return `${clamped}ms`;
  }
  return `${(clamped / MS_PER_SECOND).toFixed(1)}s`;
}

// ---------------------------------------------------------------------------
// Session model shared with client.ts and logger.ts.
// ---------------------------------------------------------------------------

/** The live connection state a dashboard reflects. */
export type ConnStatus =
  | "connecting"
  | "online"
  | "reconnecting"
  | "closing"
  | "offline";

/** Everything the banner/panel needs to describe an established tunnel. */
export interface SessionInfo {
  /** Agent version, e.g. "0.1.0". */
  readonly version: string;
  /** Public tunnel URL, e.g. "https://myapp.rift.anomaly.sh". */
  readonly url: string;
  /** Local forwarding target, e.g. "http://127.0.0.1:3000". */
  readonly forwardTo: string;
  /** Gateway host serving the tunnel, e.g. "rift.anomaly.sh". */
  readonly gateway: string;
  /** Gateway-assigned tunnel id (shown in the plain banner). */
  readonly tunnelId: string;
}

/** Live request tallies polled by the dashboard. */
export interface Metrics {
  /** Requests seen since the agent started. */
  readonly total: number;
  /** Requests currently in flight. */
  readonly open: number;
}

// ---------------------------------------------------------------------------
// Panel rendering (pure).
// ---------------------------------------------------------------------------

/** Single-line box-drawing characters. */
export const BOX = {
  topLeft: "┌",
  topRight: "┐",
  bottomLeft: "└",
  bottomRight: "┘",
  horizontal: "─",
  vertical: "│",
} as const;

/** Braille spinner frames for transitional (connecting/reconnecting) states. */
export const SPINNER_FRAMES = [
  "⠋",
  "⠙",
  "⠹",
  "⠸",
  "⠼",
  "⠴",
  "⠦",
  "⠧",
  "⠇",
  "⠏",
] as const;

/** Column reserved for the label in `Label   value` rows. */
const LABEL_WIDTH = 12;
/** Panel columns: never wider than this, and never wider than the terminal. */
const PANEL_MAX_WIDTH = 72;
/** Floor so a very narrow terminal still yields a usable inner width. */
const PANEL_MIN_WIDTH = 24;
/** Interior padding (`│ ` … ` │`) subtracted from the panel width. */
const FRAME_PADDING = 4;

/** Clamp a terminal width into the panel's drawable range.
 *
 * The panel is kept strictly narrower than the terminal: a line that fills the
 * final column phantom-wraps on many terminals, which makes the redraw's
 * logical line count smaller than the rows actually on screen, so the clear
 * scrolls up too few rows and the next event lands on the panel. Reserving one
 * trailing column avoids that; the redraw's no-trailing-newline discipline
 * handles the separate bottom-of-screen scroll case. */
export function clampWidth(columns: number): number {
  return Math.max(PANEL_MIN_WIDTH, Math.min(PANEL_MAX_WIDTH, columns - 1));
}

interface StatusMeta {
  readonly label: string;
  readonly glyph: "spinner" | "on" | "off";
  paint(style: Style, text: string): string;
}

function statusMeta(status: ConnStatus): StatusMeta {
  switch (status) {
    case "connecting":
      return {
        label: "connecting",
        glyph: "spinner",
        paint: (s, t) => s.yellow(t),
      };
    case "online":
      return { label: "online", glyph: "on", paint: (s, t) => s.green(t) };
    case "reconnecting":
      return {
        label: "reconnecting",
        glyph: "spinner",
        paint: (s, t) => s.yellow(t),
      };
    case "closing":
      return { label: "closing", glyph: "spinner", paint: (s, t) => s.dim(t) };
    case "offline":
      return { label: "offline", glyph: "off", paint: (s, t) => s.red(t) };
  }
}

/** Immutable snapshot handed to `renderPanel`. */
export interface PanelState {
  readonly session: SessionInfo | null;
  readonly status: ConnStatus;
  /** Short suffix beside the status (e.g. "retry in 2s"). */
  readonly detail: string;
  readonly uptimeMs: number;
  readonly metrics: Metrics | null;
  /** The current spinner frame; used only for transitional states. */
  readonly spinnerFrame: string;
}

function border(width: number, left: string, right: string): string {
  return left + BOX.horizontal.repeat(Math.max(0, width - 2)) + right;
}

/** Wrap one line of interior `content` in vertical borders, padded to `inner`. */
function frameRow(content: string, inner: number): string {
  return `${BOX.vertical} ${padEndVisible(content, inner)} ${BOX.vertical}`;
}

function fieldRow(style: Style, label: string, value: string): string {
  const head = label === "" ? "" : style.gray(label);
  return padEndVisible(head, LABEL_WIDTH) + value;
}

/**
 * Render the full dashboard panel as an array of screen rows (no trailing
 * newlines, each already padded/truncated to `width`). Pure: identical inputs
 * always yield identical output, which is what makes it unit-testable.
 */
export function renderPanel(
  state: PanelState,
  style: Style,
  width: number,
): string[] {
  const inner = Math.max(0, width - FRAME_PADDING);
  const row = (content: string): string => frameRow(content, inner);
  const lines: string[] = [];

  lines.push(border(width, BOX.topLeft, BOX.topRight));

  const meta = statusMeta(state.status);
  const glyph =
    meta.glyph === "on" ? "●" : meta.glyph === "off" ? "○" : state.spinnerFrame;
  const statusCell =
    meta.paint(style, `${glyph} ${meta.label}`) +
    (state.detail !== "" ? ` ${style.dim(state.detail)}` : "");
  const brand =
    style.bold("rift") +
    (state.session !== null ? ` ${style.dim(state.session.version)}` : "");
  lines.push(row(justify(brand, statusCell, inner)));
  lines.push(row(""));

  // Always emit the same rows so the panel height is constant. The header is a
  // fixed region at the top of the screen (see Dashboard); a changing height
  // would move the scroll region and disturb the log below it.
  if (state.session !== null) {
    lines.push(
      row(fieldRow(style, "Forwarding", style.cyan(state.session.url))),
    );
    lines.push(
      row(fieldRow(style, "", style.dim("→ ") + state.session.forwardTo)),
    );
    lines.push(row(fieldRow(style, "Gateway", state.session.gateway)));
  } else {
    lines.push(
      row(fieldRow(style, "Forwarding", style.dim("establishing tunnel…"))),
    );
    lines.push(row(""));
    lines.push(row(""));
  }

  lines.push(row(fieldRow(style, "Uptime", formatDuration(state.uptimeMs))));

  const requests =
    state.metrics !== null
      ? `${style.bold(String(state.metrics.total))} total ` +
        `${style.dim("·")} ${style.bold(String(state.metrics.open))} open`
      : style.dim("0 total · 0 open");
  lines.push(row(fieldRow(style, "Requests", requests)));

  lines.push(row(""));
  lines.push(row(style.dim("Ctrl-C to quit")));
  lines.push(border(width, BOX.bottomLeft, BOX.bottomRight));
  return lines;
}

/** Fixed number of rows renderPanel always emits (the header region height). */
export const PANEL_HEIGHT = 11;

// ---------------------------------------------------------------------------
// Event line + plain banner formatting (pure).
// ---------------------------------------------------------------------------

/** Level of a scrollback event line above the sticky panel. */
export type EventLevel = "info" | "warn" | "error";

function clockTime(now: number): string {
  const d = new Date(now);
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}

/**
 * Format one colourised event line for the TUI scrollback: a dim timestamp, a
 * level glyph, and the message (errors and warnings tinted for scanning).
 */
export function formatEvent(
  level: EventLevel,
  message: string,
  style: Style,
  now: number,
): string {
  const time = style.dim(clockTime(now));
  switch (level) {
    case "info":
      return `${time}  ${style.cyan("•")}  ${message}`;
    case "warn":
      return `${time}  ${style.yellow("!")}  ${style.yellow(message)}`;
    case "error":
      return `${time}  ${style.red("✗")}  ${style.red(message)}`;
  }
}

/**
 * A plain, colour-free, greppable banner for non-TTY output. This is what a
 * piped or CI invocation prints to stdout in place of the live panel.
 */
export function formatPlainBanner(info: SessionInfo): string {
  return [
    "",
    `  rift ${info.version}`,
    `  forwarding  ${info.url}`,
    `          ->  ${info.forwardTo}`,
    `  gateway     ${info.gateway}`,
    `  tunnel      ${info.tunnelId}`,
    "",
  ].join("\n");
}

// ---------------------------------------------------------------------------
// Dashboard: the imperative shell that owns cursor control and the redraw loop.
// All I/O flows through injected callbacks; construction has no side effects.
// ---------------------------------------------------------------------------

/** Cursor, screen, and scroll-region control (never measured as content). */
const HIDE_CURSOR = "\x1b[?25l";
const SHOW_CURSOR = "\x1b[?25h";
const SAVE_CURSOR = "\x1b7"; // DECSC: save cursor position
const RESTORE_CURSOR = "\x1b8"; // DECRC: restore cursor position
const CLEAR_SCREEN = "\x1b[2J\x1b[H";
const CLEAR_LINE = "\x1b[2K";
const RESET_SCROLL_REGION = "\x1b[r";
const moveTo = (row: number, col: number): string => `\x1b[${row};${col}H`;
const setScrollRegion = (top: number, bottom: number): string =>
  `\x1b[${top};${bottom}r`;

/** Spinner cadence; signature-gated header repaints keep idle states near 1 Hz. */
const TICK_INTERVAL_MS = 120;
/** Terminal height to assume when stdout does not report one. */
const FALLBACK_ROWS = 24;

/** Injected environment for the Dashboard, so it is TTY- and clock-agnostic. */
export interface DashboardDeps {
  /** Emit a string to the terminal stream (stdout in production). */
  write(chunk: string): void;
  /** Current terminal width in columns. */
  columns(): number;
  /** Current terminal height in rows. */
  rows(): number;
  /** Enabled colour palette. */
  readonly style: Style;
  /** Clock source (`Date.now` in production; deterministic in tests). */
  now(): number;
  /** Schedule the redraw tick; returns a cancel handle. */
  setInterval(fn: () => void, ms: number): { cancel(): void };
  /** Register/deregister a best-effort terminal-restore on process exit. */
  onExit(fn: () => void): void;
  offExit(fn: () => void): void;
}

/**
 * An ngrok-style dashboard: the status panel is a fixed header pinned to the top
 * of the screen with a DECSTBM scroll region, while the event/request log scrolls
 * in the region below it. The header height is constant (PANEL_HEIGHT) so the
 * region never moves; the header repaints in place around a saved cursor, leaving
 * the log untouched. A terminal too short for the header degrades to a one-shot
 * banner followed by plain scrolling logs.
 */
export class Dashboard {
  private session: SessionInfo | null = null;
  private status: ConnStatus = "connecting";
  private detail = "";
  private metricsSource: (() => Metrics) | null = null;
  private frame = 0;
  private lastSignature = "";
  private cursorHidden = false;
  private stopped = false;
  /** True once the fixed-header scroll region is set up (not degraded/stopped). */
  private active = false;
  private termRows = FALLBACK_ROWS;
  private ticker: { cancel(): void } | null = null;
  private readonly startedAt: number;
  private readonly exitHandler = (): void => this.teardown();

  constructor(private readonly deps: DashboardDeps) {
    this.startedAt = deps.now();
  }

  /** Clear the screen, pin the header, and start scrolling the log below it. */
  start(): void {
    this.termRows = Math.max(1, this.deps.rows() || FALLBACK_ROWS);
    // A terminal too short for the header plus a line of log cannot host the
    // fixed layout; degrade to a one-shot banner and plain logs.
    if (this.termRows <= PANEL_HEIGHT + 1) {
      this.deps.write(
        `${renderPanel(this.snapshot(), this.deps.style, this.headerWidth()).join("\n")}\n`,
      );
      return;
    }
    this.cursorHidden = true;
    this.active = true;
    this.deps.onExit(this.exitHandler);
    // Clear, carve out the scroll region below the header, and park the cursor at
    // the top of that region so the first log line lands just under the panel.
    this.deps.write(
      HIDE_CURSOR +
        CLEAR_SCREEN +
        setScrollRegion(PANEL_HEIGHT + 1, this.termRows) +
        moveTo(PANEL_HEIGHT + 1, 1),
    );
    this.paintHeader(true);
    this.ticker = this.deps.setInterval(() => {
      this.frame++;
      this.paintHeader(false);
    }, TICK_INTERVAL_MS);
  }

  setSession(info: SessionInfo): void {
    this.session = info;
    this.paintHeader(true);
  }

  setStatus(status: ConnStatus, detail?: string): void {
    this.status = status;
    this.detail = detail ?? "";
    this.paintHeader(true);
  }

  setMetrics(source: () => Metrics): void {
    this.metricsSource = source;
  }

  /** Append a line to the scrolling log below the fixed header. The cursor lives
   *  in the scroll region, so writing here scrolls the log and never touches the
   *  header above it. */
  event(line: string): void {
    this.deps.write(line.endsWith("\n") ? line : `${line}\n`);
  }

  /** Freeze the header in a final state and release the terminal. */
  close(final: ConnStatus, detail?: string): void {
    if (this.stopped) {
      return;
    }
    this.stopped = true;
    this.ticker?.cancel();
    this.ticker = null;
    this.status = final;
    if (detail !== undefined) {
      this.detail = detail;
    }
    this.paintHeader(true);
    this.teardown();
  }

  /** Reset the scroll region, move below the header, and restore the cursor.
   *  Idempotent, so it is safe as both the close path and the on-exit handler. */
  private teardown(): void {
    if (!this.active) {
      if (this.cursorHidden) {
        this.deps.write(SHOW_CURSOR);
        this.cursorHidden = false;
      }
      return;
    }
    this.active = false;
    let out = `${RESET_SCROLL_REGION}${moveTo(this.termRows, 1)}\n`;
    if (this.cursorHidden) {
      out += SHOW_CURSOR;
      this.cursorHidden = false;
    }
    this.deps.write(out);
    this.deps.offExit(this.exitHandler);
  }

  private headerWidth(): number {
    return clampWidth(this.deps.columns());
  }

  private snapshot(): PanelState {
    const metrics = this.metricsSource !== null ? this.metricsSource() : null;
    const spinnerFrame =
      SPINNER_FRAMES[this.frame % SPINNER_FRAMES.length] ?? SPINNER_FRAMES[0];
    return {
      session: this.session,
      status: this.status,
      detail: this.detail,
      uptimeMs: this.deps.now() - this.startedAt,
      metrics,
      spinnerFrame,
    };
  }

  /** Repaint the fixed header at the top of the screen without disturbing the log
   *  cursor in the region below: save the cursor, position+clear each header row,
   *  restore the cursor. A terminal resize re-issues the scroll region first. */
  private paintHeader(force: boolean): void {
    if (!this.active) {
      return;
    }
    const width = this.headerWidth();
    const rows = Math.max(1, this.deps.rows() || FALLBACK_ROWS);
    const state = this.snapshot();
    const sig = `${signatureOf(state, width)}|${rows}`;
    if (!force && sig === this.lastSignature) {
      return;
    }
    const resized = rows !== this.termRows;
    this.termRows = rows;
    this.lastSignature = sig;

    const lines = renderPanel(state, this.deps.style, width);
    let out = SAVE_CURSOR;
    if (resized) {
      out += setScrollRegion(PANEL_HEIGHT + 1, this.termRows);
    }
    for (let i = 0; i < lines.length; i++) {
      out += moveTo(i + 1, 1) + CLEAR_LINE + lines[i];
    }
    out += RESTORE_CURSOR;
    this.deps.write(out);
  }
}

// Cheap redraw gate: only the fields that change what's on screen. The spinner
// frame is excluded for steady states (online/offline) so an idle tunnel does
// not repaint on every 120 ms tick — only when uptime seconds or metrics move.
function signatureOf(state: PanelState, width: number): string {
  const animated = state.status === "online" || state.status === "offline";
  const spin = animated ? "" : state.spinnerFrame;
  const metrics =
    state.metrics !== null
      ? `${state.metrics.total}/${state.metrics.open}`
      : "-";
  const session =
    state.session !== null
      ? `${state.session.url}|${state.session.forwardTo}|${state.session.gateway}|${state.session.version}`
      : "-";
  const seconds = Math.floor(state.uptimeMs / MS_PER_SECOND);
  return `${state.status}|${state.detail}|${spin}|${seconds}|${metrics}|${width}|${session}`;
}
