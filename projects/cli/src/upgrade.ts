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

import type { Socket } from "bun";
import {
  FrameType,
  MAX_PAYLOAD_BYTES,
  MAX_UPGRADE_HEAD_BYTES,
  ResetCode,
  type ResetCodeValue,
} from "./constants.ts";
import type { ForwardTarget, FrameSink, Stream } from "./forwarder.ts";
import type { Logger } from "./logger.ts";
import type {
  HeaderMap,
  RequestHead,
  ResponseHead,
  StreamReset,
} from "./protocol.ts";

const EMPTY = new Uint8Array(0);
const CRLF = "\r\n";
const encoder = new TextEncoder();
const decoder = new TextDecoder();

export interface UpgradeStreamDeps {
  readonly target: ForwardTarget;
  readonly sink: FrameSink;
  readonly logger: Logger;
  readonly onDone: (streamId: bigint) => void;
}

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
  private headBuf: Uint8Array = EMPTY;
  // Client bytes and a half-close that arrived before the socket connected.
  private pending: Uint8Array[] = [];
  private endPending = false;
  // A raw (tcp/tls) stream has no HTTP handshake: no request is written and no
  // response head is parsed; bytes simply flow once the socket connects.
  private readonly raw: boolean;

  constructor(
    private readonly streamId: bigint,
    private readonly head: RequestHead,
    private readonly deps: UpgradeStreamDeps,
  ) {
    this.raw = head.raw;
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
    if (this.socket === null) {
      this.pending.push(chunk);
      return;
    }
    this.socket.write(chunk);
  }

  /** REQ_END: the client half-closed; shut down our write side to the service. */
  endBody(): void {
    if (this.aborted) {
      return;
    }
    if (this.socket === null) {
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
    this.aborted = true;
    this.terminateSocket();
    this.finish();
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
      sock.write(this.buildUpgradeRequest());
    }
    for (const chunk of this.pending) {
      sock.write(chunk);
    }
    this.pending = [];
    if (this.endPending) {
      this.endPending = false;
      this.halfCloseWrite();
    }
  }

  private onData(data: Uint8Array): void {
    if (this.aborted) {
      return;
    }
    if (this.headParsed) {
      this.sendBody(data);
      return;
    }
    this.headBuf = concat(this.headBuf, data);
    const idx = indexOfHeaderEnd(this.headBuf);
    if (idx < 0) {
      if (this.headBuf.length > MAX_UPGRADE_HEAD_BYTES) {
        this.deps.logger.warn(
          `upgrade response header exceeded ${MAX_UPGRADE_HEAD_BYTES} bytes on stream ${this.streamId}`,
        );
        this.sendReset(ResetCode.INTERNAL, "response header too large");
        // Mark aborted before tearing the socket down so the terminate's own
        // close/error callback does not fire a second, misleading RESET.
        this.aborted = true;
        this.terminateSocket();
        this.finish();
      }
      return;
    }
    const headerBytes = this.headBuf.subarray(0, idx);
    const rest = this.headBuf.subarray(idx + 4);
    this.headBuf = EMPTY;
    this.headParsed = true;
    if (!this.parseAndSendHead(headerBytes)) {
      this.aborted = true;
      this.terminateSocket();
      this.finish();
      return;
    }
    if (rest.length > 0) {
      this.sendBody(rest);
    }
  }

  /** The service sent FIN: no more service->client bytes, but we may still write. */
  private onServiceEnd(): void {
    if (this.aborted || this.serviceEnded) {
      return;
    }
    this.serviceEnded = true;
    if (this.headSent) {
      if (this.deps.sink.isOpen()) {
        this.deps.sink.send(FrameType.RES_END, this.streamId, EMPTY);
      }
    } else {
      this.sendReset(
        ResetCode.UPSTREAM_ERROR,
        "upstream closed before responding",
      );
      this.terminateSocket();
      this.finish();
    }
  }

  private onServiceClose(): void {
    if (this.finished) {
      return;
    }
    if (!this.serviceEnded && !this.aborted) {
      // A close without an observed FIN still ends the response.
      if (this.headSent) {
        if (this.deps.sink.isOpen()) {
          this.deps.sink.send(FrameType.RES_END, this.streamId, EMPTY);
        }
      } else {
        this.sendReset(
          ResetCode.UPSTREAM_ERROR,
          "upstream closed before responding",
        );
      }
    }
    this.finish();
  }

  private onError(err: unknown): void {
    if (this.aborted || this.finished) {
      return;
    }
    const message = err instanceof Error ? err.message : String(err);
    if (!this.headSent) {
      this.deps.logger.warn(
        `upstream unreachable for upgrade stream ${this.streamId}: ${message}`,
      );
      this.sendReset(ResetCode.UPSTREAM_ERROR, message);
    } else {
      this.deps.logger.warn(
        `upgrade stream ${this.streamId} failed mid-pipe: ${message}`,
      );
      this.sendReset(ResetCode.INTERNAL, message);
    }
    this.aborted = true;
    this.terminateSocket();
    this.finish();
  }

  /** Reconstruct the raw HTTP upgrade request, pointing Host at the local target. */
  private buildUpgradeRequest(): Uint8Array {
    const hostValue = `${this.deps.target.host}:${this.deps.target.port}`;
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

  /** Parse the status line + headers and frame a RES_HEAD back. */
  private parseAndSendHead(headerBytes: Uint8Array): boolean {
    const lines = decoder.decode(headerBytes).split(CRLF);
    const statusLine = lines[0] ?? "";
    const match = /^HTTP\/\d(?:\.\d)?\s+(\d{3})/.exec(statusLine);
    if (match === null) {
      this.deps.logger.warn(
        `malformed upstream status line on stream ${this.streamId}: ${JSON.stringify(statusLine)}`,
      );
      this.sendReset(ResetCode.INTERNAL, "malformed upstream status line");
      return false;
    }
    const status = Number.parseInt(match[1] as string, 10);
    const headers: HeaderMap = {};
    for (let i = 1; i < lines.length; i++) {
      const line = lines[i];
      if (line === undefined || line === "") {
        continue;
      }
      const colon = line.indexOf(":");
      if (colon < 0) {
        continue;
      }
      const name = line.slice(0, colon).trim().toLowerCase();
      const value = line.slice(colon + 1).trim();
      const bucket = headers[name];
      if (bucket === undefined) {
        headers[name] = [value];
      } else {
        bucket.push(value);
      }
    }
    const resHead: ResponseHead = { status, headers };
    this.deps.sink.sendJson(FrameType.RES_HEAD, this.streamId, resHead);
    this.headSent = true;
    return true;
  }

  private sendBody(data: Uint8Array): void {
    if (this.aborted || !this.deps.sink.isOpen()) {
      return;
    }
    let offset = 0;
    while (offset < data.length) {
      const end = Math.min(offset + MAX_PAYLOAD_BYTES, data.length);
      this.deps.sink.send(
        FrameType.RES_BODY,
        this.streamId,
        data.subarray(offset, end),
      );
      offset = end;
    }
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
    if (!this.deps.sink.isOpen()) {
      return;
    }
    const reset: StreamReset = { code };
    if (message !== "") {
      reset.message = message;
    }
    this.deps.sink.sendJson(FrameType.RESET, this.streamId, reset);
  }

  private finish(): void {
    if (this.finished) {
      return;
    }
    this.finished = true;
    this.deps.onDone(this.streamId);
  }
}

/** Concatenate two byte buffers, avoiding a copy when one is empty. */
function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
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
