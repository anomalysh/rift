import { afterAll, describe, expect, test } from "bun:test";

import { FrameType } from "../src/constants.ts";
import type { FrameSink } from "../src/forwarder.ts";
import type { RequestHead, ResponseHead } from "../src/protocol.ts";
import { UpgradeStream } from "../src/upgrade.ts";

// A minimal raw-TCP upstream: it reads the HTTP upgrade request, answers 101,
// then echoes every subsequent byte -- a stand-in for a WebSocket server that
// lets us assert the raw pipe without hand-rolling WebSocket frames. On /decline
// it answers a normal 426 with a body and closes instead of upgrading.
interface ConnState {
  buf: Uint8Array;
  upgraded: boolean;
}

let lastRequest = "";

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

const state = new WeakMap<object, ConnState>();

const upstream = Bun.listen({
  hostname: "127.0.0.1",
  port: 0,
  socket: {
    open(sock) {
      state.set(sock, { buf: new Uint8Array(0), upgraded: false });
    },
    data(sock, data) {
      const st = state.get(sock);
      if (st === undefined) return;
      if (st.upgraded) {
        sock.write(data); // echo the upgraded byte stream
        return;
      }
      st.buf = concat(st.buf, data);
      const text = Buffer.from(st.buf).toString("latin1");
      const idx = text.indexOf("\r\n\r\n");
      if (idx < 0) return;
      lastRequest = text.slice(0, idx);
      const path = /^[A-Z]+\s+(\S+)/.exec(lastRequest)?.[1] ?? "/";
      if (path === "/flood") {
        // A misbehaving/hostile local service that never terminates its header
        // block. The agent must bound what it buffers, not grow without limit.
        sock.write("A".repeat(70 * 1024));
        return;
      }
      if (path === "/decline") {
        sock.write(
          "HTTP/1.1 426 Upgrade Required\r\nContent-Length: 5\r\n\r\nnope!",
        );
        sock.end();
        return;
      }
      st.upgraded = true;
      sock.write(
        "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n",
      );
      const rest = st.buf.subarray(idx + 4);
      if (rest.length > 0) sock.write(rest); // echo any pipelined bytes
    },
  },
});

function requirePort(port: number | undefined): number {
  if (port === undefined) throw new Error("test upstream did not bind a port");
  return port;
}
const upstreamPort = requirePort(upstream.port);

afterAll(() => {
  upstream.stop(true);
});

const noopLogger = {
  debug: () => {},
  info: () => {},
  warn: () => {},
  error: () => {},
} as unknown as ConstructorParameters<typeof UpgradeStream>[2]["logger"];

class RecordingSink implements FrameSink {
  readonly heads: ResponseHead[] = [];
  readonly bodyChunks: Uint8Array[] = [];
  ended = false;
  reset: unknown = null;

  send(type: number, _id: bigint, payload: Uint8Array): void {
    // payload aliases the socket buffer; copy before retaining.
    if (type === FrameType.RES_BODY) this.bodyChunks.push(new Uint8Array(payload));
    if (type === FrameType.RES_END) this.ended = true;
  }
  sendJson(type: number, _id: bigint, payload: unknown): void {
    if (type === FrameType.RES_HEAD) this.heads.push(payload as ResponseHead);
    if (type === FrameType.RESET) this.reset = payload;
  }
  bufferedAmount(): number {
    return 0;
  }
  isOpen(): boolean {
    return true;
  }
  body(): string {
    return Buffer.concat(this.bodyChunks.map((c) => Buffer.from(c))).toString();
  }
}

async function waitFor(pred: () => boolean, ms = 2000): Promise<void> {
  const start = performance.now();
  while (!pred()) {
    if (performance.now() - start > ms) throw new Error("waitFor timed out");
    await new Promise((r) => setTimeout(r, 5));
  }
}

function makeStream(sink: RecordingSink, overrides: Partial<RequestHead> = {}) {
  const head: RequestHead = {
    method: "GET",
    path: "/ws",
    headers: {
      host: ["demo.rift.example.com"],
      upgrade: ["websocket"],
      connection: ["Upgrade"],
      "sec-websocket-key": ["dGhlIHNhbXBsZSBub25jZQ=="],
      "sec-websocket-version": ["13"],
    },
    host: "demo.rift.example.com",
    scheme: "https",
    remote_addr: "203.0.113.9",
    has_body: false,
    upgrade: true,
    raw: false,
    ...overrides,
  };
  return new UpgradeStream(1n, head, {
    target: { host: "127.0.0.1", port: upstreamPort },
    sink,
    logger: noopLogger,
    onDone: () => {},
  });
}

describe("UpgradeStream", () => {
  test("relays the 101 handshake and pipes bytes both ways", async () => {
    const sink = new RecordingSink();
    const stream = makeStream(sink);

    await waitFor(() => sink.heads.length > 0);
    expect(sink.heads[0]?.status).toBe(101);
    expect(sink.heads[0]?.headers["upgrade"]).toEqual(["websocket"]);

    // Client -> service -> (echo) -> client.
    stream.pushBody(new TextEncoder().encode("ping-frame"));
    await waitFor(() => sink.body().length >= "ping-frame".length);
    expect(sink.body()).toBe("ping-frame");
  });

  test("replays the request verbatim with Host rewritten to the local target", async () => {
    const sink = new RecordingSink();
    makeStream(sink, { path: "/ws2" });
    await waitFor(() => sink.heads.length > 0);

    expect(lastRequest).toContain("GET /ws2 HTTP/1.1");
    expect(lastRequest.toLowerCase()).toContain("upgrade: websocket");
    expect(lastRequest.toLowerCase()).toContain("connection: upgrade");
    // The public hostname must be replaced with the local target.
    expect(lastRequest).toContain(`Host: 127.0.0.1:${upstreamPort}`);
    expect(lastRequest).not.toContain("demo.rift.example.com");
  });

  test("a service that declines to upgrade relays its normal response", async () => {
    const sink = new RecordingSink();
    makeStream(sink, { path: "/decline" });

    await waitFor(() => sink.ended);
    expect(sink.heads[0]?.status).toBe(426);
    expect(sink.body()).toBe("nope!");
    expect(sink.reset).toBeNull();
  });

  // Buffer-bound safety: a response header block that never terminates must be
  // capped (MAX_UPGRADE_HEAD_BYTES) and reset, not accumulated forever.
  test("an overlong upgrade response header resets instead of buffering unboundedly", async () => {
    const sink = new RecordingSink();
    makeStream(sink, { path: "/flood" });

    await waitFor(() => sink.reset !== null);
    expect(sink.heads).toHaveLength(0);
    expect(JSON.stringify(sink.reset)).toContain("internal");
  });

  test("raw mode pipes bytes with no handshake and no RES_HEAD", async () => {
    // A pure TCP echo server -- no HTTP at all, like a tcp tunnel target.
    const echo = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open() {},
        data(sock, data) {
          sock.write(data);
        },
      },
    });
    const echoPort = requirePort(echo.port);

    try {
      const sink = new RecordingSink();
      const stream = new UpgradeStream(
        3n,
        {
          method: "",
          path: "",
          headers: {},
          host: "",
          scheme: "",
          remote_addr: "203.0.113.9",
          has_body: false,
          upgrade: false,
          raw: true,
        },
        {
          target: { host: "127.0.0.1", port: echoPort },
          sink,
          logger: noopLogger,
          onDone: () => {},
        },
      );

      // No REQ_HEAD/RES_HEAD for raw; bytes flow immediately.
      stream.pushBody(new TextEncoder().encode("raw-bytes"));
      await waitFor(() => sink.body().length >= "raw-bytes".length);
      expect(sink.body()).toBe("raw-bytes");
      expect(sink.heads).toHaveLength(0);
    } finally {
      echo.stop(true);
    }
  });

  test("an unreachable local service resets the stream", async () => {
    const sink = new RecordingSink();
    new UpgradeStream(
      2n,
      {
        method: "GET",
        path: "/ws",
        headers: { upgrade: ["websocket"], connection: ["Upgrade"] },
        host: "demo.rift.example.com",
        scheme: "https",
        remote_addr: "203.0.113.9",
        has_body: false,
        upgrade: true,
        raw: false,
      },
      {
        target: { host: "127.0.0.1", port: 1 }, // reserved, never listening
        sink,
        logger: noopLogger,
        onDone: () => {},
      },
    );

    await waitFor(() => sink.reset !== null);
    expect(JSON.stringify(sink.reset)).toContain("upstream_error");
  });
});
