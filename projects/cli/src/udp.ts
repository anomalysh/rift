// Raw UDP tunnel forwarding (P4). A udp tunnel carries each public client flow
// as one raw stream whose bytes are length-delimited datagrams: a 2-byte
// big-endian length prefix followed by the payload. This module deframes the
// client->service direction onto a local Bun UDP socket and reframes the
// service->client datagrams back onto the stream. The gateway does the mirror.

import type { udp } from "bun";
import {
  BACKPRESSURE_THRESHOLD_BYTES,
  FrameType,
  MAX_STREAM_BUFFER_BYTES,
  ResetCode,
  type ResetCodeValue,
} from "./constants.ts";
import type { ForwardTarget, FrameSink, Stream } from "./forwarder.ts";
import type { Logger } from "./logger.ts";
import type { StreamReset } from "./protocol.ts";

const EMPTY = new Uint8Array(0);

/** Largest UDP payload carried over the tunnel (matches the gateway's cap). */
export const MAX_DATAGRAM = 65507;

/** Frame one datagram as a 2-byte big-endian length prefix plus the payload. */
export function frameDatagram(payload: Uint8Array): Uint8Array {
  const framed = new Uint8Array(2 + payload.length);
  framed[0] = (payload.length >> 8) & 0xff;
  framed[1] = payload.length & 0xff;
  framed.set(payload, 2);
  return framed;
}

/**
 * A length-prefix reassembler for the client->service direction. Bytes arrive in
 * arbitrary chunks (a datagram may span several, or one chunk may hold several);
 * push returns every complete datagram and keeps the remainder. It throws on a
 * length prefix over MAX_DATAGRAM so a corrupt stream cannot force a huge read.
 */
export class Deframer {
  private buf: Uint8Array = new Uint8Array(0);

  push(chunk: Uint8Array): Uint8Array[] {
    this.buf = concat(this.buf, chunk);
    const out: Uint8Array[] = [];
    for (;;) {
      if (this.buf.length < 2) {
        return out;
      }
      // Length is known present: the guard above ensures two header bytes.
      const len = ((this.buf[0] ?? 0) << 8) | (this.buf[1] ?? 0);
      if (len > MAX_DATAGRAM) {
        throw new Error(`udp datagram length ${len} exceeds maximum`);
      }
      if (this.buf.length < 2 + len) {
        return out;
      }
      // Copy out: the backing buffer is sliced forward and later overwritten.
      out.push(this.buf.slice(2, 2 + len));
      this.buf = this.buf.subarray(2 + len);
    }
  }
}

export interface UdpStreamDeps {
  readonly target: ForwardTarget;
  readonly sink: FrameSink;
  readonly logger: Logger;
  readonly onDone: (streamId: bigint) => void;
}

/**
 * One UDP client flow on a stream_id. Construction opens a connected Bun UDP
 * socket to the local service; length-delimited datagrams are fed in via
 * pushBody and relayed, and replies are framed back onto the stream.
 */
export class UdpStream implements Stream {
  private socket: udp.ConnectedSocket<"buffer"> | null = null;
  private aborted = false;
  private finished = false;
  private readonly deframer = new Deframer();
  // Datagrams that arrived before the socket finished connecting.
  private pending: Uint8Array[] = [];
  private pendingBytes = 0;
  // Replies dropped because the gateway link was backed up (logged once).
  private droppedReplies = 0;

  constructor(
    private readonly streamId: bigint,
    private readonly deps: UdpStreamDeps,
  ) {
    void this.connect();
  }

  private async connect(): Promise<void> {
    const { host, port } = this.deps.target;
    try {
      this.socket = await Bun.udpSocket({
        connect: { hostname: host, port },
        socket: {
          data: (_sock, data) => this.onServiceDatagram(data),
          error: (_sock, err) => this.onError(err),
        },
      });
      if (this.aborted) {
        this.terminateSocket();
        return;
      }
      for (const dgram of this.pending) {
        this.socket.send(dgram);
      }
      this.pending = [];
      this.pendingBytes = 0;
    } catch (err) {
      this.onError(err);
    }
  }

  /** REQ_BODY: length-delimited datagrams bound for the local service. */
  pushBody(chunk: Uint8Array): void {
    if (this.aborted) {
      return;
    }
    let datagrams: Uint8Array[];
    try {
      datagrams = this.deframer.push(chunk);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.deps.logger.warn(
        `udp framing error on stream ${this.streamId}: ${message}`,
      );
      // The gateway still considers the flow open; tell it why it is gone
      // before tearing down locally, or its side lingers until a timeout.
      this.sendReset(ResetCode.INTERNAL, message);
      this.reset(ResetCode.INTERNAL);
      return;
    }
    for (const dgram of datagrams) {
      this.sendToService(dgram);
    }
  }

  /** REQ_END: the client flow ended. UDP has no FIN, so just tear down. */
  endBody(): void {
    this.finishAndClose();
  }

  /** RESET (or local transport loss): abort the flow. */
  reset(code: string): void {
    if (this.aborted) {
      return;
    }
    this.aborted = true;
    void code;
    this.terminateSocket();
    this.finish();
  }

  private sendToService(dgram: Uint8Array): void {
    if (this.socket === null) {
      this.pending.push(dgram);
      this.pendingBytes += dgram.length;
      if (this.pendingBytes > MAX_STREAM_BUFFER_BYTES) {
        this.deps.logger.warn(
          `udp flow ${this.streamId}: pre-connect backlog exceeded ${MAX_STREAM_BUFFER_BYTES} bytes`,
        );
        this.sendReset(ResetCode.PAYLOAD_TOO_LARGE, "pre-connect backlog full");
        this.reset(ResetCode.PAYLOAD_TOO_LARGE);
      }
      return;
    }
    // A UDP send is all-or-nothing per datagram: false means the kernel had no
    // room and dropped it, which is ordinary UDP loss rather than a truncated
    // stream, so it is not retried.
    if (!this.socket.send(dgram)) {
      this.deps.logger.debug(
        `udp flow ${this.streamId}: local send buffer full, datagram dropped`,
      );
    }
  }

  /** A datagram came back from the local service: frame it onto the stream. */
  private onServiceDatagram(data: Uint8Array<ArrayBufferLike>): void {
    if (this.aborted || !this.deps.sink.isOpen()) {
      return;
    }
    if (data.length > MAX_DATAGRAM) {
      this.deps.logger.warn(`dropping oversized udp reply (${data.length} B)`);
      return;
    }
    // A UDP socket cannot be paused, so while the gateway link is backed up the
    // reply is dropped -- the loss UDP applications already tolerate -- rather
    // than piling every datagram into the WebSocket send buffer.
    if (this.deps.sink.bufferedAmount() > BACKPRESSURE_THRESHOLD_BYTES) {
      if (this.droppedReplies === 0) {
        this.deps.logger.warn(
          `udp flow ${this.streamId}: gateway link backed up, dropping replies`,
        );
      }
      this.droppedReplies++;
      return;
    }
    this.deps.sink.send(FrameType.RES_BODY, this.streamId, frameDatagram(data));
  }

  private onError(err: unknown): void {
    if (this.aborted || this.finished) {
      return;
    }
    const message = err instanceof Error ? err.message : String(err);
    this.deps.logger.warn(`udp flow ${this.streamId} error: ${message}`);
    this.sendReset(ResetCode.UPSTREAM_ERROR, message);
    this.terminateSocket();
    this.finish();
  }

  private finishAndClose(): void {
    if (this.finished) {
      return;
    }
    if (this.deps.sink.isOpen()) {
      this.deps.sink.send(FrameType.RES_END, this.streamId, EMPTY);
    }
    this.terminateSocket();
    this.finish();
  }

  private sendReset(code: ResetCodeValue, message: string): void {
    if (!this.deps.sink.isOpen()) {
      return;
    }
    const reset: StreamReset = { code };
    if (message !== "") {
      reset.message = message;
    }
    this.deps.sink.sendJson(FrameType.RESET, this.streamId, reset);
  }

  private terminateSocket(): void {
    if (this.socket !== null) {
      this.socket.close();
      this.socket = null;
    }
  }

  private finish(): void {
    if (this.finished) {
      return;
    }
    this.finished = true;
    this.deps.onDone(this.streamId);
  }
}

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
  if (a.length === 0) {
    return b;
  }
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}
