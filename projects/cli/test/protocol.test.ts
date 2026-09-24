import { describe, expect, test } from "bun:test";

import {
  CONTROL_STREAM_ID,
  FrameType,
  HEADER_SIZE,
  MAX_PAYLOAD_BYTES,
} from "../src/constants.ts";
import {
  asHelloOk,
  asRequestHead,
  decodeControl,
  decodeFrame,
  encodeControl,
  encodeFrame,
  type Frame,
  FrameError,
  headerMapProblem,
  isKnownFrameType,
  requestTargetProblem,
} from "../src/protocol.ts";

const utf8 = new TextEncoder();

function frameErrorCode(fn: () => unknown): string {
  try {
    fn();
  } catch (err) {
    if (err instanceof FrameError) {
      return err.code;
    }
    throw err;
  }
  throw new Error("expected FrameError, none thrown");
}

describe("frame round-trip", () => {
  const dataTypes = [
    FrameType.REQ_HEAD,
    FrameType.REQ_BODY,
    FrameType.REQ_END,
    FrameType.RES_HEAD,
    FrameType.RES_BODY,
    FrameType.RES_END,
    FrameType.RESET,
  ] as const;

  test("control frame on stream 0 round-trips", () => {
    const payload = utf8.encode(`{"type":"ping"}`);
    const decoded = decodeFrame(
      encodeFrame(FrameType.CONTROL, CONTROL_STREAM_ID, payload),
    );
    expect(decoded.type).toBe(FrameType.CONTROL);
    expect(decoded.streamId).toBe(CONTROL_STREAM_ID);
    expect(decoded.payload).toEqual(payload);
  });

  test("every data frame type round-trips with a non-zero stream", () => {
    for (const type of dataTypes) {
      const payload = utf8.encode(`type-${type}`);
      const decoded = decodeFrame(encodeFrame(type, 7n, payload));
      expect(decoded.type).toBe(type);
      expect(decoded.streamId).toBe(7n);
      expect(decoded.payload).toEqual(payload);
    }
  });

  test("empty payload round-trips", () => {
    const raw = encodeFrame(FrameType.RES_END, 3n, new Uint8Array(0));
    expect(raw.length).toBe(HEADER_SIZE);
    const decoded = decodeFrame(raw);
    expect(decoded.payload.length).toBe(0);
  });

  test("max payload round-trips", () => {
    const payload = new Uint8Array(MAX_PAYLOAD_BYTES).fill(0xab);
    const raw = encodeFrame(FrameType.REQ_BODY, 9n, payload);
    expect(raw.length).toBe(HEADER_SIZE + MAX_PAYLOAD_BYTES);
    const decoded = decodeFrame(raw);
    expect(decoded.payload.length).toBe(MAX_PAYLOAD_BYTES);
    expect(decoded.payload[0]).toBe(0xab);
    expect(decoded.payload[MAX_PAYLOAD_BYTES - 1]).toBe(0xab);
  });

  test("stream_id beyond Number.MAX_SAFE_INTEGER survives as bigint", () => {
    const big = 0xffff_ffff_ffff_fffen; // 2^64 - 2
    expect(big > BigInt(Number.MAX_SAFE_INTEGER)).toBe(true);
    const decoded = decodeFrame(
      encodeFrame(FrameType.RES_BODY, big, utf8.encode("x")),
    );
    expect(decoded.streamId).toBe(big);
  });

  test("maximum uint64 stream_id round-trips", () => {
    const max = 0xffff_ffff_ffff_ffffn;
    const decoded = decodeFrame(
      encodeFrame(FrameType.REQ_HEAD, max, new Uint8Array(0)),
    );
    expect(decoded.streamId).toBe(max);
  });
});

describe("stream-id discipline", () => {
  test("encode rejects control frame on a non-zero stream", () => {
    expect(
      frameErrorCode(() =>
        encodeFrame(FrameType.CONTROL, 1n, new Uint8Array(0)),
      ),
    ).toBe("control_stream");
  });

  test("encode rejects data frame on stream 0", () => {
    expect(
      frameErrorCode(() =>
        encodeFrame(FrameType.REQ_HEAD, CONTROL_STREAM_ID, new Uint8Array(0)),
      ),
    ).toBe("data_stream");
  });

  test("decode rejects a control frame that claims a non-zero stream", () => {
    // Encode as an unknown type (no discipline), then stamp it as CONTROL.
    const raw = encodeFrame(0xee, 5n, new Uint8Array(0));
    raw[0] = FrameType.CONTROL;
    expect(frameErrorCode(() => decodeFrame(raw))).toBe("control_stream");
  });

  test("decode rejects a data frame on stream 0", () => {
    const raw = encodeFrame(0xee, CONTROL_STREAM_ID, new Uint8Array(0));
    raw[0] = FrameType.REQ_HEAD;
    expect(frameErrorCode(() => decodeFrame(raw))).toBe("data_stream");
  });
});

describe("unknown and malformed frames", () => {
  test("unknown frame type decodes without discipline enforcement", () => {
    const raw = encodeFrame(0xee, 9n, utf8.encode("x"));
    raw[0] = 0xee;
    const decoded = decodeFrame(raw);
    expect(decoded.type).toBe(0xee);
    expect(isKnownFrameType(decoded.type)).toBe(false);
    expect(decoded.payload).toEqual(utf8.encode("x"));
  });

  test("unknown frame type is allowed even on stream 0", () => {
    const raw = encodeFrame(0xef, CONTROL_STREAM_ID, new Uint8Array(0));
    const decoded = decodeFrame(raw);
    expect(decoded.type).toBe(0xef);
    expect(decoded.streamId).toBe(CONTROL_STREAM_ID);
  });

  test("truncated frame (shorter than header) is rejected", () => {
    expect(
      frameErrorCode(() => decodeFrame(new Uint8Array(HEADER_SIZE - 1))),
    ).toBe("short_frame");
  });

  test("declared length longer than payload is rejected", () => {
    const raw = encodeFrame(FrameType.REQ_BODY, 1n, utf8.encode("hello"));
    const truncated = raw.subarray(0, raw.length - 1); // drop a payload byte
    expect(frameErrorCode(() => decodeFrame(truncated))).toBe(
      "length_mismatch",
    );
  });

  test("declared length shorter than payload is rejected", () => {
    const raw = encodeFrame(FrameType.REQ_BODY, 1n, utf8.encode("hello"));
    const view = new DataView(raw.buffer);
    view.setUint32(9, 3, false); // claim 3 bytes but carry 5
    expect(frameErrorCode(() => decodeFrame(raw))).toBe("length_mismatch");
  });

  test("encode rejects an oversize payload", () => {
    expect(
      frameErrorCode(() =>
        encodeFrame(
          FrameType.REQ_BODY,
          1n,
          new Uint8Array(MAX_PAYLOAD_BYTES + 1),
        ),
      ),
    ).toBe("payload_too_large");
  });
});

describe("control envelope", () => {
  test("encode/decode round-trip with payload", () => {
    const raw = encodeControl("hello_ok", {
      tunnel_id: "T1",
      subdomain: "demo",
      hostname: "demo.example.com",
      url: "https://demo.example.com",
      heartbeat_interval_ms: 15000,
    });
    const frame: Frame = decodeFrame(raw);
    expect(frame.type).toBe(FrameType.CONTROL);
    expect(frame.streamId).toBe(CONTROL_STREAM_ID);
    const env = decodeControl(frame.payload);
    expect(env.type).toBe("hello_ok");
    const ok = asHelloOk(env.payload);
    expect(ok).not.toBeNull();
    expect(ok?.subdomain).toBe("demo");
    expect(ok?.heartbeat_interval_ms).toBe(15000);
  });

  test("decode rejects an envelope without a type", () => {
    const raw = encodeFrame(
      FrameType.CONTROL,
      CONTROL_STREAM_ID,
      utf8.encode(`{"payload":{}}`),
    );
    expect(() => decodeControl(decodeFrame(raw).payload)).toThrow();
  });

  test("guard rejects a malformed hello_ok", () => {
    expect(asHelloOk({ subdomain: "x" })).toBeNull();
  });
});

describe("asRequestHead", () => {
  const httpHead = {
    method: "GET",
    path: "/x",
    headers: { "x-a": ["1"] },
    host: "app.example.com",
    scheme: "https",
    remote_addr: "203.0.113.7:5000",
    has_body: false,
  };

  test("accepts a normal HTTP request head", () => {
    const h = asRequestHead(httpHead);
    expect(h).not.toBeNull();
    expect(h?.raw).toBe(false);
    expect(h?.headers).toEqual({ "x-a": ["1"] });
  });

  // The gateway sends RequestHead{Raw: true} for tcp/tls tunnels; Go marshals
  // its nil header map as `null`. The agent must accept that or every raw
  // tunnel stream is dropped as malformed (regression: tcp/tls tunnels broke).
  test("accepts a raw head whose headers are null", () => {
    const raw = {
      method: "",
      path: "",
      headers: null,
      host: "",
      scheme: "",
      remote_addr: "",
      has_body: false,
      raw: true,
    };
    const h = asRequestHead(raw);
    expect(h).not.toBeNull();
    expect(h?.raw).toBe(true);
    expect(h?.headers).toEqual({});
  });

  test("accepts a raw head with headers omitted entirely", () => {
    const h = asRequestHead({
      method: "",
      path: "",
      host: "",
      scheme: "",
      remote_addr: "",
      has_body: false,
      raw: true,
    });
    expect(h?.raw).toBe(true);
    expect(h?.headers).toEqual({});
  });

  test("still rejects a head with a malformed (non-map) headers value", () => {
    expect(asRequestHead({ ...httpHead, headers: "nope" })).toBeNull();
  });

  test("still rejects a head missing a required field", () => {
    expect(asRequestHead({ ...httpHead, has_body: undefined })).toBeNull();
  });
});

// Regression (SSRF / request smuggling): the path, method, and headers of a
// REQ_HEAD are spliced into a local URL or a raw request line, so an HTTP head
// must be validated before any stream is created from it.
describe("asRequestHead validates HTTP heads", () => {
  const httpHead = {
    method: "GET",
    path: "/x",
    headers: { "x-a": ["1"] },
    host: "app.example.com",
    scheme: "https",
    remote_addr: "203.0.113.7:5000",
    has_body: false,
  };

  for (const path of [
    "@169.254.169.254/latest/meta-data",
    "1/x",
    "//evil.example/x",
    "/\\evil.example",
    "",
    "x",
    "/a b",
    "/a\r\nHost: evil",
    "/a\x00",
    "/a\x7f",
  ]) {
    test(`rejects path ${JSON.stringify(path)}`, () => {
      expect(asRequestHead({ ...httpHead, path })).toBeNull();
    });
  }

  test("accepts '*' for OPTIONS only", () => {
    expect(
      asRequestHead({ ...httpHead, method: "OPTIONS", path: "*" }),
    ).not.toBeNull();
    expect(asRequestHead({ ...httpHead, path: "*" })).toBeNull();
  });

  test("accepts query strings and percent-encoding", () => {
    expect(
      asRequestHead({ ...httpHead, path: "/a%20b/c?q=1&r=%2F#frag" }),
    ).not.toBeNull();
  });

  test("rejects a method that is not an RFC 7230 token", () => {
    expect(
      asRequestHead({ ...httpHead, method: "GET /x HTTP/1.1\r\n" }),
    ).toBeNull();
    expect(asRequestHead({ ...httpHead, method: "" })).toBeNull();
  });

  test("rejects CR/LF/NUL in header values and non-token names", () => {
    expect(
      asRequestHead({ ...httpHead, headers: { "x-a": ["1\r\nEvil: 2"] } }),
    ).toBeNull();
    expect(
      asRequestHead({ ...httpHead, headers: { "x-a": ["1\x002"] } }),
    ).toBeNull();
    expect(
      asRequestHead({ ...httpHead, headers: { "x a": ["1"] } }),
    ).toBeNull();
  });

  test("an upgrade head is validated like any HTTP head", () => {
    expect(
      asRequestHead({ ...httpHead, upgrade: true, path: "@evil/x" }),
    ).toBeNull();
  });

  test("a raw head is not subject to HTTP validation", () => {
    expect(
      asRequestHead({ ...httpHead, method: "", path: "", raw: true }),
    ).not.toBeNull();
  });

  test("headers come back on a prototype-less map", () => {
    const h = asRequestHead(
      JSON.parse(
        '{"method":"GET","path":"/","host":"h","scheme":"https",' +
          '"remote_addr":"r","has_body":false,"headers":{"__proto__":["p"]}}',
      ),
    );
    expect(h).not.toBeNull();
    expect(Object.getPrototypeOf(h?.headers)).toBeNull();
    const headers = h?.headers ?? {};
    expect(Object.hasOwn(headers, "constructor")).toBe(false);
    expect(
      Object.getOwnPropertyDescriptor(headers, "__proto__")?.value,
    ).toEqual(["p"]);
  });
});

describe("requestTargetProblem / headerMapProblem", () => {
  test("report null for a safe head", () => {
    expect(requestTargetProblem("PATCH", "/x?y=1")).toBeNull();
    expect(headerMapProblem({ "content-type": ['a/b; q="x"'] })).toBeNull();
  });
});
