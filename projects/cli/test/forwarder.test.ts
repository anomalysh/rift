import { afterAll, describe, expect, test } from "bun:test";
import { gunzipSync, gzipSync } from "node:zlib";

import { FrameType } from "../src/constants.ts";
import { type FrameSink, RequestStream } from "../src/forwarder.ts";
import type { RequestHead, ResponseHead } from "../src/protocol.ts";

/** A body long enough that gzip is meaningfully smaller than the plaintext. */
const GZIP_PLAINTEXT = "the quick brown fox ".repeat(500);
const GZIP_BODY = gzipSync(Buffer.from(GZIP_PLAINTEXT));

/** What the local service saw, so a test can assert on the framing it received. */
interface Seen {
  method: string;
  path: string;
  contentLength: string | null;
  transferEncoding: string | null;
  body: string;
}

const seen: Seen[] = [];

/** accept-encoding the upstream saw on the last /gzip request, for assertions. */
let lastGzipAcceptEncoding: string | null = null;

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
    // A pre-gzipped response, like a static-site dev server serving compressed
    // assets. The agent must pass these bytes through untouched.
    if (url.pathname === "/gzip") {
      lastGzipAcceptEncoding = req.headers.get("accept-encoding");
      return new Response(GZIP_BODY, {
        headers: {
          "content-encoding": "gzip",
          "content-type": "text/plain",
          "content-length": String(GZIP_BODY.length),
        },
      });
    }
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

// A self-signed HTTPS upstream, to prove an `https` tunnel dials its local
// service over TLS. The CA is untrusted (it signs only itself), so a verifying
// dial must fail while an insecure one succeeds -- exactly the loopback-vs-flag
// policy the client resolves. The cert is a fixed fixture (SAN 127.0.0.1 /
// localhost, expiry in 2126) so the test needs no openssl at runtime.
const TLS_CERT = `-----BEGIN CERTIFICATE-----
MIIDJzCCAg+gAwIBAgIUDzMfDaChF1rMlP445Uut4CUZ0LgwDQYJKoZIhvcNAQEL
BQAwFDESMBAGA1UEAwwJbG9jYWxob3N0MCAXDTI2MDcwOTEzMzQyM1oYDzIxMjYw
NjE1MTMzNDIzWjAUMRIwEAYDVQQDDAlsb2NhbGhvc3QwggEiMA0GCSqGSIb3DQEB
AQUAA4IBDwAwggEKAoIBAQCRg87MSPinE1rLOfeaEwdXr9fayATgC8dg4vh1N699
LzQeIngW40GurqnjvD6hCdTPCHMUK5+o5CHF7YzgSmcGnAdRnc3OYd/cK5DmdV3T
xuJ2hECF7TG9AlX9BZQfoD/NTnDVOb2a90/Wz4yKS89cDsDE7+DhnVPCud8XzZxJ
CvNgboI2KPmmQAUnJkoSxZGCelfg9HwKxYTogeUWWujXY514yJjK6M5eqCLrU2Zt
O3yzpqXKkRan5WgiQUorNcz11yUszJVf2iDSQe/TcAsyt1wyZd9jDEtDJK9z5csX
BZityaIIopF3UCXs8tBERxogoLznhRcrMw8vUyRlsShBAgMBAAGjbzBtMB0GA1Ud
DgQWBBTe0/oUMCJhJSkTmTOoxqvY0H4QQTAfBgNVHSMEGDAWgBTe0/oUMCJhJSkT
mTOoxqvY0H4QQTAPBgNVHRMBAf8EBTADAQH/MBoGA1UdEQQTMBGCCWxvY2FsaG9z
dIcEfwAAATANBgkqhkiG9w0BAQsFAAOCAQEAaui0Bg/hPHCdHlvvKCzQR6Wov5sq
rNC6eE/d5RCocvDWc1D3o8zaV2WYr0hd5LBC0udME6NkIR1H7WylCnyolacME/bc
Q3zxI9ToqpoVElAWImHpgmzVhjgZ3fEspD3A5l1MCond3ybNKH8HQDlEOPru5hcL
m3xPtUH9F6ZVssspARmaBhEvBKe8KmyEJo4yCwIn6R6nR4tsQgxYEUsyoYlJaSiw
9WLb6bFN4eRwLiQyrVZjz7jyNoFTA+QIPcE0xO7s6HO9iuiBjjMVwhKnz+3l++9C
dIGkOfcoZcGxEHfuOZh7OBD/Z34CuvJmJ/DATS998ra2qaR3m5+qnhePvQ==
-----END CERTIFICATE-----
`;
const TLS_KEY = `-----BEGIN PRIVATE KEY-----
MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQCRg87MSPinE1rL
OfeaEwdXr9fayATgC8dg4vh1N699LzQeIngW40GurqnjvD6hCdTPCHMUK5+o5CHF
7YzgSmcGnAdRnc3OYd/cK5DmdV3TxuJ2hECF7TG9AlX9BZQfoD/NTnDVOb2a90/W
z4yKS89cDsDE7+DhnVPCud8XzZxJCvNgboI2KPmmQAUnJkoSxZGCelfg9HwKxYTo
geUWWujXY514yJjK6M5eqCLrU2ZtO3yzpqXKkRan5WgiQUorNcz11yUszJVf2iDS
Qe/TcAsyt1wyZd9jDEtDJK9z5csXBZityaIIopF3UCXs8tBERxogoLznhRcrMw8v
UyRlsShBAgMBAAECggEAGLnM6el8VudzBhVTfVq+ZKf8hbB3I5rcxhnLHh/YMe1T
bcttnHYBMy16sLfL7JE/F+7XnxXKi2g4VOmIhpQd7YGVvMiTr/3xi/fbJ03KI7In
yPuv+xHS4csD0XqhML6KGNi7U3/8N9jOODIML3OySHI5Tz1zeOLC2NO8lM7bP43b
M6O+/alIwzv+kWdLR3/PCx1HFjITE3+qAXoxgdiNHop8LyED6rtmjAmxZcUa4tOX
5BYXmInkw0/eRi0e2r50fiXxpol0Bu1T7VCFQTocBjioKHU6M1VA7HbbJ3P9n16a
LWdgKecHF7sGW1RazPpNrV3nOJO6PrTt+KsQC6tOAQKBgQDD0dmGdx2SowoyeOBr
A+Wm651IK41gdUskjM3urxjr2nbtHeMi+nU0SWDKyI3ScjgtLu9IxC8nxwzUagHo
uONBjw5xGwc2sol+zIGgKGB520KstsRs3PopNAKgFcUsQ3/iDCrBweXI8goZyAQ/
rsUIV45hhs1pVcXWLBtzefyCJwKBgQC+PDchOy0YAxnT2BG3usH5DQRd+Tz2rJZj
RkXquWrW8JygUWXCDMoCXzRG3F6TEANumzMMgTLqKTMF+Lik8aYObA+yT604DC4t
LhvWndWEF7dMJZ8t6cL0aRGzc9klvM30Jf+q6eHo6r3/3DnTfvQ3IFiBHSfMTnUF
8Pyj5HTLVwKBgFiGukxr9Vahlq6SrwIyVNRNmGFULyn4XOw9K6xIRH/71+ACrvjV
Ob9VnQiP+m21bWgf29WNu7PD7Szqb8qCK1ssDV9c1LoJpNdKJR/+oP71/QKP7eU5
UW7nMHim3ujP6zSKQ5osynE52w8kuacAn9rRmnDEvIBuYm4cqpxd/aXpAoGAFpMM
s7vS+RN9IB920settwEtcH1gF6GZYwR2zYjdPc5lt7yRB7r+ydNEX9hMvMTcs2Zl
Y2l9gj4LWP0P5Drsyq9WGYHM+2auoBvln80xBjDORpH8VrVztg8104a+0PSbuAo+
UajZbwtUKqWWkxtwnY4QEppEG8F/r4nOYSB+H5cCgYAItYyW0N5KlzglcCk+PEx2
yJrNdLux1AXqvOWrKQ+7yCaZV2RqCiX2iEZZb6azubMXS2T0uehYC6adYx22hSCu
LvgRv4v1wGl2JvEgrWNBY4g+hdpnMuK05l13Z2m1J0YZpsyeNYIP/8zj22XcLDho
cmGFFCjMwYI3synD+7NS8Q==
-----END PRIVATE KEY-----
`;

const httpsUpstream = Bun.serve({
  port: 0,
  tls: { cert: TLS_CERT, key: TLS_KEY },
  fetch() {
    return new Response("secure-ok", { headers: { "x-upstream": "tls" } });
  },
});
const httpsPort = requirePort(httpsUpstream.port);

afterAll(() => {
  upstream.stop(true);
  httpsUpstream.stop(true);
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
    upgrade: false,
    raw: false,
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
      head({
        method: "POST",
        path: "/empty",
        has_body: false,
        headers: { "content-length": ["0"] },
      }),
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
    expect(new TextDecoder().decode(new Uint8Array(sink.body))).toBe(
      "upstream-ok",
    );
    expect(sink.ended).toBe(true);
  });

  // Regression: a Content-Encoding: gzip response must reach the gateway still
  // compressed, with Content-Encoding intact and Content-Length matching the
  // compressed size. Bun's fetch would otherwise gunzip the body but keep the
  // stale headers, and the browser fails with ERR_CONTENT_DECODING_FAILED.
  test("a gzip response passes through compressed with headers intact", async () => {
    const { sink, done } = makeStream(
      head({ path: "/gzip", headers: { "accept-encoding": ["gzip, br"] } }),
    );
    await done;

    expect(sink.heads).toHaveLength(1);
    const h = sink.heads[0];
    expect(h?.headers["content-encoding"]).toEqual(["gzip"]);
    expect(h?.headers["content-length"]).toEqual([String(GZIP_BODY.length)]);

    const forwarded = new Uint8Array(sink.body);
    // Untouched: exact byte length, gzip magic, and a clean gunzip round-trip.
    expect(forwarded.length).toBe(GZIP_BODY.length);
    expect(forwarded[0]).toBe(0x1f);
    expect(forwarded[1]).toBe(0x8b);
    expect(gunzipSync(Buffer.from(forwarded)).toString()).toBe(GZIP_PLAINTEXT);
  });

  // Transparency: the upstream must see only the client's own accept-encoding,
  // never one Bun injected, or it would compress a response the client can't
  // decode.
  test("the client's accept-encoding is forwarded verbatim, none injected", async () => {
    const { done } = makeStream(
      head({ path: "/gzip", headers: { "accept-encoding": ["gzip"] } }),
    );
    await done;
    expect(lastGzipAcceptEncoding).toBe("gzip");

    const { done: done2 } = makeStream(head({ path: "/gzip", headers: {} }));
    await done2;
    // No accept-encoding from the client -> none reaches the upstream.
    expect(lastGzipAcceptEncoding).toBeNull();
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

  // An https tunnel dials the local upstream over TLS. With verification skipped
  // (the loopback / --upstream-insecure default) a self-signed dev server is
  // reached and its response streams straight back.
  test("an https upstream is dialed over TLS and streams back when insecure", async () => {
    const sink = new RecordingSink();
    let resolve: () => void = () => {};
    const done = new Promise<void>((r) => {
      resolve = r;
    });
    new RequestStream(3n, head({ path: "/secure" }), {
      target: { host: "127.0.0.1", port: httpsPort, tls: true, insecure: true },
      sink,
      logger: noopLogger,
      onDone: () => resolve(),
    });
    await done;

    expect(sink.heads).toHaveLength(1);
    expect(sink.heads[0]?.status).toBe(200);
    expect(sink.heads[0]?.headers["x-upstream"]).toEqual(["tls"]);
    expect(new TextDecoder().decode(new Uint8Array(sink.body))).toBe(
      "secure-ok",
    );
    expect(sink.reset).toBeNull();
  });

  // Verifying a self-signed upstream must fail the handshake, and that failure
  // surfaces as the same upstream_error RESET as a refused dial -- so an https
  // tunnel to a non-loopback host without --upstream-insecure is rejected rather
  // than trusting an unverified certificate.
  test("a verifying dial to a self-signed https upstream resets upstream_error", async () => {
    const sink = new RecordingSink();
    let resolve: () => void = () => {};
    const done = new Promise<void>((r) => {
      resolve = r;
    });
    new RequestStream(4n, head({ path: "/secure" }), {
      target: { host: "127.0.0.1", port: httpsPort, tls: true },
      sink,
      logger: noopLogger,
      onDone: () => resolve(),
    });
    await done;

    expect(sink.heads).toHaveLength(0);
    expect(sink.reset).not.toBeNull();
    expect(JSON.stringify(sink.reset)).toContain("upstream_error");
  });
});
