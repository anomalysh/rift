// Terminal-emulator regression for the sticky Dashboard.
//
// The Dashboard only emits escape sequences to an injected `write`, so we can
// feed those into a tiny VT parser that maintains a screen grid -- including the
// behaviour that actually caused the bug: a newline on the bottom row SCROLLS
// the viewport. If the redraw's cursor math is wrong, events land on top of the
// panel, which shows up as a corrupted grid. This lets us prove the fix without
// a real TTY.

import { describe, expect, test } from "bun:test";
import {
  clampWidth,
  createStyle,
  Dashboard,
  type DashboardDeps,
  type PanelState,
  renderPanel,
  type SessionInfo,
} from "../src/ui.ts";

/** A minimal VT100 screen: enough of the protocol for what the Dashboard emits. */
class Screen {
  private grid: string[][];
  private row = 0;
  private col = 0;

  constructor(
    private readonly height: number,
    private readonly width: number,
  ) {
    this.grid = Array.from({ length: height }, () =>
      Array<string>(width).fill(" "),
    );
  }

  private scroll(): void {
    this.grid.shift();
    this.grid.push(Array<string>(this.width).fill(" "));
  }

  private lineFeed(): void {
    this.row++;
    if (this.row >= this.height) {
      this.scroll();
      this.row = this.height - 1;
    }
  }

  write(s: string): void {
    for (let i = 0; i < s.length; ) {
      const ch = s[i] as string;
      if (ch === "\x1b") {
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
        // The display treats a newline as move-to-start-of-next-line.
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
    const n = params === "" ? 1 : Number.parseInt(params, 10);
    switch (cmd) {
      case "A": // cursor up (the scroll-safe redraw)
        this.row = Math.max(0, this.row - n);
        break;
      case "F": // cursor previous line (the OLD, buggy redraw): up + column 0
        this.row = Math.max(0, this.row - n);
        this.col = 0;
        break;
      case "J": // erase in display; 0 = cursor to end of screen
        if (params === "" || params === "0") {
          this.eraseToEnd();
        }
        break;
      // SGR colours, cursor show/hide (?25h/l), etc. do not move the cursor.
      default:
        break;
    }
  }

  private eraseToEnd(): void {
    const line = this.grid[this.row] as string[];
    for (let c = this.col; c < this.width; c++) {
      line[c] = " ";
    }
    for (let r = this.row + 1; r < this.height; r++) {
      this.grid[r] = Array<string>(this.width).fill(" ");
    }
  }

  /** Rows with trailing blanks trimmed, for comparison. */
  rows(): string[] {
    return this.grid.map((r) => r.join("").replace(/\s+$/, ""));
  }
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
      style: createStyle(false),
      now: () => clock.t,
      setInterval: () => ({ cancel: () => {} }),
      onExit: () => {},
      offExit: () => {},
    },
  };
}

function expectedPanel(width: number, uptimeMs: number): string[] {
  const state: PanelState = {
    session: SESSION,
    status: "online",
    detail: "",
    uptimeMs,
    metrics: null,
    spinnerFrame: "⠋",
  };
  return renderPanel(state, createStyle(false), clampWidth(width)).map((l) =>
    l.replace(/\s+$/, ""),
  );
}

describe("Dashboard rendered through a VT emulator", () => {
  test("the panel stays intact after events scroll the viewport", () => {
    const H = 16;
    const W = 40;
    const r = rig(H, W);
    const d = new Dashboard(r.deps);

    d.start();
    d.setSession(SESSION);
    d.setStatus("online");

    // Enough events to push the panel to the bottom and force scrolling.
    for (let i = 0; i < 12; i++) {
      r.clock.t += 1000;
      d.event(`event line ${i}`);
    }

    const panel = expectedPanel(W, r.clock.t);
    const rows = r.screen.rows();
    // The panel must occupy the bottom rows, intact and in order -- not painted
    // over by event lines.
    const bottom = rows.slice(H - panel.length);
    expect(bottom).toEqual(panel);

    // And the most recent events must be directly above it, unclobbered.
    const above = rows.slice(0, H - panel.length).join("\n");
    expect(above).toContain("event line 11");
  });

  test("no event text bleeds into the panel region", () => {
    const H = 14;
    const W = 48;
    const r = rig(H, W);
    const d = new Dashboard(r.deps);
    d.start();
    d.setSession(SESSION);
    d.setStatus("online");
    for (let i = 0; i < 20; i++) {
      r.clock.t += 1000;
      d.event(`req ${i} GET /path`);
    }
    const panel = expectedPanel(W, r.clock.t);
    const rows = r.screen.rows();
    const bottom = rows.slice(H - panel.length);
    // Every panel row is a box row (starts with a border glyph), never an event.
    for (const row of bottom) {
      expect(
        row.startsWith("┌") || row.startsWith("│") || row.startsWith("└"),
      ).toBe(true);
      expect(row).not.toContain("req ");
    }
  });
});
