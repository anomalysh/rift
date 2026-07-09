// Terminal-emulator regression for the fixed-header (ngrok-style) Dashboard.
//
// The Dashboard only emits escape sequences to an injected `write`, so we feed
// those into a tiny VT parser that maintains a screen grid AND honours a DECSTBM
// scroll region. That lets us assert the real invariant: the status panel stays
// pinned to the top rows while the log scrolls only in the region below it --
// no real TTY required.

import { describe, expect, test } from "bun:test";

import {
  clampWidth,
  createStyle,
  Dashboard,
  type DashboardDeps,
  PANEL_HEIGHT,
  renderPanel,
  type SessionInfo,
} from "../src/ui.ts";

/** A minimal VT100 screen with a scroll region: enough for the Dashboard. */
class Screen {
  private grid: string[][];
  private row = 0;
  private col = 0;
  private savedRow = 0;
  private savedCol = 0;
  private scrollTop: number;
  private scrollBottom: number;

  constructor(
    private readonly height: number,
    private readonly width: number,
  ) {
    this.grid = this.blankGrid();
    this.scrollTop = 0;
    this.scrollBottom = height - 1;
  }

  private blankGrid(): string[][] {
    return Array.from({ length: this.height }, () =>
      Array<string>(this.width).fill(" "),
    );
  }

  private blankRow(): string[] {
    return Array<string>(this.width).fill(" ");
  }

  private scrollRegionUp(): void {
    // Shift rows up within [scrollTop, scrollBottom]; blank the bottom row.
    for (let r = this.scrollTop; r < this.scrollBottom; r++) {
      this.grid[r] = this.grid[r + 1] as string[];
    }
    this.grid[this.scrollBottom] = this.blankRow();
  }

  private lineFeed(): void {
    if (this.row === this.scrollBottom) {
      this.scrollRegionUp();
    } else if (this.row < this.height - 1) {
      this.row++;
    }
  }

  write(s: string): void {
    for (let i = 0; i < s.length; ) {
      const ch = s[i] as string;
      if (ch === "\x1b") {
        const two = s.slice(i, i + 2);
        if (two === "\x1b7") {
          this.savedRow = this.row;
          this.savedCol = this.col;
          i += 2;
          continue;
        }
        if (two === "\x1b8") {
          this.row = this.savedRow;
          this.col = this.savedCol;
          i += 2;
          continue;
        }
        // biome-ignore lint/suspicious/noControlCharactersInRegex: parsing the ANSI ESC (0x1b) the Dashboard emits.
        const m = /^\x1b\[([0-9;?]*)([A-Za-z])/.exec(s.slice(i));
        if (m) {
          this.csi(m[1] as string, m[2] as string);
          i += m[0].length;
          continue;
        }
        i++; // unknown escape; skip the ESC
        continue;
      }
      if (ch === "\n") {
        this.col = 0;
        this.lineFeed();
        i++;
        continue;
      }
      if (ch === "\r") {
        this.col = 0;
        i++;
        continue;
      }
      if (this.col >= this.width) {
        this.col = 0;
        this.lineFeed();
      }
      (this.grid[this.row] as string[])[this.col] = ch;
      this.col++;
      i++;
    }
  }

  private csi(params: string, cmd: string): void {
    const parts = params.split(";");
    const n = params === "" ? 1 : Number.parseInt(parts[0] as string, 10);
    switch (cmd) {
      case "H": {
        // Cursor position (1-indexed); default 1;1.
        this.row = clamp((n || 1) - 1, 0, this.height - 1);
        this.col = clamp(
          (Number.parseInt(parts[1] ?? "1", 10) || 1) - 1,
          0,
          this.width - 1,
        );
        break;
      }
      case "r": {
        // DECSTBM set scroll region, or reset when empty. Homes the cursor.
        if (params === "") {
          this.scrollTop = 0;
          this.scrollBottom = this.height - 1;
        } else {
          this.scrollTop = clamp((n || 1) - 1, 0, this.height - 1);
          this.scrollBottom = clamp(
            (Number.parseInt(parts[1] ?? String(this.height), 10) ||
              this.height) - 1,
            0,
            this.height - 1,
          );
        }
        this.row = 0;
        this.col = 0;
        break;
      }
      case "J":
        if (params === "" || params === "2") {
          this.grid = this.blankGrid();
        }
        break;
      case "K":
        (this.grid[this.row] as string[]).fill(" ");
        break;
      // SGR colours and cursor show/hide do not move the cursor.
      default:
        break;
    }
  }

  /** Rows with trailing blanks trimmed. */
  rows(): string[] {
    return this.grid.map((r) => r.join("").replace(/\s+$/, ""));
  }
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.max(lo, Math.min(hi, v));
}

const SESSION: SessionInfo = {
  version: "0.1.0",
  url: "https://frosty-dune-5560.rift.anomaly.sh",
  forwardTo: "http://127.0.0.1:5000",
  gateway: "gateway.rift.anomaly.sh",
  tunnelId: "tnl_x",
};

interface Rig {
  screen: Screen;
  deps: DashboardDeps;
  clock: { t: number };
}

function rig(height: number, width: number): Rig {
  const screen = new Screen(height, width);
  const clock = { t: 0 };
  return {
    screen,
    clock,
    deps: {
      write: (c) => screen.write(c),
      columns: () => width,
      rows: () => height,
      style: createStyle(false),
      now: () => clock.t,
      setInterval: () => ({ cancel: () => {} }),
      onExit: () => {},
      offExit: () => {},
    },
  };
}

describe("Dashboard renders an ngrok-style fixed header", () => {
  test("the header stays pinned to the top while the log scrolls below it", () => {
    const H = 20;
    const W = 60;
    const r = rig(H, W);
    const d = new Dashboard(r.deps);

    d.start();
    d.setSession(SESSION);
    d.setStatus("online");

    // Many more events than the log region can hold, forcing it to scroll.
    for (let i = 0; i < 30; i++) {
      r.clock.t += 1000;
      d.event(`req ${i} GET /path`);
    }
    d.setStatus("online"); // force a final header repaint at the current clock

    const grid = r.screen.rows();
    const header = grid.slice(0, PANEL_HEIGHT);
    const region = grid.slice(PANEL_HEIGHT);

    // Header: the box, intact, with live fields -- and never an event line.
    expect(header[0]?.startsWith("┌")).toBe(true);
    expect(header.at(-1)?.startsWith("└")).toBe(true);
    const headerText = header.join("\n");
    expect(headerText).toContain("online");
    expect(headerText).toContain(SESSION.url);
    expect(headerText).not.toContain("req ");
    for (let i = 1; i < PANEL_HEIGHT - 1; i++) {
      expect(header[i]?.startsWith("│")).toBe(true);
    }

    // Log region: the most recent events, and no box border bled into it.
    const regionText = region.join("\n");
    expect(regionText).toContain("req 29 GET /path");
    expect(regionText).not.toContain("┌");
    expect(regionText).not.toContain("└");
    // Old events scrolled out of the region.
    expect(regionText).not.toContain("req 0 GET /path");
  });

  test("the header matches renderPanel exactly after a repaint", () => {
    const H = 24;
    const W = 72;
    const r = rig(H, W);
    const d = new Dashboard(r.deps);
    d.start();
    d.setSession(SESSION);
    r.clock.t = 5000;
    d.setStatus("online"); // repaint at a known clock

    const expected = renderPanel(
      {
        session: SESSION,
        status: "online",
        detail: "",
        uptimeMs: 5000,
        metrics: null,
        spinnerFrame: "⠋",
      },
      createStyle(false),
      clampWidth(W),
    ).map((l) => l.replace(/\s+$/, ""));

    const header = r.screen.rows().slice(0, PANEL_HEIGHT);
    expect(header).toEqual(expected);
  });

  test("close resets the scroll region and shows the cursor", () => {
    const H = 20;
    const W = 60;
    const r = rig(H, W);
    const captured: string[] = [];
    const spyDeps: DashboardDeps = {
      ...r.deps,
      write: (c) => captured.push(c),
    };
    const d = new Dashboard(spyDeps);
    d.start();
    d.setStatus("online");
    captured.length = 0;
    d.close("offline");
    const out = captured.join("");
    expect(out).toContain("\x1b[r"); // scroll region reset
    expect(out).toContain("\x1b[?25h"); // cursor restored
  });

  test("a terminal too short for the header degrades to a one-shot banner", () => {
    const H = PANEL_HEIGHT; // no room for the header plus a log line
    const W = 60;
    const r = rig(H, W);
    const captured: string[] = [];
    const d = new Dashboard({ ...r.deps, write: (c) => captured.push(c) });
    d.start();
    const out = captured.join("");
    // Degraded mode never hides the cursor or sets a scroll region.
    expect(out).not.toContain("\x1b[?25l");
    // biome-ignore lint/suspicious/noControlCharactersInRegex: asserting no DECSTBM scroll-region escape was emitted.
    expect(out).not.toMatch(/\x1b\[\d+;\d+r/);
    expect(out).toContain("┌"); // the banner is still printed once
  });
});
