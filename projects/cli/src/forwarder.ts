// Per-stream HTTP forwarding. For each REQ_HEAD the gateway opens, one
// RequestStream issues a streaming fetch to the local service and streams the
// response back as RES_HEAD / RES_BODY* / RES_END frames, or a RESET on error.

import {
  BACKPRESSURE_THRESHOLD_BYTES,
  DRAIN_POLL_INTERVAL_MS,
  FrameType,
  HOP_BY_HOP_HEADERS,
  MAX_PAYLOAD_BYTES,
  ResetCode,
  type ResetCodeValue,
} from "./constants.ts";
import type { Logger } from "./logger.ts";
import type {
  HeaderMap,
  RequestHead,
  ResponseHead,
  StreamReset,
} from "./protocol.ts";
import type { SyntheticResponse, TrafficController } from "./traffic.ts";

const EMPTY = new Uint8Array(0);

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
 * The agent-side handler for one gateway stream. Both an ordinary HTTP exchange
 * (RequestStream) and an upgraded connection (UpgradeStream) implement it, so
 * the client demultiplexes REQ_BODY / REQ_END / RESET frames without caring
 * which kind a given stream is.
 */
export interface Stream {
  /** REQ_BODY: bytes from the public client. */
  pushBody(chunk: Uint8Array): void;
  /** REQ_END: the public client will send no more bytes. */
  endBody(): void;
  /** RESET (or local transport loss): abort the exchange with a reason code. */
  reset(code: string): void;
}

export interface RequestStreamDeps {
  readonly target: ForwardTarget;
  readonly sink: FrameSink;
  readonly logger: Logger;
  /** Called exactly once when the stream is fully retired. */
  readonly onDone: (streamId: bigint) => void;
  /** Agent-side traffic policy (headers, CORS, mock, routing, breaker). */
  readonly traffic?: TrafficController;
}

// The Fetch standard requires `duplex: "half"` when the request body is a
// stream; the ambient RequestInit type does not declare it. `decompress` is a
// Bun extension (see run() for why it is forced off); `tls` is another, used to
// dial an HTTPS upstream (see run() when target.tls).
interface FetchInit extends RequestInit {
  duplex?: "half";
  decompress?: boolean;
  tls?: BunFetchRequestInitTLS;
}

function isHopByHop(name: string): boolean {
  return HOP_BY_HOP_HEADERS.has(name) || name.startsWith("proxy-");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Build the outbound request headers, dropping hop-by-hop and `host`.
 *
 * `content-length` is deliberately preserved. The body is re-framed as a
 * stream, and a stream without a declared length makes fetch fall back to
 * `Transfer-Encoding: chunked`. Plenty of local development servers -- Python's
 * http.server among them -- never implement chunked *request* decoding, and
 * silently hand the application an empty body. Passing the length through keeps
 * identity framing, which is also what the public client sent in the first
 * place.
 */
function buildRequestHeaders(source: HeaderMap): Headers {
  const headers = new Headers();
  for (const [name, values] of Object.entries(source)) {
    const lower = name.toLowerCase();
    // `host` is the public hostname; fetch sets Host from the target URL.
    if (isHopByHop(lower) || lower === "host") {
      continue;
    }
    for (const value of values) {
      headers.append(name, value);
    }
  }
  return headers;
}

/** Convert response headers to a HeaderMap, stripping hop-by-hop headers. */
function responseHeaderMap(headers: Headers): HeaderMap {
  const out: HeaderMap = {};
  headers.forEach((value, name) => {
    const lower = name.toLowerCase();
    // set-cookie must not be comma-joined; collected separately below.
    if (lower === "set-cookie" || isHopByHop(lower)) {
      return;
    }
    const existing = out[lower];
    if (existing) {
      existing.push(value);
    } else {
      out[lower] = [value];
    }
  });
  const cookies = headers.getAll("set-cookie");
  if (cookies.length > 0) {
    out["set-cookie"] = cookies;
  }
  return out;
}

/**
 * A single proxied request/response exchange on one stream_id. Construction
 * immediately begins the upstream fetch; body frames are fed in as they arrive.
 */
export class RequestStream implements Stream {
  private readonly controller = new AbortController();
  private bodyController: ReadableStreamDefaultController<Uint8Array> | null =
    null;
  private readonly bodyStream: ReadableStream<Uint8Array> | null;
  private aborted = false;
  private finished = false;
  private bodyClosed = false;
  private headSent = false;

  constructor(
    private readonly streamId: bigint,
    private readonly head: RequestHead,
    private readonly deps: RequestStreamDeps,
  ) {
    if (head.has_body) {
      this.bodyStream = new ReadableStream<Uint8Array>({
        start: (controller) => {
          this.bodyController = controller;
        },
      });
    } else {
      this.bodyStream = null;
    }
    void this.run();
  }

  /** Feed a REQ_BODY chunk into the upstream request body. */
  pushBody(chunk: Uint8Array): void {
    if (this.bodyController !== null && !this.bodyClosed && !this.aborted) {
      this.bodyController.enqueue(chunk);
    }
  }

  /** REQ_END: no more request body. */
  endBody(): void {
    if (this.bodyController !== null && !this.bodyClosed) {
      this.bodyClosed = true;
      this.bodyController.close();
    }
  }

  /** RESET from the gateway (or a local transport loss): abort the exchange. */
  reset(code: string): void {
    if (this.aborted) {
      return;
    }
    this.aborted = true;
    this.failBody(`stream reset: ${code}`);
    this.controller.abort();
  }

  private async run(): Promise<void> {
    const traffic = this.deps.traffic;
    // T2/T3: a mock, redirect, or CORS preflight the agent answers itself,
    // never touching the local service.
    if (traffic !== undefined) {
      const synthetic = traffic.synthesize(
        this.head.method,
        this.head.path,
        this.head.headers,
      );
      if (synthetic !== null) {
        this.sendSynthetic(synthetic);
        this.finish();
        return;
      }
    }

    const { host, tls } = this.deps.target;
    // T5: a path prefix can route to a different local port; the breaker (T6)
    // and dial both use the resolved port.
    const port = traffic?.routePort(this.head.path) ?? this.deps.target.port;

    // T6: if this upstream's circuit is open, fail fast with 503 instead of
    // eating a full dial timeout on a service known to be down.
    if (traffic?.breakerTripped(port) === true) {
      this.deps.logger.warn(
        `circuit open for upstream :${port}, refusing stream ${this.streamId}`,
      );
      this.sendSynthetic(traffic.breakerResponse(this.head.headers));
      this.finish();
      return;
    }

    const scheme = tls === true ? "https" : "http";
    const url = `${scheme}://${host}:${port}${this.head.path}`;
    const headers = buildRequestHeaders(this.head.headers);
    // T1: rewrite outbound request headers before the fetch.
    traffic?.decorateRequest(headers);
    const init: FetchInit = {
      method: this.head.method,
      headers,
      redirect: "manual",
      signal: this.controller.signal,
      // Forward the upstream body byte-for-byte. Left to itself, Bun's fetch
      // transparently gunzips a `Content-Encoding: gzip`/`br` response but keeps
      // the Content-Encoding and Content-Length headers, which now describe the
      // *compressed* bytes the caller never receives. The browser then tries to
      // decode already-decoded data and fails with ERR_CONTENT_DECODING_FAILED.
      // Disabling decompression also stops Bun from injecting its own
      // Accept-Encoding upstream, so the local service compresses only when the
      // real client asked it to -- exactly what a transparent proxy must do.
      decompress: false,
    };
    if (tls === true) {
      // Dial the local upstream over TLS. A dev HTTPS server is typically
      // self-signed, so verification is skipped when asked (target.insecure);
      // SNI defaults to the target host. A handshake or verify failure surfaces
      // as the same upstream-error RESET as a refused dial (the catch below).
      init.tls = {
        rejectUnauthorized: this.deps.target.insecure !== true,
        serverName: this.deps.target.serverName ?? host,
      };
    }
    if (this.bodyStream !== null) {
      init.body = this.bodyStream;
      init.duplex = "half";
    } else {
      // No body to send: a declared length would describe bytes that never
      // arrive, and fetch would wait for them.
      headers.delete("content-length");
    }

    try {
      const response = await fetch(url, init);
      if (this.aborted) {
        return;
      }
      // The upstream answered: this port's circuit is healthy again (T6).
      traffic?.recordResult(port, true);
      let headers = responseHeaderMap(response.headers);
      // T1/T2: rewrite response headers and add CORS decoration.
      if (traffic !== undefined) {
        headers = traffic.decorateResponse(headers, this.head.headers);
      }
      const resHead: ResponseHead = { status: response.status, headers };
      this.deps.sink.sendJson(FrameType.RES_HEAD, this.streamId, resHead);
      this.headSent = true;

      await this.streamResponseBody(response);
      if (!this.aborted && this.deps.sink.isOpen()) {
        this.deps.sink.send(FrameType.RES_END, this.streamId, EMPTY);
      }
    } catch (err) {
      if (this.aborted) {
        return;
      }
      const message = err instanceof Error ? err.message : String(err);
      // Before RES_HEAD the local service was unreachable (ECONNREFUSED / DNS);
      // after it, the failure is mid-stream and internal to this exchange.
      if (!this.headSent) {
        // A connect-time failure counts against the breaker (T6). A mid-stream
        // failure does not: the service was reachable, so the circuit is fine.
        traffic?.recordResult(port, false);
        this.deps.logger.warn(
          `upstream unreachable for stream ${this.streamId}: ${message}`,
        );
        this.sendReset(ResetCode.UPSTREAM_ERROR, message);
      } else {
        this.deps.logger.warn(
          `upstream error mid-stream ${this.streamId}: ${message}`,
        );
        this.sendReset(ResetCode.INTERNAL, message);
      }
    } finally {
      this.finish();
    }
  }

  private async streamResponseBody(response: Response): Promise<void> {
    const body = response.body;
    if (body === null) {
      return;
    }
    const reader = body.getReader();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) {
          return;
        }
        if (this.aborted) {
          await reader.cancel();
          return;
        }
        if (value !== undefined && value.length > 0) {
          await this.sendChunked(value);
        }
      }
    } finally {
      reader.releaseLock();
    }
  }

  /** Split a body chunk to MAX_PAYLOAD_BYTES frames, honouring backpressure. */
  private async sendChunked(data: Uint8Array): Promise<void> {
    let offset = 0;
    while (offset < data.length) {
      if (this.aborted || !this.deps.sink.isOpen()) {
        return;
      }
      await this.waitForDrain();
      const end = Math.min(offset + MAX_PAYLOAD_BYTES, data.length);
      this.deps.sink.send(
        FrameType.RES_BODY,
        this.streamId,
        data.subarray(offset, end),
      );
      offset = end;
    }
  }

  private async waitForDrain(): Promise<void> {
    while (
      !this.aborted &&
      this.deps.sink.isOpen() &&
      this.deps.sink.bufferedAmount() > BACKPRESSURE_THRESHOLD_BYTES
    ) {
      await sleep(DRAIN_POLL_INTERVAL_MS);
    }
  }

  /**
   * Emit a response the agent produced itself (mock, redirect, CORS preflight,
   * or open-circuit 503) as RES_HEAD [+ RES_BODY] + RES_END, bypassing the
   * upstream fetch entirely.
   */
  private sendSynthetic(res: SyntheticResponse): void {
    if (this.aborted || !this.deps.sink.isOpen()) {
      return;
    }
    const resHead: ResponseHead = { status: res.status, headers: res.headers };
    this.deps.sink.sendJson(FrameType.RES_HEAD, this.streamId, resHead);
    this.headSent = true;
    if (res.body.length > 0) {
      this.deps.sink.send(FrameType.RES_BODY, this.streamId, res.body);
    }
    this.deps.sink.send(FrameType.RES_END, this.streamId, EMPTY);
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

  private failBody(reason: string): void {
    if (this.bodyController !== null && !this.bodyClosed) {
      this.bodyClosed = true;
      this.bodyController.error(new Error(reason));
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
