import { afterAll, describe, expect, test } from "bun:test";

import { FrameType } from "../src/constants.ts";
import { RequestStream, type FrameSink } from "../src/forwarder.ts";
import type { RequestHead, ResponseHead } from "../src/protocol.ts";

/** What the local service saw, so a test can assert on the framing it received. */
interface Seen {
  method: string;
  path: string;
  contentLength: string | null;
  transferEncoding: string | null;
  body: string;
}

const seen: Seen[] = [];

const upstream = Bun.serve({
  port: 0,
  async fetch(req) {
    const url = new URL(req.url);
    seen.push({
      method: req.method,
      path: url.pathname,
      contentLength: req.headers.get("content-length"),
      transferEncoding: req.headers.get("transfer-encoding"),
      body: await req.text(),
    });
    return new Response("upstream-ok", { headers: { "x-upstream": "yes" } });
  },
});

// Bun types `port` as optional because a unix-socket server has none. We asked
// for a TCP port, so prove it once rather than asserting at each use.
function requirePort(port: number | undefined): number {
  if (port === undefined) {
    throw new Error("test upstream did not bind a TCP port");
  }
  return port;
}
const upstreamPort = requirePort(upstream.port);

afterAll(() => {
  upstream.stop(true);
});

/** Collects the frames the stream would have written to the socket. */
class RecordingSink implements FrameSink {
  readonly heads: ResponseHead[] = [];
  readonly body: number[] = [];
  ended = false;
  reset: unknown = null;

  send(type: number, _id: bigint, payload: Uint8Array): void {
    if (type === FrameType.RES_BODY) this.body.push(...payload);
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
}

const noopLogger = {
  debug: () => {},
  info: () => {},
  warn: () => {},
  error: () => {},
} as unknown as ConstructorParameters<typeof RequestStream>[2]["logger"];

function makeStream(head: RequestHead): {
  stream: RequestStream;
  sink: RecordingSink;
  done: Promise<void>;
} {
  const sink = new RecordingSink();
  let resolve: () => void = () => {};
  const done = new Promise<void>((r) => {
    resolve = r;
  });
  const stream = new RequestStream(1n, head, {
    target: { host: "127.0.0.1", port: upstreamPort },
    sink,
    logger: noopLogger,
    onDone: () => resolve(),
  });
  return { stream, sink, done };
}

function head(overrides: Partial<RequestHead> = {}): RequestHead {
  return {
    method: "GET",
    path: "/",
    headers: {},
    host: "demo.rift.example.com",
    scheme: "https",
    remote_addr: "203.0.113.9",
    has_body: false,
    ...overrides,
  };
}

describe("forwarder request framing", () => {
  // The gateway knows the body's length, so the agent must declare it. Without
  // it, fetch falls back to Transfer-Encoding: chunked, and local servers that
  // never implemented chunked request decoding (python http.server, among many)
  // hand the application an empty body instead of failing loudly.
  test("a body with a known length is sent with content-length, not chunked", async () => {
    const payload = "hello-body";
    const { stream, done } = makeStream(
      head({
        method: "POST",
        path: "/submit",
        has_body: true,
        headers: { "content-length": [String(payload.length)] },
      }),
    );
    stream.pushBody(new TextEncoder().encode(payload));
    stream.endBody();
    await done;

    const got = seen.at(-1);
    expect(got?.body).toBe(payload);
    expect(got?.contentLength).toBe(String(payload.length));
    expect(got?.transferEncoding).toBeNull();
  });

  // A body whose length the public client never declared stays chunked; there
  // is nothing else it could be.
  test("a body with no declared length falls back to chunked", async () => {
    const { stream, done } = makeStream(
      head({ method: "POST", path: "/streamed", has_body: true, headers: {} }),
    );
    stream.pushBody(new TextEncoder().encode("abc"));
    stream.endBody();
    await done;

    const got = seen.at(-1);
    expect(got?.body).toBe("abc");
    expect(got?.transferEncoding).toBe("chunked");
  });

  // A declared length with no body would describe bytes that never arrive, and
  // fetch would wait for them.
  test("content-length is dropped when there is no body", async () => {
    const { done } = makeStream(
      head({ method: "POST", path: "/empty", has_body: false, headers: { "content-length": ["0"] } }),
    );
    await done;

    const got = seen.at(-1);
    expect(got?.path).toBe("/empty");
    expect(got?.body).toBe("");
  });

  test("hop-by-hop and host headers are not forwarded", async () => {
    const { done } = makeStream(
      head({
        path: "/hops",
        headers: {
          host: ["demo.rift.example.com"],
          connection: ["keep-alive"],
          "transfer-encoding": ["chunked"],
          "x-keep": ["yes"],
        },
      }),
    );
    await done;

    const got = seen.at(-1);
    expect(got?.path).toBe("/hops");
    // fetch sets Host from the target URL, so the public hostname must not leak.
    expect(got?.transferEncoding).toBeNull();
  });

  test("the response head and body are framed back to the gateway", async () => {
    const { sink, done } = makeStream(head({ path: "/ok" }));
    await done;

    expect(sink.heads).toHaveLength(1);
    expect(sink.heads[0]?.status).toBe(200);
    expect(sink.heads[0]?.headers["x-upstream"]).toEqual(["yes"]);
    expect(new TextDecoder().decode(new Uint8Array(sink.body))).toBe("upstream-ok");
    expect(sink.ended).toBe(true);
  });

  // A refused local service must become a RESET, not a hang.
  test("an unreachable local service resets the stream", async () => {
    const sink = new RecordingSink();
    let resolve: () => void = () => {};
    const done = new Promise<void>((r) => {
      resolve = r;
    });
    new RequestStream(2n, head({ path: "/dead" }), {
      // Port 1 is reserved and never listening.
      target: { host: "127.0.0.1", port: 1 },
      sink,
      logger: noopLogger,
      onDone: () => resolve(),
    });
    await done;

    expect(sink.reset).not.toBeNull();
    expect(JSON.stringify(sink.reset)).toContain("upstream_error");
  });
});
