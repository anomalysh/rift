import { afterAll, describe, expect, test } from "bun:test";

import { FrameType, MAX_STREAM_BUFFER_BYTES } from "../src/constants.ts";
import type { FrameSink } from "../src/forwarder.ts";
import type { RequestHead, ResponseHead } from "../src/protocol.ts";
import { ChunkedDecoder, UpgradeStream } from "../src/upgrade.ts";

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
      // Declined upgrades from a keep-alive server: the connection stays open
      // after the response, so only the agent's own framing can end the stream.
      if (path === "/decline-keepalive") {
        sock.write(
          "HTTP/1.1 426 Upgrade Required\r\nContent-Length: 5\r\nConnection: keep-alive\r\n\r\nnope!",
        );
        return;
      }
      if (path === "/decline-chunked") {
        sock.write(
          "HTTP/1.1 400 Bad Request\r\nTransfer-Encoding: chunked\r\n\r\n4;ext=1\r\nnot \r\n",
        );
        setTimeout(() => sock.write("7\r\nupgrade\r\n0\r\nX-T: 1\r\n\r\n"), 20);
        return;
      }
      if (path === "/continue") {
        // An interim 100 precedes the real (declined) response.
        sock.write(
          "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 403 Forbidden\r\nContent-Length: 2\r\n\r\nno",
        );
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
    if (type === FrameType.RES_BODY)
      this.bodyChunks.push(new Uint8Array(payload));
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
    expect(sink.heads[0]?.headers.upgrade).toEqual(["websocket"]);

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

// Regression: a declined upgrade (non-101) used to wait for the local service
// to close the connection, which a keep-alive server never does.
describe("UpgradeStream: declined upgrades end on their own framing", () => {
  function decline(path: string, method = "GET") {
    const sink = new RecordingSink();
    let done = false;
    new UpgradeStream(
      10n,
      {
        method,
        path,
        headers: { upgrade: ["websocket"], connection: ["Upgrade"] },
        host: "demo.rift.example.com",
        scheme: "https",
        remote_addr: "203.0.113.9",
        has_body: false,
        upgrade: true,
        raw: false,
      },
      {
        target: { host: "127.0.0.1", port: upstreamPort },
        sink,
        logger: noopLogger,
        onDone: () => {
          done = true;
        },
      },
    );
    return { sink, isDone: () => done };
  }

  test("Content-Length body on a kept-alive connection ends the stream", async () => {
    const { sink, isDone } = decline("/decline-keepalive");
    await waitFor(() => sink.ended && isDone(), 1000);
    expect(sink.heads[0]?.status).toBe(426);
    expect(sink.body()).toBe("nope!");
    expect(sink.reset).toBeNull();
  });

  test("a chunked body is decoded and ends at the last chunk", async () => {
    const { sink, isDone } = decline("/decline-chunked");
    await waitFor(() => sink.ended && isDone(), 1000);
    expect(sink.heads[0]?.status).toBe(400);
    expect(sink.heads[0]?.headers["transfer-encoding"]).toBeUndefined();
    expect(sink.body()).toBe("not upgrade");
    expect(sink.reset).toBeNull();
  });

  test("an interim 100 is skipped and the final response relayed", async () => {
    const { sink, isDone } = decline("/continue");
    await waitFor(() => sink.ended && isDone(), 1000);
    expect(sink.heads).toHaveLength(1);
    expect(sink.heads[0]?.status).toBe(403);
    expect(sink.body()).toBe("no");
  });
});

describe("ChunkedDecoder", () => {
  const enc = new TextEncoder();
  const dec = (parts: Uint8Array[]) =>
    Buffer.concat(parts.map((p) => Buffer.from(p))).toString();

  test("decodes byte-at-a-time input with extensions and trailers", () => {
    const d = new ChunkedDecoder();
    const wire = enc.encode(
      "3;x=y\r\nabc\r\nA\r\n0123456789\r\n0\r\nT: v\r\n\r\n",
    );
    const out: Uint8Array[] = [];
    for (let i = 0; i < wire.length; i++) {
      for (const p of d.push(wire.subarray(i, i + 1))) {
        out.push(new Uint8Array(p));
      }
    }
    expect(dec(out)).toBe("abc0123456789");
    expect(d.done).toBe(true);
  });

  test("rejects a malformed size line or missing terminator", () => {
    expect(() => new ChunkedDecoder().push(enc.encode("zz\r\n"))).toThrow();
    expect(() =>
      new ChunkedDecoder().push(enc.encode("1\r\nab\r\n")),
    ).toThrow();
  });
});

// Regression: request-line / header injection. The upgrade request is replayed
// verbatim onto a raw socket, so CR/LF in any gateway-supplied field would
// smuggle extra requests to the local service.
describe("UpgradeStream refuses unsafe request heads", () => {
  const base: RequestHead = {
    method: "GET",
    path: "/ws",
    headers: { upgrade: ["websocket"], connection: ["Upgrade"] },
    host: "demo.rift.example.com",
    scheme: "https",
    remote_addr: "203.0.113.9",
    has_body: false,
    upgrade: true,
    raw: false,
  };
  const cases: [string, Partial<RequestHead>][] = [
    ["CRLF in the method", { method: "GET /x HTTP/1.1\r\nX: y\r\n\r\nGET" }],
    ["CRLF in the path", { path: "/ws HTTP/1.1\r\nEvil: 1\r\n\r\nGET /x" }],
    ["userinfo path", { path: "@169.254.169.254/x" }],
    ["scheme-relative path", { path: "//evil.example/x" }],
    ["CRLF in a header value", { headers: { "x-a": ["1\r\nEvil: 2"] } }],
    ["bad header name", { headers: { "x a": ["1"] } }],
  ];
  for (const [label, override] of cases) {
    test(label, async () => {
      const sink = new RecordingSink();
      let done = false;
      lastRequest = "";
      new UpgradeStream(
        11n,
        { ...base, ...override },
        {
          target: { host: "127.0.0.1", port: upstreamPort },
          sink,
          logger: noopLogger,
          onDone: () => {
            done = true;
          },
        },
      );
      expect(done).toBe(true);
      expect(JSON.stringify(sink.reset)).toContain("internal");
      await new Promise((r) => setTimeout(r, 30));
      expect(lastRequest).toBe(""); // nothing was dialed or written
    });
  }
});

const RAW_HEAD: RequestHead = {
  method: "",
  path: "",
  headers: {},
  host: "",
  scheme: "",
  remote_addr: "203.0.113.9",
  has_body: false,
  upgrade: false,
  raw: true,
};

// Regression: Bun's Socket.write returns how many bytes it accepted and does
// NOT buffer the rest, so a slow local reader used to silently lose most of a
// large upload (32 MiB in, ~4 MB delivered).
describe("UpgradeStream write backpressure", () => {
  const MiB = 1 << 20;
  const expectedByte = (o: number) => ((o & 0xff) ^ (o >>> 20)) & 0xff;

  function patterned(offset: number, len: number): Uint8Array {
    const b = new Uint8Array(len);
    for (let i = 0; i < len; i++) b[i] = expectedByte(offset + i);
    return b;
  }

  test("a slow reader receives every byte, in order", async () => {
    let received = 0;
    let mismatch = -1;
    let ended = false;
    const slow = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open(sock) {
          // Stall long enough for the kernel buffers to fill and writes to
          // come back short.
          sock.pause();
          setTimeout(() => sock.resume(), 300);
        },
        data(_sock, data) {
          for (let i = 0; i < data.length; i++) {
            if (mismatch < 0 && data[i] !== expectedByte(received + i)) {
              mismatch = received + i;
            }
          }
          received += data.length;
        },
        end() {
          ended = true;
        },
      },
    });
    try {
      const sink = new RecordingSink();
      const stream = new UpgradeStream(20n, RAW_HEAD, {
        target: { host: "127.0.0.1", port: requirePort(slow.port) },
        sink,
        logger: noopLogger,
        onDone: () => {},
      });
      // Some bytes before the connect lands, the rest after, back to back.
      const total = 32 * MiB;
      stream.pushBody(patterned(0, MiB));
      await new Promise((r) => setTimeout(r, 50));
      for (let off = MiB; off < total; off += MiB) {
        stream.pushBody(patterned(off, MiB));
      }
      stream.endBody();
      await waitFor(() => ended, 15_000);
      expect(mismatch).toBe(-1);
      expect(received).toBe(total);
      expect(sink.reset).toBeNull();
    } finally {
      slow.stop(true);
    }
  }, 20_000);

  test("a backlog past the cap resets with payload_too_large", () => {
    const sink = new RecordingSink();
    let done = false;
    // Nothing listens on port 1, and the pushes below all land before the
    // (asynchronous) connect attempt resolves, so everything queues.
    const stream = new UpgradeStream(21n, RAW_HEAD, {
      target: { host: "127.0.0.1", port: 1 },
      sink,
      logger: noopLogger,
      onDone: () => {
        done = true;
      },
    });
    const chunk = new Uint8Array(MiB);
    for (let i = 0; i <= MAX_STREAM_BUFFER_BYTES / MiB && !done; i++) {
      stream.pushBody(chunk);
    }
    expect(done).toBe(true);
    expect(JSON.stringify(sink.reset)).toContain("payload_too_large");
  });
});

// Flow control toward the gateway: while the WebSocket's bufferedAmount is over
// the high-water mark, the agent must stop reading the local socket instead of
// piling a fast service's output into memory.
describe("UpgradeStream pauses reading while the gateway link is backed up", () => {
  test("reading stops over the threshold and resumes after it drains", async () => {
    const MiB = 1 << 20;
    const total = 48 * MiB;
    const block = new Uint8Array(MiB);
    const sent = new WeakMap<object, number>();
    const pump = (sock: {
      write(b: Uint8Array): number;
      end(): void;
    }): void => {
      let s = sent.get(sock) ?? 0;
      while (s < total) {
        const want = Math.min(MiB, total - s);
        const n = sock.write(block.subarray(0, want));
        s += Math.max(0, n);
        if (n < want) break;
      }
      sent.set(sock, s);
      if (s >= total) sock.end();
    };
    const fast = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open: pump,
        drain: pump,
        data() {},
      },
    });
    class GatedSink extends RecordingSink {
      buffered = 16 * MiB;
      received = 0;
      override send(type: number, _id: bigint, payload: Uint8Array): void {
        if (type === FrameType.RES_BODY) this.received += payload.length;
        if (type === FrameType.RES_END) this.ended = true;
      }
      override bufferedAmount(): number {
        return this.buffered;
      }
    }
    try {
      const sink = new GatedSink();
      new UpgradeStream(22n, RAW_HEAD, {
        target: { host: "127.0.0.1", port: requirePort(fast.port) },
        sink,
        logger: noopLogger,
        onDone: () => {},
      });
      await waitFor(() => sink.received > 0);
      await new Promise((r) => setTimeout(r, 200));
      const stalledAt = sink.received;
      await new Promise((r) => setTimeout(r, 200));
      // Paused: nothing more is read while the link is backed up.
      expect(sink.received).toBe(stalledAt);
      expect(stalledAt).toBeLessThan(total);

      sink.buffered = 0; // the link drains
      await waitFor(() => sink.ended, 10_000);
      expect(sink.received).toBe(total);
    } finally {
      fast.stop(true);
    }
  }, 20_000);
});
