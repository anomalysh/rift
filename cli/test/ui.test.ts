import { describe, expect, test } from "bun:test";

import {
  clampWidth,
  createStyle,
  formatDuration,
  formatEvent,
  formatPlainBanner,
  formatRetryDelay,
  justify,
  padEndVisible,
  renderPanel,
  stripAnsi,
  truncateVisible,
  visibleWidth,
  type PanelState,
  type SessionInfo,
} from "../src/ui.ts";

// A disabled palette makes rendering deterministic and escape-free, so layout
// assertions measure real columns rather than colour codes.
const plain = createStyle(false);
const colored = createStyle(true);

const SESSION: SessionInfo = {
  version: "0.1.0",
  url: "https://myapp.rift.anomaly.sh",
  forwardTo: "http://127.0.0.1:3000",
  gateway: "rift.anomaly.sh",
  tunnelId: "tnl_abc123",
};

function baseState(overrides: Partial<PanelState> = {}): PanelState {
  return {
    session: SESSION,
    status: "online",
    detail: "",
    uptimeMs: 83_000,
    metrics: { total: 42, open: 3 },
    spinnerFrame: "⠹",
    ...overrides,
  };
}

describe("visible width and stripAnsi", () => {
  test("strips SGR codes and measures printed columns", () => {
    const s = colored.green("online");
    expect(s).not.toBe("online"); // colour actually applied
    expect(stripAnsi(s)).toBe("online");
    expect(visibleWidth(s)).toBe(6);
  });

  test("box and spinner glyphs count as one column each", () => {
    expect(visibleWidth("│──⠹→●│")).toBe(7);
  });
});

describe("padEndVisible", () => {
  test("pads plain text to the target width", () => {
    expect(padEndVisible("hi", 5)).toBe("hi   ");
  });

  test("pads by printed columns, ignoring colour codes", () => {
    const padded = padEndVisible(colored.red("hi"), 5);
    expect(visibleWidth(padded)).toBe(5);
    expect(stripAnsi(padded)).toBe("hi   ");
  });

  test("truncates when longer than the width", () => {
    expect(padEndVisible("abcdef", 4)).toBe("abc…");
  });
});

describe("truncateVisible", () => {
  test("plain text truncates to an ellipsis with no escape codes", () => {
    const out = truncateVisible("abcdefgh", 4);
    expect(out).toBe("abc…");
    expect(stripAnsi(out)).toBe(out); // no escapes injected
  });

  test("short strings pass through unchanged", () => {
    expect(truncateVisible("abc", 10)).toBe("abc");
  });

  test("coloured text keeps codes intact and resets at the cut", () => {
    const out = truncateVisible(colored.cyan("abcdefgh"), 4);
    expect(visibleWidth(out)).toBe(4);
    expect(stripAnsi(out)).toBe("abc…");
    expect(out.endsWith("\x1b[0m")).toBe(true);
  });
});

describe("justify", () => {
  test("flushes left and right to the edges", () => {
    expect(justify("a", "b", 5)).toBe("a   b");
  });

  test("keeps at least one space and truncates the left when tight", () => {
    const out = justify("longleft", "right", 8);
    expect(visibleWidth(out)).toBe(8);
    expect(out.endsWith("right")).toBe(true);
  });
});

describe("formatDuration", () => {
  test.each([
    [0, "0s"],
    [999, "0s"],
    [1_000, "1s"],
    [45_000, "45s"],
    [65_000, "1m 05s"],
    [3_600_000, "1h 00m"],
    [9_000_000, "2h 30m"],
  ])("%i ms -> %s", (ms, expected) => {
    expect(formatDuration(ms)).toBe(expected);
  });
});

describe("formatRetryDelay", () => {
  test.each([
    [0, "0ms"],
    [820, "820ms"],
    [999, "999ms"],
    [1_000, "1.0s"],
    [1_500, "1.5s"],
    [30_000, "30.0s"],
  ])("%i ms -> %s", (ms, expected) => {
    expect(formatRetryDelay(ms)).toBe(expected);
  });
});

describe("clampWidth", () => {
  test("caps wide terminals and floors narrow ones", () => {
    expect(clampWidth(200)).toBe(72);
    expect(clampWidth(60)).toBe(60);
    expect(clampWidth(5)).toBe(24);
  });
});

describe("renderPanel", () => {
  test("every row is exactly the panel width", () => {
    for (const width of [30, 48, 72]) {
      const lines = renderPanel(baseState(), plain, width);
      for (const line of lines) {
        expect(visibleWidth(line)).toBe(width);
      }
    }
  });

  test("frames the panel with box-drawing corners", () => {
    const lines = renderPanel(baseState(), plain, 60);
    expect(lines[0]?.startsWith("┌")).toBe(true);
    expect(lines[0]?.endsWith("┐")).toBe(true);
    expect(lines.at(-1)?.startsWith("└")).toBe(true);
    expect(lines.at(-1)?.endsWith("┘")).toBe(true);
  });

  test("an established session shows url, target, gateway, and metrics", () => {
    const text = renderPanel(baseState(), plain, 72).join("\n");
    expect(text).toContain("rift 0.1.0");
    expect(text).toContain("online");
    expect(text).toContain("●"); // steady dot when online
    expect(text).toContain(SESSION.url);
    expect(text).toContain(SESSION.forwardTo);
    expect(text).toContain(SESSION.gateway);
    expect(text).toContain("1m 23s"); // 83s uptime
    expect(text).toContain("42 total");
    expect(text).toContain("3 open");
    expect(text).toContain("Ctrl-C to quit");
  });

  test("connecting with no session shows the spinner and placeholder", () => {
    const text = renderPanel(
      baseState({ session: null, status: "connecting", metrics: null }),
      plain,
      60,
    ).join("\n");
    expect(text).toContain("⠹"); // spinner frame passed through
    expect(text).toContain("connecting");
    expect(text).toContain("establishing tunnel…");
    expect(text).not.toContain(SESSION.url);
  });

  test("reconnecting surfaces the retry detail beside the status", () => {
    const text = renderPanel(
      baseState({ status: "reconnecting", detail: "retry in 1.5s" }),
      plain,
      72,
    ).join("\n");
    expect(text).toContain("reconnecting");
    expect(text).toContain("retry in 1.5s");
  });

  test("colours the panel when the palette is enabled", () => {
    const lines = renderPanel(baseState(), colored, 60);
    const joined = lines.join("\n");
    expect(joined).toContain("\x1b["); // escape codes present
    // Width is still exact once colour codes are discounted.
    for (const line of lines) {
      expect(visibleWidth(line)).toBe(60);
    }
  });
});

describe("formatEvent", () => {
  test("prefixes a timestamp and a per-level glyph (plain)", () => {
    const line = formatEvent("info", "connecting to wss://gw", plain, Date.UTC(2026, 0, 1, 9, 8, 7));
    expect(line).toContain("connecting to wss://gw");
    expect(line).toContain("•");
    expect(stripAnsi(line)).toBe(line); // no colour when disabled
    expect(/\d{2}:\d{2}:\d{2}/.test(line)).toBe(true);
  });

  test("uses distinct glyphs per level", () => {
    const at = Date.now();
    expect(formatEvent("warn", "x", plain, at)).toContain("!");
    expect(formatEvent("error", "x", plain, at)).toContain("✗");
  });

  test("tints warnings and errors when colour is enabled", () => {
    expect(formatEvent("error", "boom", colored, Date.now())).toContain("\x1b[31m");
  });
});

describe("formatPlainBanner", () => {
  test("is colour-free and carries every field", () => {
    const banner = formatPlainBanner(SESSION);
    expect(stripAnsi(banner)).toBe(banner);
    expect(banner).toContain("rift 0.1.0");
    expect(banner).toContain(SESSION.url);
    expect(banner).toContain(SESSION.forwardTo);
    expect(banner).toContain(SESSION.gateway);
    expect(banner).toContain(SESSION.tunnelId);
  });
});
