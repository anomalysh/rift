// Raw UDP tunnel forwarding (P4). A udp tunnel carries each public client flow
// as one raw stream whose bytes are length-delimited datagrams: a 2-byte
// big-endian length prefix followed by the payload. This module deframes the
// client->service direction onto a local Bun UDP socket and reframes the
// service->client datagrams back onto the stream. The gateway does the mirror.

import type { udp } from "bun";
import {
  FrameType,
  MAX_DATAGRAM,
  MAX_STREAM_BUFFER_BYTES,
  ResetCode,
  type ResetCodeValue,
} from "./constants.ts";
import { errorMessage } from "./logger.ts";
import {
  concatBytes,
  EMPTY_BYTES,
  linkBackedUp,
  type Stream,
  type StreamDeps,
  sendStreamEnd,
  sendStreamReset,
} from "./stream.ts";

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
  private buf: Uint8Array = EMPTY_BYTES;

  push(chunk: Uint8Array): Uint8Array[] {
    this.buf = concatBytes(this.buf, chunk);
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

/**
 * One UDP client flow on a stream_id. Construction opens a connected Bun UDP
 * socket to the local service; length-delimited datagrams are fed in via
 * pushBody and relayed, and replies are framed back onto the stream.
 */
export class UdpStream implements Stream {
  private socket: udp.ConnectedSocket<"buffer"> | null = null;
  // Set once the flow is torn down for any reason (REQ_END, RESET, error); no
  // datagram is relayed in either direction after it.
  private aborted = false;
  private finished = false;
  private readonly deframer = new Deframer();
  // Datagrams that arrived before the socket finished connecting.
  private pending: Uint8Array[] = [];
  private pendingBytes = 0;
  // REQ_END arrived before the socket connected: end once pending is flushed.
  private endPending = false;
  // Replies dropped because the gateway link was backed up (logged once).
  private droppedReplies = 0;

  constructor(
    private readonly streamId: bigint,
    private readonly deps: StreamDeps,
  ) {
    void this.connect();
  }

  private async connect(): Promise<void> {
    const { host, port } = this.deps.target;
    let socket: udp.ConnectedSocket<"buffer">;
    try {
      socket = await Bun.udpSocket({
        connect: { hostname: host, port },
        socket: {
          data: (_sock, data) => this.onServiceDatagram(data),
          error: (_sock, err) => this.onError(err),
        },
      });
    } catch (err) {
      this.onError(err);
      return;
    }
    this.socket = socket;
    if (this.aborted) {
      // Torn down while connecting: the socket must not outlive the flow.
      this.closeSocket();
      return;
    }
    const pending = this.pending;
    this.pending = [];
    this.pendingBytes = 0;
    for (const dgram of pending) {
      this.sendToService(dgram);
    }
    if (this.endPending) {
      this.end();
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
      const message = errorMessage(err);
      // The gateway still considers the flow open; tell it why it is gone
      // before tearing down locally, or its side lingers until a timeout.
      this.fail(
        ResetCode.INTERNAL,
        message,
        `udp framing error on stream ${this.streamId}: ${message}`,
      );
      return;
    }
    for (const dgram of datagrams) {
      this.sendToService(dgram);
    }
  }

  /**
   * REQ_END: the client flow ended. UDP has no FIN, so the flow is torn down --
   * after the datagrams that preceded it, if the socket is still connecting.
   */
  endBody(): void {
    if (this.aborted) {
      return;
    }
    if (this.socket === null) {
      this.endPending = true;
      return;
    }
    this.end();
  }

  /** RESET (or local transport loss): abort the flow. */
  reset(_code: string): void {
    if (!this.aborted) {
      this.teardown();
    }
  }

  private sendToService(dgram: Uint8Array): void {
    if (this.socket === null) {
      this.pending.push(dgram);
      this.pendingBytes += dgram.length;
      if (this.pendingBytes > MAX_STREAM_BUFFER_BYTES) {
        this.fail(
          ResetCode.PAYLOAD_TOO_LARGE,
          "pre-connect backlog full",
          `udp flow ${this.streamId}: pre-connect backlog exceeded ${MAX_STREAM_BUFFER_BYTES} bytes`,
        );
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
    if (linkBackedUp(this.deps.sink)) {
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
    const message = errorMessage(err);
    this.fail(
      ResetCode.UPSTREAM_ERROR,
      message,
      `udp flow ${this.streamId} error: ${message}`,
    );
  }

  /** End the flow cleanly: RES_END, then tear down. */
  private end(): void {
    sendStreamEnd(this.deps.sink, this.streamId);
    this.teardown();
  }

  /** Abort from this side: log why, tell the gateway, and tear down. */
  private fail(code: ResetCodeValue, message: string, log: string): void {
    if (this.aborted) {
      return;
    }
    this.deps.logger.warn(log);
    sendStreamReset(this.deps.sink, this.streamId, code, message);
    this.teardown();
  }

  /** Stop relaying, close the socket, and retire the stream (idempotent). */
  private teardown(): void {
    this.aborted = true;
    this.pending = [];
    this.pendingBytes = 0;
    this.closeSocket();
    if (!this.finished) {
      this.finished = true;
      this.deps.onDone(this.streamId);
    }
  }

  private closeSocket(): void {
    const socket = this.socket;
    this.socket = null;
    try {
      socket?.close();
    } catch {
      // Already closed.
    }
  }
}
