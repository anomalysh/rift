import { describe, expect, test } from "bun:test";

import { Deframer, frameDatagram, MAX_DATAGRAM } from "../src/udp.ts";

function bytes(...v: number[]): Uint8Array {
  return new Uint8Array(v);
}

describe("frameDatagram", () => {
  test("prefixes a 2-byte big-endian length", () => {
    expect(frameDatagram(bytes(1, 2, 3))).toEqual(bytes(0, 3, 1, 2, 3));
    expect(frameDatagram(bytes())).toEqual(bytes(0, 0));
    // 300 bytes -> length 0x012C.
    const framed = frameDatagram(new Uint8Array(300));
    expect(framed[0]).toBe(0x01);
    expect(framed[1]).toBe(0x2c);
    expect(framed.length).toBe(302);
  });
});

describe("Deframer", () => {
  test("round-trips framed datagrams, preserving boundaries", () => {
    const d = new Deframer();
    const stream = new Uint8Array([
      ...frameDatagram(bytes(10, 11)),
      ...frameDatagram(bytes()), // empty datagram
      ...frameDatagram(bytes(20, 21, 22)),
    ]);
    const out = d.push(stream);
    expect(out.map((u) => [...u])).toEqual([[10, 11], [], [20, 21, 22]]);
  });

  test("reassembles a datagram split across chunks", () => {
    const d = new Deframer();
    const framed = frameDatagram(bytes(1, 2, 3, 4));
    // Feed one byte at a time; only the final byte completes the datagram.
    let completed: Uint8Array[] = [];
    for (let i = 0; i < framed.length; i++) {
      completed = completed.concat(d.push(framed.subarray(i, i + 1)));
    }
    expect(completed.map((u) => [...u])).toEqual([[1, 2, 3, 4]]);
  });

  test("handles several datagrams arriving in one chunk", () => {
    const d = new Deframer();
    const chunk = new Uint8Array([
      ...frameDatagram(bytes(1)),
      ...frameDatagram(bytes(2)),
    ]);
    expect(d.push(chunk).map((u) => [...u])).toEqual([[1], [2]]);
  });

  test("throws on a length prefix over the maximum", () => {
    const d = new Deframer();
    // 0xFFFF = 65535 > MAX_DATAGRAM (65507).
    expect(() => d.push(bytes(0xff, 0xff))).toThrow(/exceeds maximum/);
    expect(MAX_DATAGRAM).toBe(65507);
  });
});
