// Per-stream HTTP forwarding. For each REQ_HEAD the gateway opens, one
// RequestStream issues a streaming fetch to the local service and streams the
// response back as RES_HEAD / RES_BODY* / RES_END frames, or a RESET on error.

import { formatAuthority } from "./config.ts";
import {
  DRAIN_POLL_INTERVAL_MS,
  FrameType,
  HOP_BY_HOP_HEADERS,
  MAX_STREAM_BUFFER_BYTES,
  ResetCode,
  type ResetCodeValue,
} from "./constants.ts";
import { errorMessage } from "./logger.ts";
import {
  appendHeader,
  type HeaderMap,
  newHeaderMap,
  type RequestHead,
  type ResponseHead,
  requestTargetProblem,
} from "./protocol.ts";
import {
  EMPTY_BYTES,
  linkBackedUp,
  payloadSlices,
  type Stream,
  type StreamDeps,
  sendStreamEnd,
  sendStreamReset,
} from "./stream.ts";
import type { SyntheticResponse, TrafficController } from "./traffic.ts";

export interface RequestStreamDeps extends StreamDeps {
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

/**
 * Methods whose fetch() may not carry a body. The Fetch standard forbids one on
 * GET and HEAD and Bun's fetch also throws for OPTIONS, which would turn an
 * otherwise-valid request into a 502. RFC 9110 gives such a body no defined
 * semantics, so it is drained and dropped instead.
 */
const BODYLESS_METHODS: ReadonlySet<string> = new Set([
  "GET",
  "HEAD",
  "OPTIONS",
]);

/**
 * The local URL a request is fetched from. The target is parsed against a base
 * built only from the configured host and port, then the result's scheme and
 * authority are asserted unchanged: the path comes from the gateway, and a
 * target that re-parsed into another host or port (userinfo, "//host",
 * backslash tricks) must never reach fetch. asRequestHead already rejects such
 * paths; this is the independent second check at the point of use.
 */
export function buildUpstreamUrl(
  host: string,
  port: number,
  tls: boolean,
  path: string,
): URL {
  const scheme = tls ? "https" : "http";
  const base = new URL(`${scheme}://${formatAuthority(host, port)}/`);
  const problem = requestTargetProblem("GET", path === "*" ? "/" : path);
  if (problem !== null) {
    throw new Error(`refusing request target: ${problem}`);
  }
  // fetch cannot express asterisk-form ("OPTIONS *"); "/" is the closest
  // server-wide target it can send.
  const url = new URL(path === "*" ? "/" : path, base);
  if (url.protocol !== base.protocol || url.host !== base.host) {
    throw new Error(
      `refusing request target: it resolves outside ${base.host}`,
    );
  }
  return url;
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
  const out = newHeaderMap();
  headers.forEach((value, name) => {
    const lower = name.toLowerCase();
    // set-cookie must not be comma-joined; collected separately below.
    if (lower !== "set-cookie" && !isHopByHop(lower)) {
      appendHeader(out, lower, value);
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
  /** A body on a method fetch cannot send it with; drained and discarded. */
  private readonly dropBody: boolean;
  private aborted = false;
  private finished = false;
  private bodyClosed = false;
  private headSent = false;

  constructor(
    private readonly streamId: bigint,
    private readonly head: RequestHead,
    private readonly deps: RequestStreamDeps,
  ) {
    this.dropBody =
      head.has_body && BODYLESS_METHODS.has(head.method.toUpperCase());
    if (head.has_body && !this.dropBody) {
      // Queue by byte length so desiredSize tracks how much of the body fetch
      // has not consumed yet; pushBody resets the stream past the cap.
      this.bodyStream = new ReadableStream<Uint8Array>(
        {
          start: (controller) => {
            this.bodyController = controller;
          },
          // fetch cancels the body it is sending when the exchange fails or is
          // aborted; enqueue() or close() on a cancelled stream throws, so
          // stop feeding it rather than let a late REQ_BODY throw.
          cancel: () => {
            this.bodyClosed = true;
          },
        },
        {
          highWaterMark: MAX_STREAM_BUFFER_BYTES,
          size: (chunk) => chunk?.byteLength ?? 0,
        },
      );
    } else {
      if (this.dropBody) {
        this.deps.logger.debug(
          `dropping request body on ${head.method} stream ${streamId}: fetch cannot send one`,
        );
      }
      this.bodyStream = null;
    }
    this.run().catch((err: unknown) => {
      // Last line of defence: an unexpected throw (a traffic hook, the sink)
      // fails this one stream instead of escaping as an unhandled rejection,
      // which would take the whole agent down.
      this.abortLocal(ResetCode.INTERNAL, errorMessage(err));
      this.finish();
    });
  }

  /** Feed a REQ_BODY chunk into the upstream request body. */
  pushBody(chunk: Uint8Array): void {
    if (this.bodyController === null || this.bodyClosed || this.aborted) {
      // Includes a dropped GET/HEAD/OPTIONS body: the bytes are discarded.
      return;
    }
    this.bodyController.enqueue(chunk);
    // The gateway cannot be asked to slow down, so a local service reading
    // slower than the public client uploads backs bytes up here. Bound it.
    if ((this.bodyController.desiredSize ?? 0) < 0) {
      this.abortLocal(
        ResetCode.PAYLOAD_TOO_LARGE,
        `request body backlog exceeded ${MAX_STREAM_BUFFER_BYTES} bytes`,
      );
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
    this.deps.logger.debug(`stream ${this.streamId} reset: ${code}`);
    this.controller.abort();
    this.failBody();
  }

  /** Abort the exchange from this side, telling the gateway why. */
  private abortLocal(code: ResetCodeValue, message: string): void {
    if (this.aborted) {
      return;
    }
    this.deps.logger.warn(`resetting stream ${this.streamId}: ${message}`);
    this.sendReset(code, message);
    this.aborted = true;
    this.controller.abort();
    this.failBody();
    this.finish();
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

    let url: URL;
    let headers: Headers;
    try {
      url = buildUpstreamUrl(host, port, tls === true, this.head.path);
      headers = buildRequestHeaders(this.head.headers);
      // T1: rewrite outbound request headers before the fetch.
      traffic?.decorateRequest(headers);
    } catch (err) {
      // An unforwardable head (see buildUpstreamUrl) or a header value the
      // Headers class rejects: fail this one stream, never the agent.
      const message = errorMessage(err);
      this.deps.logger.warn(
        `cannot forward stream ${this.streamId}: ${message}`,
      );
      this.sendReset(ResetCode.INTERNAL, message);
      this.finish();
      return;
    }
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
      if (!this.aborted) {
        sendStreamEnd(this.deps.sink, this.streamId);
      }
    } catch (err) {
      if (this.aborted) {
        return;
      }
      const message = errorMessage(err);
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
        if (this.aborted || !this.deps.sink.isOpen()) {
          // Nobody is left to receive the rest: stop pulling the upstream body
          // rather than draining it into the void.
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
    for (const part of payloadSlices(data)) {
      await this.waitForDrain();
      if (this.aborted || !this.deps.sink.isOpen()) {
        return;
      }
      this.deps.sink.send(FrameType.RES_BODY, this.streamId, part);
    }
  }

  private async waitForDrain(): Promise<void> {
    while (
      !this.aborted &&
      this.deps.sink.isOpen() &&
      linkBackedUp(this.deps.sink)
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
    // A mock body can exceed one frame (a project file is not bound by argv
    // limits); an oversized frame would throw instead of being sent.
    for (const part of payloadSlices(res.body)) {
      this.deps.sink.send(FrameType.RES_BODY, this.streamId, part);
    }
    this.deps.sink.send(FrameType.RES_END, this.streamId, EMPTY_BYTES);
  }

  private sendReset(code: ResetCodeValue, message: string): void {
    sendStreamReset(this.deps.sink, this.streamId, code, message);
  }

  /**
   * Stop feeding the upstream request body. Call it only AFTER aborting the
   * fetch: the abort cancels the body stream fetch is reading, which releases
   * the queued chunks. Erroring that stream instead (controller.error) makes
   * Bun's fetch body pump reject with nobody awaiting it -- from Bun 1.3.14 an
   * unhandled rejection, which would take the whole agent down with it.
   */
  private failBody(): void {
    if (this.bodyController !== null && !this.bodyClosed) {
      this.bodyClosed = true;
      try {
        this.bodyController.close();
      } catch {
        // Already cancelled by the aborted fetch: nothing left to release.
      }
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
