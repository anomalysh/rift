// Raw and upgraded stream forwarding over a local TCP socket.
//
// Two flavours share the same duplex pipe:
//   - upgrade (`upgrade: true`): a WebSocket-style HTTP upgrade. fetch cannot
//     carry a 101 switch, so this replays the request verbatim, parses just
//     enough of the response to relay its head, then pipes the rest.
//   - raw (`raw: true`): a tcp/tls tunnel. No HTTP at all -- the socket is
//     connected and bytes flow immediately in both directions.
// In both, REQ_BODY carries client->service bytes and RES_BODY the reverse,
// with REQ_END/RES_END as half-closes, until either side ends.
//
// Flow control, both directions:
//   - client->service: Bun's Socket.write returns how many bytes the kernel
//     accepted and does NOT buffer the rest, so every write goes through a
//     per-stream queue that is flushed from the socket's `drain` callback.
//     The queue is capped (MAX_STREAM_BUFFER_BYTES); past it the stream is
//     reset rather than silently losing bytes.
//   - service->client: while the gateway WebSocket's bufferedAmount is over
//     BACKPRESSURE_THRESHOLD_BYTES, reading from the local socket is paused.

import type { Socket } from "bun";
import { formatAuthority } from "./config.ts";
import {
  DRAIN_POLL_INTERVAL_MS,
  FrameType,
  MAX_STREAM_BUFFER_BYTES,
  MAX_UPGRADE_HEAD_BYTES,
  ResetCode,
  type ResetCodeValue,
} from "./constants.ts";
import { errorMessage } from "./logger.ts";
import {
  appendHeader,
  type HeaderMap,
  headerMapProblem,
  newHeaderMap,
  type RequestHead,
  type ResponseHead,
  requestTargetProblem,
} from "./protocol.ts";
import {
  concatBytes,
  EMPTY_BYTES,
  linkBackedUp,
  payloadSlices,
  type Stream,
  type StreamDeps,
  sendStreamEnd,
  sendStreamReset,
} from "./stream.ts";

const CRLF = "\r\n";
const encoder = new TextEncoder();
const decoder = new TextDecoder();

/** Longest chunk-size or trailer line accepted in a chunked response. */
const MAX_CHUNK_LINE_BYTES = 4096;

/**
 * How the service->client bytes after the response head are framed.
 *   pipe    -- 101 or raw: an open-ended duplex byte stream
 *   length  -- a declined upgrade with Content-Length: exactly that many bytes
 *   chunked -- a declined upgrade with chunked encoding: decoded here
 *   close   -- a declined upgrade with neither: the body ends when the service
 *              closes the connection (RFC 9112 §6.3)
 *   none    -- no body at all (HEAD, 204, 304, Content-Length: 0)
 */
type BodyMode = "pipe" | "length" | "chunked" | "close" | "none";

/**
 * One upgraded exchange on a stream_id. Construction immediately dials the local
 * service; body frames are fed in as they arrive.
 */
export class UpgradeStream implements Stream {
  private socket: Socket | null = null;
  private aborted = false;
  private finished = false;
  private headParsed = false;
  private headSent = false;
  private serviceEnded = false;
  private headBuf: Uint8Array = EMPTY_BYTES;
  // Client->service bytes the socket has not accepted yet: everything that
  // arrived before the socket connected, then whatever a short write left over.
  private writeQueue: Uint8Array[] = [];
  private queuedBytes = 0;
  // The client half-closed; shut our write side once the queue is flushed.
  private endPending = false;
  // Reading from the local socket is paused until the gateway link drains.
  private readPaused = false;
  private drainTimer: ReturnType<typeof setTimeout> | null = null;
  private bodyMode: BodyMode = "pipe";
  private bodyRemaining = 0;
  private dechunker: ChunkedDecoder | null = null;
  // A raw (tcp/tls) stream has no HTTP handshake: no request is written and no
  // response head is parsed; bytes simply flow once the socket connects.
  private readonly raw: boolean;

  constructor(
    private readonly streamId: bigint,
    private readonly head: RequestHead,
    private readonly deps: StreamDeps,
  ) {
    this.raw = head.raw;
    if (!this.raw) {
      // The request line and headers are replayed verbatim onto a raw socket,
      // so a CR/LF or a non-origin-form target would let the gateway inject
      // extra requests or retarget the local service. asRequestHead already
      // refuses such heads; this is the independent check at the point of use.
      const problem =
        requestTargetProblem(head.method, head.path) ??
        headerMapProblem(head.headers);
      if (problem !== null) {
        this.fail(
          ResetCode.INTERNAL,
          problem,
          `refusing upgrade stream ${streamId}: ${problem}`,
        );
        return;
      }
    }
    void this.connect();
  }

  private async connect(): Promise<void> {
    const { host, port, tls } = this.deps.target;
    try {
      await Bun.connect({
        hostname: host,
        port,
        // An https tunnel dials its local upgrade target over TLS, so a replayed
        // HTTP/1.1 upgrade becomes wss to a local HTTPS server. ALPN is left at
        // its default (no h2 advertised) because a connection upgrade requires
        // HTTP/1.1. A self-signed dev cert skips verification when asked
        // (target.insecure); SNI defaults to the target host. The key is spread
        // in only when TLS is wanted: `tls?` excludes undefined under
        // exactOptionalPropertyTypes, so an explicit `tls: undefined` is a type
        // error, and a plain (non-https) tunnel wants no TLS at all.
        ...(tls === true
          ? {
              tls: {
                rejectUnauthorized: this.deps.target.insecure !== true,
                serverName: this.deps.target.serverName ?? host,
              },
            }
          : {}),
        socket: {
          open: (sock) => this.onOpen(sock),
          data: (_sock, data) => this.onData(data),
          drain: () => this.flushWrites(),
          end: () => this.onServiceEnd(),
          close: () => this.onServiceClose(),
          error: (_sock, err) => this.onError(err),
        },
      });
    } catch (err) {
      // A synchronous connect failure (bad host, refused) never fires `error`.
      this.onError(err);
    }
  }

  /** REQ_BODY: forward client bytes to the local service. */
  pushBody(chunk: Uint8Array): void {
    if (this.aborted) {
      return;
    }
    this.enqueueWrite(chunk);
  }

  /** REQ_END: the client half-closed; shut down our write side to the service. */
  endBody(): void {
    if (this.aborted) {
      return;
    }
    if (this.socket === null || this.writeQueue.length > 0) {
      // Half-closing now would cut off bytes still queued; flushWrites does it
      // once the queue is empty.
      this.endPending = true;
      return;
    }
    this.halfCloseWrite();
  }

  /** RESET / transport loss: abort the exchange and drop the socket. */
  reset(_code: string): void {
    if (this.aborted) {
      return;
    }
    this.teardown();
  }

  private onOpen(sock: Socket): void {
    this.socket = sock;
    if (this.aborted) {
      this.terminateSocket();
      return;
    }
    // P1: disable Nagle on the upstream socket. These streams (raw tcp, tls
    // passthrough, WebSocket) are interactive, so coalescing small writes only
    // adds latency. setNoDelay may be unsupported on some transports; ignore it.
    sock.setNoDelay(true);
    if (this.raw) {
      // No handshake: the connection is live and every byte is payload.
      this.headParsed = true;
      this.headSent = true;
    } else {
      // The request must precede any client bytes buffered before the connect.
      const request = this.buildUpgradeRequest();
      this.writeQueue.unshift(request);
      this.queuedBytes += request.length;
    }
    this.flushWrites();
  }

  /**
   * Write `chunk` to the service, queueing whatever the socket does not accept
   * right now. Order is preserved: while anything is queued, new bytes queue
   * behind it rather than jumping ahead with a direct write.
   */
  private enqueueWrite(chunk: Uint8Array): void {
    if (chunk.length === 0) {
      return;
    }
    let rest = chunk;
    if (this.socket !== null && this.writeQueue.length === 0) {
      const written = this.socket.write(rest);
      if (written >= rest.length) {
        return;
      }
      rest = rest.subarray(Math.max(0, written));
    }
    this.writeQueue.push(rest);
    this.queuedBytes += rest.length;
    if (this.queuedBytes > MAX_STREAM_BUFFER_BYTES) {
      const message = `local service write backlog exceeded ${MAX_STREAM_BUFFER_BYTES} bytes`;
      this.fail(
        ResetCode.PAYLOAD_TOO_LARGE,
        message,
        `resetting stream ${this.streamId}: ${message}`,
      );
    }
  }

  /** Push queued bytes into the socket; runs on open and on every `drain`. */
  private flushWrites(): void {
    const sock = this.socket;
    if (sock === null || this.aborted) {
      return;
    }
    while (this.writeQueue.length > 0) {
      const next = this.writeQueue[0] as Uint8Array;
      const written = Math.max(0, sock.write(next));
      if (written < next.length) {
        // The kernel buffer is full; `drain` calls back here when it empties.
        this.writeQueue[0] = next.subarray(written);
        this.queuedBytes -= written;
        return;
      }
      this.writeQueue.shift();
      this.queuedBytes -= next.length;
    }
    if (this.endPending) {
      this.endPending = false;
      this.halfCloseWrite();
    }
  }

  private onData(data: Uint8Array): void {
    if (this.aborted || this.finished) {
      return;
    }
    if (this.headParsed) {
      this.onResponseBytes(data);
      return;
    }
    this.headBuf = concatBytes(this.headBuf, data);
    for (;;) {
      const idx = indexOfHeaderEnd(this.headBuf);
      if (idx < 0) {
        if (this.headBuf.length > MAX_UPGRADE_HEAD_BYTES) {
          this.fail(
            ResetCode.INTERNAL,
            "response header too large",
            `upgrade response header exceeded ${MAX_UPGRADE_HEAD_BYTES} bytes on stream ${this.streamId}`,
          );
        }
        return;
      }
      const headerBytes = this.headBuf.subarray(0, idx);
      const rest = this.headBuf.subarray(idx + 4);
      this.headBuf = EMPTY_BYTES;
      const parsed = parseResponseHead(headerBytes);
      if (typeof parsed === "string") {
        this.fail(
          ResetCode.INTERNAL,
          "malformed upstream status line",
          `malformed upstream status line on stream ${this.streamId}: ${JSON.stringify(parsed)}`,
        );
        return;
      }
      if (
        parsed.status >= 100 &&
        parsed.status < 200 &&
        parsed.status !== 101
      ) {
        // An interim response (100 Continue, 103 Early Hints) precedes the real
        // one; it is not relayed, and parsing resumes on the bytes after it.
        this.headBuf = rest;
        continue;
      }
      this.headParsed = true;
      this.beginResponse(parsed);
      if (!this.finished && rest.length > 0) {
        this.onResponseBytes(rest);
      }
      return;
    }
  }

  /**
   * Relay the final response head and decide how its body is framed. A 101
   * becomes an open-ended pipe. Anything else is an ordinary HTTP response to
   * a declined upgrade: its body must be delimited here, because a keep-alive
   * service will not close the connection after it and the stream would
   * otherwise hang until the service's idle timeout.
   */
  private beginResponse(res: ResponseHead): void {
    const headers = res.headers;
    if (res.status === 101) {
      this.bodyMode = "pipe";
    } else if (
      this.head.method === "HEAD" ||
      res.status === 204 ||
      res.status === 304
    ) {
      this.bodyMode = "none";
    } else if (
      (headers["transfer-encoding"] ?? []).some((v) =>
        v.toLowerCase().includes("chunked"),
      )
    ) {
      // Decoded here; the gateway re-frames the plain body for its client.
      this.bodyMode = "chunked";
      this.dechunker = new ChunkedDecoder();
      delete headers["transfer-encoding"];
      delete headers["content-length"];
    } else if (headers["content-length"] !== undefined) {
      const declared = headers["content-length"][0] ?? "";
      if (!/^\d+$/.test(declared.trim())) {
        this.fail(
          ResetCode.UPSTREAM_ERROR,
          "invalid content-length",
          `invalid upstream Content-Length on stream ${this.streamId}: ${JSON.stringify(declared)}`,
        );
        return;
      }
      this.bodyRemaining = Number.parseInt(declared.trim(), 10);
      this.bodyMode = this.bodyRemaining === 0 ? "none" : "length";
    } else {
      this.bodyMode = "close";
    }
    this.deps.sink.sendJson(FrameType.RES_HEAD, this.streamId, res);
    this.headSent = true;
    if (this.bodyMode === "none") {
      this.completeResponse();
    }
  }

  /** Service->client bytes after the response head, framed per bodyMode. */
  private onResponseBytes(data: Uint8Array): void {
    switch (this.bodyMode) {
      case "pipe":
      case "close":
        this.sendBody(data);
        return;
      case "length": {
        const n = Math.min(this.bodyRemaining, data.length);
        this.sendBody(data.subarray(0, n));
        this.bodyRemaining -= n;
        if (this.bodyRemaining === 0) {
          this.completeResponse();
        }
        return;
      }
      case "chunked": {
        const dechunker = this.dechunker as ChunkedDecoder;
        let parts: Uint8Array[];
        try {
          parts = dechunker.push(data);
        } catch (err) {
          this.fail(
            ResetCode.UPSTREAM_ERROR,
            "malformed chunked body",
            `malformed chunked response on stream ${this.streamId}: ${errorMessage(err)}`,
          );
          return;
        }
        for (const part of parts) {
          this.sendBody(part);
        }
        if (dechunker.done) {
          this.completeResponse();
        }
        return;
      }
      case "none":
        return;
    }
  }

  /**
   * A delimited response to a declined upgrade is complete: end the stream and
   * drop the local connection. The connection cannot be reused -- the next
   * request on this tunnel opens its own stream and its own socket.
   */
  private completeResponse(): void {
    sendStreamEnd(this.deps.sink, this.streamId);
    this.serviceEnded = true;
    this.teardown();
  }

  /** A delimited body (length/chunked) that has not been fully received yet. */
  private bodyIncomplete(): boolean {
    return this.bodyMode === "length" || this.bodyMode === "chunked";
  }

  /**
   * The service sent FIN, or the socket closed: no more service->client bytes
   * (after a FIN we may still write). A response that is complete ends the
   * stream; one cut short resets it.
   */
  private onServiceEnd(): void {
    if (this.aborted || this.serviceEnded) {
      return;
    }
    this.serviceEnded = true;
    if (this.headSent && !this.bodyIncomplete()) {
      sendStreamEnd(this.deps.sink, this.streamId);
      return;
    }
    const message = this.headSent
      ? "upstream closed mid-response"
      : "upstream closed before responding";
    this.fail(
      ResetCode.UPSTREAM_ERROR,
      message,
      `upgrade stream ${this.streamId}: ${message}`,
    );
  }

  private onServiceClose(): void {
    // A close without an observed FIN still ends the response.
    this.onServiceEnd();
    this.finish();
  }

  private onError(err: unknown): void {
    const message = errorMessage(err);
    if (!this.headSent) {
      this.fail(
        ResetCode.UPSTREAM_ERROR,
        message,
        `upstream unreachable for upgrade stream ${this.streamId}: ${message}`,
      );
    } else {
      this.fail(
        ResetCode.INTERNAL,
        message,
        `upgrade stream ${this.streamId} failed mid-pipe: ${message}`,
      );
    }
  }

  /**
   * Abort from this side: log why, tell the gateway, and drop the socket. A
   * no-op once the stream is aborted, so the socket's own close/error callbacks
   * after a teardown never send a second, misleading RESET.
   */
  private fail(code: ResetCodeValue, message: string, log: string): void {
    if (this.aborted || this.finished) {
      return;
    }
    this.deps.logger.warn(log);
    this.sendReset(code, message);
    this.teardown();
  }

  /** Stop relaying, drop the local socket, and retire the stream. */
  private teardown(): void {
    this.aborted = true;
    this.terminateSocket();
    this.finish();
  }

  /** Reconstruct the raw HTTP upgrade request, pointing Host at the local target. */
  private buildUpgradeRequest(): Uint8Array {
    // Bracketed for an IPv6 target: "Host: ::1:3000" is not a valid Host.
    const hostValue = formatAuthority(
      this.deps.target.host,
      this.deps.target.port,
    );
    const lines: string[] = [`${this.head.method} ${this.head.path} HTTP/1.1`];
    let hostSet = false;
    for (const [name, values] of Object.entries(this.head.headers)) {
      if (name.toLowerCase() === "host") {
        // fetch rewrites Host to the target on the normal path; match it here so
        // vhosted local servers route the handshake correctly.
        lines.push(`Host: ${hostValue}`);
        hostSet = true;
        continue;
      }
      // Upgrade and Connection are preserved deliberately: they are the upgrade.
      for (const value of values) {
        lines.push(`${name}: ${value}`);
      }
    }
    if (!hostSet) {
      lines.push(`Host: ${hostValue}`);
    }
    return encoder.encode(lines.join(CRLF) + CRLF + CRLF);
  }

  private sendBody(data: Uint8Array): void {
    if (this.aborted || !this.deps.sink.isOpen()) {
      return;
    }
    for (const part of payloadSlices(data)) {
      this.deps.sink.send(FrameType.RES_BODY, this.streamId, part);
    }
    this.applyBackpressure();
  }

  /**
   * Stop reading the local socket while the gateway link is backed up, and
   * resume once it drains. Without this a fast local service (a large download
   * over a tcp tunnel) would pile its whole output into the WebSocket's send
   * buffer. The WebSocket client has no drain event, so it is polled, exactly
   * as RequestStream.waitForDrain does.
   */
  private applyBackpressure(): void {
    const sock = this.socket;
    if (this.readPaused || sock === null || !linkBackedUp(this.deps.sink)) {
      return;
    }
    this.readPaused = true;
    sock.pause();
    const poll = (): void => {
      this.drainTimer = null;
      if (this.aborted || this.finished || !this.deps.sink.isOpen()) {
        return;
      }
      if (linkBackedUp(this.deps.sink)) {
        this.drainTimer = setTimeout(poll, DRAIN_POLL_INTERVAL_MS);
        return;
      }
      this.readPaused = false;
      sock.resume();
    };
    this.drainTimer = setTimeout(poll, DRAIN_POLL_INTERVAL_MS);
  }

  private halfCloseWrite(): void {
    const sock = this.socket;
    if (sock === null) {
      return;
    }
    try {
      sock.shutdown();
    } catch {
      // Some transports cannot half-close; a full close is an acceptable
      // fallback since the client is done sending anyway.
      try {
        sock.end();
      } catch {
        // Already gone.
      }
    }
  }

  private terminateSocket(): void {
    const sock = this.socket;
    if (sock === null) {
      return;
    }
    try {
      sock.terminate();
    } catch {
      // Already gone.
    }
  }

  private sendReset(code: ResetCodeValue, message: string): void {
    sendStreamReset(this.deps.sink, this.streamId, code, message);
  }

  private finish(): void {
    if (this.finished) {
      return;
    }
    this.finished = true;
    if (this.drainTimer !== null) {
      clearTimeout(this.drainTimer);
      this.drainTimer = null;
    }
    this.writeQueue = [];
    this.queuedBytes = 0;
    this.deps.onDone(this.streamId);
  }
}

/**
 * Incremental decoder for an HTTP/1.1 chunked body (RFC 9112 §7.1). Bytes are
 * pushed in arbitrary slices; push returns the body bytes they carried and
 * `done` turns true once the last-chunk and trailer section are consumed.
 * Chunk extensions and trailers are accepted and discarded. Throws on a
 * malformed size line or a missing chunk terminator.
 */
export class ChunkedDecoder {
  private state: "size" | "data" | "data-end" | "trailer" | "done" = "size";
  private line = "";
  private remaining = 0;

  get done(): boolean {
    return this.state === "done";
  }

  push(data: Uint8Array): Uint8Array[] {
    const out: Uint8Array[] = [];
    let i = 0;
    while (i < data.length && this.state !== "done") {
      if (this.state === "data") {
        const n = Math.min(this.remaining, data.length - i);
        out.push(data.subarray(i, i + n));
        i += n;
        this.remaining -= n;
        if (this.remaining === 0) {
          this.state = "data-end";
        }
        continue;
      }
      // Every other state consumes one CRLF-terminated line.
      const byte = data[i] as number;
      i++;
      if (byte !== 0x0a) {
        this.line += String.fromCharCode(byte);
        if (this.line.length > MAX_CHUNK_LINE_BYTES) {
          throw new Error("chunk line too long");
        }
        continue;
      }
      const line = this.line.endsWith("\r")
        ? this.line.slice(0, -1)
        : this.line;
      this.line = "";
      switch (this.state) {
        case "size": {
          const m = /^([0-9a-fA-F]{1,12})[ \t]*(?:;.*)?$/.exec(line);
          if (m === null) {
            throw new Error(`invalid chunk size line ${JSON.stringify(line)}`);
          }
          const size = Number.parseInt(m[1] as string, 16);
          if (size === 0) {
            this.state = "trailer";
          } else {
            this.remaining = size;
            this.state = "data";
          }
          break;
        }
        case "data-end":
          if (line !== "") {
            throw new Error("chunk data not followed by CRLF");
          }
          this.state = "size";
          break;
        case "trailer":
          if (line === "") {
            this.state = "done";
          }
          break;
      }
    }
    return out;
  }
}

/**
 * Parse a response status line + header block (without the final CRLFCRLF).
 * Returns the offending status line as a string when it is malformed.
 */
function parseResponseHead(headerBytes: Uint8Array): ResponseHead | string {
  const lines = decoder.decode(headerBytes).split(CRLF);
  const statusLine = lines[0] ?? "";
  const match = /^HTTP\/\d(?:\.\d)?\s+(\d{3})/.exec(statusLine);
  if (match === null) {
    return statusLine;
  }
  const status = Number.parseInt(match[1] as string, 10);
  const headers: HeaderMap = newHeaderMap();
  for (const line of lines.slice(1)) {
    const colon = line.indexOf(":");
    if (colon < 0) {
      continue;
    }
    appendHeader(
      headers,
      line.slice(0, colon).trim().toLowerCase(),
      line.slice(colon + 1).trim(),
    );
  }
  return { status, headers };
}

/** Index of the CRLFCRLF that ends the HTTP header block, or -1 if not present. */
function indexOfHeaderEnd(buf: Uint8Array): number {
  for (let i = 0; i + 3 < buf.length; i++) {
    if (
      buf[i] === 0x0d &&
      buf[i + 1] === 0x0a &&
      buf[i + 2] === 0x0d &&
      buf[i + 3] === 0x0a
    ) {
      return i;
    }
  }
  return -1;
}
