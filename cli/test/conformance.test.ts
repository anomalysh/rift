// Cross-language conformance: frames built by cli/src/protocol.ts must be
// byte-identical to frames built by the Go reference encoder in
// server/internal/tunnelproto.
//
// The expected hex strings below were produced by running the Go encoder
// (tunnelproto.Encode / EncodeControl / EncodeJSONFrame) via a throwaway
// program that imports github.com/siliconcolony/tunl/server/internal/tunnelproto.
// If the wire format changes, regenerate these against Go — do not hand-edit.

import { describe, expect, test } from "bun:test";

import { ControlType, FrameType } from "../src/constants.ts";
import {
  encodeControl,
  encodeFrame,
  encodeJsonFrame,
  type Hello,
  type ResponseHead,
  type StreamReset,
} from "../src/protocol.ts";

function toHex(bytes: Uint8Array): string {
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

const utf8 = new TextEncoder();

describe("conformance with Go tunnelproto encoder", () => {
  // Golden hex strings emitted by the Go encoder (see file header).
  const GO = {
    res_body_hi: "210000000000000001000000026869",
    res_end_empty: "22000000000000002a00000000",
    res_body_bigstream: "2100000100000000000000000400ff7f80",
    ctrl_ping:
      "0100000000000000000000002e7b2274797065223a2270696e67222c227061796c6f6164223a7b227473223a313733363338303830303030307d7d",
    ctrl_hello:
      "010000000000000000000000987b2274797065223a2268656c6c6f222c227061796c6f6164223a7b2270726f746f636f6c5f76657273696f6e223a312c22746f6b656e223a2274756e6c5f736563726574222c2270726f746f636f6c223a2268747470222c22737562646f6d61696e223a226d79617070222c226c6f63616c5f706f7274223a333030302c22636c69656e745f76657273696f6e223a22302e312e30227d7d",
    res_head:
      "200000000000000003000000557b22737461747573223a3230302c2268656164657273223a7b22636f6e74656e742d6c656e677468223a5b2232225d2c22636f6e74656e742d74797065223a5b226170706c69636174696f6e2f6a736f6e225d7d7d",
    reset:
      "300000000000000005000000417b22636f6465223a22757073747265616d5f6572726f72222c226d657373616765223a2245434f4e4e52454655534544203132372e302e302e313a33303030227d",
    ctrl_pong_nopayload: "0100000000000000000000000f7b2274797065223a22706f6e67227d",
  } as const;

  test("RES_BODY raw payload", () => {
    expect(toHex(encodeFrame(FrameType.RES_BODY, 1n, utf8.encode("hi")))).toBe(
      GO.res_body_hi,
    );
  });

  test("RES_END empty payload", () => {
    expect(
      toHex(encodeFrame(FrameType.RES_END, 42n, new Uint8Array(0))),
    ).toBe(GO.res_end_empty);
  });

  test("RES_BODY with stream_id beyond 2^40 and binary payload", () => {
    const streamId = 1n << 40n;
    expect(
      toHex(
        encodeFrame(
          FrameType.RES_BODY,
          streamId,
          new Uint8Array([0x00, 0xff, 0x7f, 0x80]),
        ),
      ),
    ).toBe(GO.res_body_bigstream);
  });

  test("CONTROL ping with heartbeat payload", () => {
    expect(toHex(encodeControl(ControlType.PING, { ts: 1736380800000 }))).toBe(
      GO.ctrl_ping,
    );
  });

  test("CONTROL hello with all fields (field order matches Go struct)", () => {
    const hello: Hello = {
      protocol_version: 1,
      token: "tunl_secret",
      protocol: "http",
      subdomain: "myapp",
      local_port: 3000,
      client_version: "0.1.0",
    };
    expect(toHex(encodeControl(ControlType.HELLO, hello))).toBe(GO.ctrl_hello);
  });

  test("RES_HEAD JSON frame with sorted header keys", () => {
    // Go's json.Marshal sorts map keys; content-length precedes content-type.
    const head: ResponseHead = {
      status: 200,
      headers: {
        "content-length": ["2"],
        "content-type": ["application/json"],
      },
    };
    expect(toHex(encodeJsonFrame(FrameType.RES_HEAD, 3n, head))).toBe(
      GO.res_head,
    );
  });

  test("RESET JSON frame", () => {
    const reset: StreamReset = {
      code: "upstream_error",
      message: "ECONNREFUSED 127.0.0.1:3000",
    };
    expect(toHex(encodeJsonFrame(FrameType.RESET, 5n, reset))).toBe(GO.reset);
  });

  test("CONTROL pong with no payload (payload omitted)", () => {
    expect(toHex(encodeControl(ControlType.PONG))).toBe(GO.ctrl_pong_nopayload);
  });
});
