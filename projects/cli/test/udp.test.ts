import { describe, expect, test } from "bun:test";

import { FrameType, MAX_DATAGRAM } from "../src/constants.ts";
import type { FrameSink } from "../src/stream.ts";
import { Deframer, frameDatagram, UdpStream } from "../src/udp.ts";

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

// Regression: a framing error tore the flow down locally without telling the
// gateway, whose side of the stream then lingered until a timeout.
describe("UdpStream framing error", () => {
  test("sends RESET to the gateway before tearing down", () => {
    const resets: unknown[] = [];
    let done = false;
    const sink: FrameSink = {
      send: () => {},
      sendJson: (type, _id, payload) => {
        if (type === FrameType.RESET) resets.push(payload);
      },
      bufferedAmount: () => 0,
      isOpen: () => true,
    };
    const stream = new UdpStream(5n, {
      target: { host: "127.0.0.1", port: 9 },
      sink,
      logger: {
        debug: () => {},
        info: () => {},
        warn: () => {},
        error: () => {},
        banner: () => {},
      },
      onDone: () => {
        done = true;
      },
    });
    stream.pushBody(bytes(0xff, 0xff)); // length 65535 > MAX_DATAGRAM
    expect(done).toBe(true);
    expect(resets).toHaveLength(1);
    expect(JSON.stringify(resets[0])).toContain("internal");
  });
});

// Regression: REQ_END arriving while the local UDP socket was still connecting
// sent RES_END and retired the flow, but the socket then connected anyway: it
// was never closed (leaking a handle that kept the process alive) and relayed
// the service's reply as RES_BODY after RES_END.
describe("UdpStream lifecycle while connecting", () => {
  function recordingSink(frames: string[]): FrameSink {
    const name = (type: number): string =>
      type === FrameType.RES_END
        ? "end"
        : type === FrameType.RESET
          ? "reset"
          : `0x${type.toString(16)}`;
    return {
      send: (type) => {
        frames.push(name(type));
      },
      sendJson: (type) => {
        frames.push(name(type));
      },
      bufferedAmount: () => 0,
      isOpen: () => true,
    };
  }
  const logger = {
    debug: () => {},
    info: () => {},
    warn: () => {},
    error: () => {},
    banner: () => {},
  };

  async function echoService() {
    const received: string[] = [];
    const sock = await Bun.udpSocket({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        data(s, data, port, addr) {
          received.push(new TextDecoder().decode(data));
          s.send(data, port, addr);
        },
        // The echo lands on a flow socket that is already closed: the ICMP
        // port-unreachable surfaces here as ECONNREFUSED, which is expected.
        error() {},
      },
    });
    return { sock, received };
  }

  async function waitFor(pred: () => boolean, ms = 2000): Promise<void> {
    const start = performance.now();
    while (!pred()) {
      if (performance.now() - start > ms) throw new Error("waitFor timed out");
      await new Promise((r) => setTimeout(r, 5));
    }
  }

  test("REQ_END before connect delivers the datagram, then ends cleanly", async () => {
    const svc = await echoService();
    try {
      const frames: string[] = [];
      let doneCalls = 0;
      const stream = new UdpStream(6n, {
        target: { host: "127.0.0.1", port: svc.sock.port },
        sink: recordingSink(frames),
        logger,
        onDone: () => {
          doneCalls++;
        },
      });
      stream.pushBody(frameDatagram(new TextEncoder().encode("hi")));
      stream.endBody(); // both land before Bun.udpSocket resolves
      expect(doneCalls).toBe(0);
      await waitFor(() => doneCalls > 0 && svc.received.length > 0);
      // Give the service's echo time to come back: a leaked socket relays it.
      await new Promise((r) => setTimeout(r, 50));
      expect(svc.received).toEqual(["hi"]);
      expect(frames).toEqual(["end"]);
      expect(doneCalls).toBe(1);
    } finally {
      svc.sock.close();
    }
  });

  test("RESET before connect drops the backlog and never sends it", async () => {
    const svc = await echoService();
    try {
      const frames: string[] = [];
      let doneCalls = 0;
      const stream = new UdpStream(7n, {
        target: { host: "127.0.0.1", port: svc.sock.port },
        sink: recordingSink(frames),
        logger,
        onDone: () => {
          doneCalls++;
        },
      });
      stream.pushBody(frameDatagram(new TextEncoder().encode("late")));
      stream.reset("canceled");
      stream.endBody(); // after a reset, REQ_END is a no-op
      await new Promise((r) => setTimeout(r, 50));
      expect(svc.received).toEqual([]);
      expect(frames).toEqual([]);
      expect(doneCalls).toBe(1);
    } finally {
      svc.sock.close();
    }
  });
});
