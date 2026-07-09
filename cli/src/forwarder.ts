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
}

export interface RequestStreamDeps {
  readonly target: ForwardTarget;
  readonly sink: FrameSink;
  readonly logger: Logger;
  /** Called exactly once when the stream is fully retired. */
  readonly onDone: (streamId: bigint) => void;
}

// The Fetch standard requires `duplex: "half"` when the request body is a
// stream; the ambient RequestInit type does not declare it.
interface FetchInit extends RequestInit {
  duplex?: "half";
}

function isHopByHop(name: string): boolean {
  return HOP_BY_HOP_HEADERS.has(name) || name.startsWith("proxy-");
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Build the outbound request headers, dropping hop-by-hop, host, length. */
function buildRequestHeaders(source: HeaderMap): Headers {
  const headers = new Headers();
  for (const [name, values] of Object.entries(source)) {
    const lower = name.toLowerCase();
    // `host` is the public hostname; fetch sets Host from the target URL.
    // `content-length` is dropped because the body is re-framed as a stream.
    if (isHopByHop(lower) || lower === "host" || lower === "content-length") {
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
export class RequestStream {
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
    const url = `http://${this.deps.target.host}:${this.deps.target.port}${this.head.path}`;
    const init: FetchInit = {
      method: this.head.method,
      headers: buildRequestHeaders(this.head.headers),
      redirect: "manual",
      signal: this.controller.signal,
    };
    if (this.bodyStream !== null) {
      init.body = this.bodyStream;
      init.duplex = "half";
    }

    try {
      const response = await fetch(url, init);
      if (this.aborted) {
        return;
      }
      const resHead: ResponseHead = {
        status: response.status,
        headers: responseHeaderMap(response.headers),
      };
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
