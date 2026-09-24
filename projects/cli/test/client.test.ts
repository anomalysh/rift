import { afterAll, describe, expect, test } from "bun:test";
import type { ServerWebSocket } from "bun";

import {
  ClientError,
  clampHeartbeatInterval,
  TunnelClient,
} from "../src/client.ts";
import type { ResolvedConfig } from "../src/config.ts";
import { ControlType, HEARTBEAT, SUBPROTOCOL } from "../src/constants.ts";
import type { Logger } from "../src/logger.ts";
import { decodeControl, decodeFrame, encodeControl } from "../src/protocol.ts";
import type { SessionInfo } from "../src/ui.ts";

// A scriptable fake gateway. Each test sets `onHello` to decide how the gateway
// answers the agent's hello; pings are deliberately never answered, so a
// connection looks exactly like a half-open one once the hello is done.
type HelloHandler = (ws: ServerWebSocket<unknown>) => void;
let onHello: HelloHandler = () => {};
let connections = 0;

const gateway = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  fetch(req, server) {
    if (
      server.upgrade(req, {
        headers: { "Sec-WebSocket-Protocol": SUBPROTOCOL },
      })
    ) {
      return undefined;
    }
    return new Response("expected a websocket", { status: 400 });
  },
  websocket: {
    open() {
      connections++;
    },
    message(ws, message) {
      if (typeof message === "string") return;
      const frame = decodeFrame(new Uint8Array(message));
      const env = decodeControl(frame.payload);
      if (env.type === ControlType.HELLO) onHello(ws);
    },
  },
});

afterAll(() => {
  gateway.stop(true);
});

function config(overrides: Partial<ResolvedConfig> = {}): ResolvedConfig {
  return {
    token: "rift_test",
    server: `ws://127.0.0.1:${gateway.port}/tunnel`,
    host: "127.0.0.1",
    logLevel: "info",
    insecure: false,
    upstreamInsecure: false,
    allowInsecureTransport: false,
    ...overrides,
  };
}

interface Capture extends Logger {
  lines: string[];
  sessions: SessionInfo[];
}

function captureLogger(): Capture {
  const lines: string[] = [];
  const sessions: SessionInfo[] = [];
  const push = (m: string): void => {
    lines.push(m);
  };
  return {
    lines,
    sessions,
    debug: () => {},
    info: push,
    warn: push,
    error: push,
    banner: push,
    session: (info) => {
      sessions.push(info);
    },
  };
}

async function waitFor(pred: () => boolean, ms: number): Promise<void> {
  const start = performance.now();
  while (!pred()) {
    if (performance.now() - start > ms) throw new Error("waitFor timed out");
    await new Promise((r) => setTimeout(r, 10));
  }
}

function helloOk(extra: Record<string, unknown> = {}): Uint8Array {
  return encodeControl(ControlType.HELLO_OK, {
    tunnel_id: "tun_1",
    subdomain: "demo",
    hostname: "demo.rift.example",
    url: "https://demo.rift.example",
    heartbeat_interval_ms: 1,
    ...extra,
  });
}

describe("clampHeartbeatInterval", () => {
  test("clamps into [1s, 5min] and defaults bad values", () => {
    expect(clampHeartbeatInterval(1)).toBe(HEARTBEAT.MIN_INTERVAL_MS);
    expect(clampHeartbeatInterval(10_000)).toBe(10_000);
    expect(clampHeartbeatInterval(24 * 3600_000)).toBe(
      HEARTBEAT.MAX_INTERVAL_MS,
    );
    expect(clampHeartbeatInterval(0)).toBe(HEARTBEAT.DEFAULT_INTERVAL_MS);
    expect(clampHeartbeatInterval(-5)).toBe(HEARTBEAT.DEFAULT_INTERVAL_MS);
    expect(clampHeartbeatInterval(Number.NaN)).toBe(
      HEARTBEAT.DEFAULT_INTERVAL_MS,
    );
  });
});

// Regression: pongs were only logged and pings had no deadline, so a half-open
// connection (sleep, NAT expiry) kept showing "online" for many minutes.
describe("dead-connection detection", () => {
  test("a silent gateway is abandoned and the agent reconnects", async () => {
    onHello = (ws) => {
      // Answer the hello, then never send another frame (no pongs).
      ws.sendBinary(helloOk());
    };
    const before = connections;
    const logger = captureLogger();
    const client = new TunnelClient({
      config: config(),
      protocol: "http",
      port: 3000,
      logger,
    });
    const run = client.run();
    try {
      await waitFor(() => connections >= before + 1, 2000);
      // Interval clamps to 1s; dead after 3 silent intervals, checked per tick.
      await waitFor(() => connections >= before + 2, 7000);
      expect(logger.lines.some((l) => l.includes("presumed dead"))).toBe(true);
    } finally {
      client.stop();
      await run;
    }
  }, 10_000);
});

// Regression: a ws:// gateway was dialed silently and the token sent in the
// clear to a remote host.
describe("cleartext transport", () => {
  test("a non-loopback ws:// gateway is refused before dialing", async () => {
    const client = new TunnelClient({
      config: config({ server: "ws://gw.example.invalid/tunnel" }),
      protocol: "http",
      port: 3000,
      logger: captureLogger(),
    });
    const err = await client.run().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ClientError);
    expect(String(err)).toContain("cleartext");
  });

  test("the explicit opt-in is honoured with a loud warning", async () => {
    // 0.0.0.0 is not a loopback name by our rules, yet on Linux/macOS a
    // connect to it reaches the local fake gateway -- a stand-in for a remote
    // gateway reached over cleartext.
    onHello = (ws) => {
      ws.sendBinary(helloOk({ heartbeat_interval_ms: 60_000 }));
    };
    const logger = captureLogger();
    const client = new TunnelClient({
      config: config({
        server: `ws://0.0.0.0:${gateway.port}/tunnel`,
        allowInsecureTransport: true,
      }),
      protocol: "http",
      port: 3000,
      logger,
    });
    const run = client.run();
    try {
      await waitFor(() => logger.sessions.length > 0, 2000);
      expect(logger.lines.some((l) => l.includes("INSECURE"))).toBe(true);
    } finally {
      client.stop();
      await run;
    }
  });
});

// Regression: gateway-controlled strings reached the terminal verbatim.
describe("gateway strings are sanitized before display", () => {
  test("hello_ok url and tunnel id lose their escape sequences", async () => {
    onHello = (ws) => {
      ws.sendBinary(
        helloOk({
          url: "https://demo.rift.example\x1b]0;pwned\x07\x1b[2J",
          tunnel_id: "tun\x1b[31m_1",
          heartbeat_interval_ms: 60_000,
        }),
      );
    };
    const logger = captureLogger();
    const client = new TunnelClient({
      config: config(),
      protocol: "http",
      port: 3000,
      logger,
    });
    const run = client.run();
    try {
      await waitFor(() => logger.sessions.length > 0, 2000);
      const s = logger.sessions[0];
      expect(s?.url).toBe("https://demo.rift.example");
      expect(s?.tunnelId).toBe("tun_1");
      expect(logger.lines.join("\n")).not.toContain("\x1b");
    } finally {
      client.stop();
      await run;
    }
  });

  test("a hello_error message cannot inject escapes into the fatal error", async () => {
    onHello = (ws) => {
      ws.sendBinary(
        encodeControl(ControlType.HELLO_ERROR, {
          code: "unauthorized",
          message: "bad token\x1b[2J\x1b]0;owned\x07\r\nFAKE: ok",
        }),
      );
      ws.close();
    };
    const client = new TunnelClient({
      config: config(),
      protocol: "http",
      port: 3000,
      logger: captureLogger(),
    });
    const err = await client.run().catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ClientError);
    const msg = (err as Error).message;
    // biome-ignore lint/suspicious/noControlCharactersInRegex: asserting none survive.
    expect(msg).not.toMatch(/[\x00-\x1f\x7f]/);
    expect(msg).toContain("bad token");
  });
});
