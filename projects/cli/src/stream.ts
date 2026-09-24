// Plumbing shared by every per-stream handler -- RequestStream (forwarder.ts),
// UpgradeStream (upgrade.ts), and UdpStream (udp.ts): the frame sink they write
// to, the local target they dial, and the frame helpers each would otherwise
// spell out for itself.

import {
  BACKPRESSURE_THRESHOLD_BYTES,
  FrameType,
  MAX_PAYLOAD_BYTES,
  type ResetCodeValue,
} from "./constants.ts";
import type { Logger } from "./logger.ts";
import type { StreamReset } from "./protocol.ts";

/** A shared zero-length payload (RES_END, empty bodies). Never written to. */
export const EMPTY_BYTES = new Uint8Array(0);

/** Sink for outbound frames, implemented by the WebSocket client. */
export interface FrameSink {
  send(type: number, streamId: bigint, payload: Uint8Array): void;
  sendJson(type: number, streamId: bigint, payload: unknown): void;
  /** Bytes queued in the socket but not yet flushed to the network. */
  bufferedAmount(): number;
  isOpen(): boolean;
}

export interface ForwardTarget {
  readonly host: string;
  readonly port: number;
  /** Dial the local upstream over TLS (an `https` tunnel). */
  readonly tls?: boolean;
  /** Skip certificate verification on that TLS dial (self-signed upstream). */
  readonly insecure?: boolean;
  /** SNI to present; defaults to the target host. */
  readonly serverName?: string;
}

/**
 * The agent-side handler for one gateway stream. An ordinary HTTP exchange, an
 * upgraded or raw connection, and a UDP flow all implement it, so the client
 * demultiplexes REQ_BODY / REQ_END / RESET frames without caring which kind a
 * given stream is.
 */
export interface Stream {
  /** REQ_BODY: bytes from the public client. */
  pushBody(chunk: Uint8Array): void;
  /** REQ_END: the public client will send no more bytes. */
  endBody(): void;
  /** RESET (or local transport loss): abort the exchange with a reason code. */
  reset(code: string): void;
}

/** What every stream handler is constructed with. */
export interface StreamDeps {
  readonly target: ForwardTarget;
  readonly sink: FrameSink;
  readonly logger: Logger;
  /** Called exactly once when the stream is fully retired. */
  readonly onDone: (streamId: bigint) => void;
}

/** Tell the gateway a stream is aborted and why (dropped if the link is down). */
export function sendStreamReset(
  sink: FrameSink,
  streamId: bigint,
  code: ResetCodeValue,
  message: string,
): void {
  if (!sink.isOpen()) {
    return;
  }
  const reset: StreamReset = { code };
  if (message !== "") {
    reset.message = message;
  }
  sink.sendJson(FrameType.RESET, streamId, reset);
}

/** End a stream's response direction (dropped if the link is down). */
export function sendStreamEnd(sink: FrameSink, streamId: bigint): void {
  if (sink.isOpen()) {
    sink.send(FrameType.RES_END, streamId, EMPTY_BYTES);
  }
}

/** Split `data` into views of at most MAX_PAYLOAD_BYTES, one per RES_BODY. */
export function* payloadSlices(data: Uint8Array): Generator<Uint8Array> {
  for (let offset = 0; offset < data.length; offset += MAX_PAYLOAD_BYTES) {
    yield data.subarray(offset, offset + MAX_PAYLOAD_BYTES);
  }
}

/**
 * Whether the gateway link is backed up: its send buffer is over the
 * high-water mark, so a stream should stop producing service->client bytes
 * until it drains. The WebSocket client has no drain event, so callers poll.
 */
export function linkBackedUp(sink: FrameSink): boolean {
  return sink.bufferedAmount() > BACKPRESSURE_THRESHOLD_BYTES;
}

/** Concatenate two byte buffers, avoiding a copy when one is empty. */
export function concatBytes(a: Uint8Array, b: Uint8Array): Uint8Array {
  if (a.length === 0) {
    return b;
  }
  if (b.length === 0) {
    return a;
  }
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}
