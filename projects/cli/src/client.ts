// WebSocket tunnel agent: hello handshake, application heartbeat, stream
// demultiplexing, and reconnection with decorrelated-jitter backoff.
//
// Reconnect policy:
//   - transport loss (socket close/error)         -> reconnect
//   - shutdown{server_shutdown|heartbeat_timeout} -> reconnect
//   - shutdown{token_revoked|replaced}            -> fatal, stop
//   - hello_error (any code)                      -> fatal, stop
// A revoked token or a displaced tunnel will only fail again, so retrying them
// would loop forever; transport loss is expected to be transient.

import { Backoff } from "./backoff.ts";
import type { ResolvedConfig } from "./config.ts";
import {
  ControlType,
  FrameType,
  HelloErrorCode,
  PROTOCOL_DIALER,
  PROTOCOL_VERSION,
  RECONNECT,
  ResetCode,
  ShutdownReason,
  SUBPROTOCOL,
  type SupportedProtocol,
  VERSION,
} from "./constants.ts";
import { type FrameSink, RequestStream, type Stream } from "./forwarder.ts";
import type { Logger } from "./logger.ts";
import type { WirePolicy } from "./policy.ts";
import {
  asHelloError,
  asHelloOk,
  asRequestHead,
  asShutdown,
  asStreamReset,
  type ControlEnvelope,
  decodeControl,
  decodeFrame,
  decodeJson,
  encodeControl,
  encodeFrame,
  encodeJsonFrame,
  type Frame,
  type Heartbeat,
  type Hello,
  isKnownFrameType,
} from "./protocol.ts";
import { formatRetryDelay, type SessionInfo } from "./ui.ts";
import { UpgradeStream } from "./upgrade.ts";

export interface ClientOptions {
  readonly config: ResolvedConfig;
  readonly protocol: SupportedProtocol;
  readonly port: number;
  readonly subdomain?: string;
  readonly logger: Logger;
  /** Visitor-access policy declared to the gateway in the Hello (A2-A5). */
  readonly policy?: WirePolicy;
}

/** Raised for non-recoverable client failures (surfaced as a nonzero exit). */
export class ClientError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ClientError";
  }
}

/** Hosts treated as loopback for the upstream-TLS verification default. */
const LOOPBACK_HOSTS: ReadonlySet<string> = new Set([
  "127.0.0.1",
  "::1",
  "localhost",
]);

function isLoopbackHost(host: string): boolean {
  return LOOPBACK_HOSTS.has(host.toLowerCase());
}

export class TunnelClient {
  private readonly config: ResolvedConfig;
  private readonly policy: WirePolicy | undefined;
  private readonly logger: Logger;
  private readonly port: number;
  private readonly protocol: SupportedProtocol;
  /** Subdomain to request; updated to the gateway-assigned one after hello_ok. */
  private subdomain: string | undefined;
  /** Whether the user asked for a specific subdomain (vs. an auto-generated one). */
  private readonly requestedByUser: boolean;

  private ws: WebSocket | null = null;
  private readonly streams = new Map<bigint, Stream>();
  private heartbeatTimer: ReturnType<typeof setInterval> | null = null;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectAttempts = 0;
  private readonly backoff = new Backoff({
    baseMs: RECONNECT.BASE_MS,
    capMs: RECONNECT.CAP_MS,
  });
  /** True once the first hello_ok lands, so reconnects don't reprint the banner. */
  private established = false;
  /** Total requests proxied since start; surfaced to the live request counter. */
  private totalRequests = 0;

  private stopped = false;
  private fatal: Error | null = null;
  private settled = false;
  private done: { resolve: () => void; reject: (err: Error) => void } | null =
    null;

  private readonly sink: FrameSink;

  constructor(opts: ClientOptions) {
    this.config = opts.config;
    this.policy = opts.policy;
    this.logger = opts.logger;
    this.port = opts.port;
    this.protocol = opts.protocol;
    this.subdomain = opts.subdomain;
    this.requestedByUser = opts.subdomain !== undefined;
    this.sink = {
      send: (type, streamId, payload) =>
        this.sendRaw(encodeFrame(type, streamId, payload)),
      sendJson: (type, streamId, payload) =>
        this.sendRaw(encodeJsonFrame(type, streamId, payload)),
      bufferedAmount: () => this.ws?.bufferedAmount ?? 0,
      isOpen: () => this.ws !== null && this.ws.readyState === WebSocket.OPEN,
    };
    // Expose live request tallies to the dashboard without the forwarder or the
    // stream map needing to know a UI exists: total is a plain counter, open is
    // the current in-flight stream count.
    this.logger.metrics?.(() => ({
      total: this.totalRequests,
      open: this.streams.size,
    }));
  }

  /** Run until a graceful stop (resolves) or a fatal error (rejects). */
  run(): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      this.done = { resolve, reject };
      this.connect();
    });
  }

  /** Begin a graceful shutdown: stop reconnecting and close the socket. */
  stop(): void {
    if (this.stopped) {
      return;
    }
    this.stopped = true;
    this.clearReconnectTimer();
    this.stopHeartbeat();
    const ws = this.ws;
    if (
      ws !== null &&
      (ws.readyState === WebSocket.OPEN ||
        ws.readyState === WebSocket.CONNECTING)
    ) {
      ws.close(1000, "client shutdown");
    } else {
      this.finishRun();
    }
  }

  private connect(): void {
    if (this.stopped || this.fatal !== null) {
      return;
    }
    const options: Bun.WebSocketOptions = { protocols: [SUBPROTOCOL] };
    if (this.config.insecure) {
      options.tls = { rejectUnauthorized: false };
    }
    // The first attempt shows "connecting"; a retry keeps the "reconnecting"
    // state set by scheduleReconnect so the spinner reads correctly.
    if (!this.established && this.reconnectAttempts === 0) {
      this.logger.status?.("connecting");
    }
    this.logger.info(`connecting to ${this.config.server}`);

    let ws: WebSocket;
    try {
      ws = new WebSocket(this.config.server, options);
    } catch (err) {
      // A malformed server URL fails synchronously; treat as fatal.
      this.fail(
        new ClientError(
          `cannot connect to ${this.config.server}: ${err instanceof Error ? err.message : String(err)}`,
        ),
      );
      return;
    }
    ws.binaryType = "arraybuffer";
    this.ws = ws;

    ws.addEventListener("open", () => this.onOpen());
    ws.addEventListener("message", (event) => this.onMessage(event));
    ws.addEventListener("error", () => {
      // Detail arrives via the following close event; log for visibility.
      this.logger.debug("websocket error");
    });
    ws.addEventListener("close", (event) =>
      this.onClose(event.code, event.reason),
    );
  }

  private onOpen(): void {
    this.logger.debug("websocket open; sending hello");
    const hello: Hello = {
      protocol_version: PROTOCOL_VERSION,
      token: this.config.token,
      // Send the wire protocol, not the CLI keyword: an `https` tunnel is an
      // `http` tunnel to the gateway, differing only in the local upstream
      // scheme (see PROTOCOL_DIALER). this.protocol is kept for local display.
      protocol: PROTOCOL_DIALER[this.protocol].wire,
    };
    if (this.subdomain !== undefined) {
      hello.subdomain = this.subdomain;
    }
    hello.local_port = this.port;
    hello.client_version = VERSION;
    if (this.policy !== undefined) {
      hello.policy = this.policy;
    }
    this.sendRaw(encodeControl(ControlType.HELLO, hello));
  }

  private onMessage(event: MessageEvent): void {
    const raw: unknown = event.data;
    if (!(raw instanceof ArrayBuffer)) {
      // The protocol is binary-only; a text message is a violation.
      this.logger.warn("ignoring non-binary websocket message");
      return;
    }
    let frame: Frame;
    try {
      frame = decodeFrame(new Uint8Array(raw));
    } catch (err) {
      this.logger.warn(
        `dropping malformed frame: ${err instanceof Error ? err.message : String(err)}`,
      );
      return;
    }
    if (!isKnownFrameType(frame.type)) {
      this.logger.debug(
        `ignoring unknown frame type 0x${frame.type.toString(16)}`,
      );
      return;
    }

    switch (frame.type) {
      case FrameType.CONTROL:
        this.handleControl(frame.payload);
        return;
      case FrameType.REQ_HEAD:
        this.handleReqHead(frame.streamId, frame.payload);
        return;
      case FrameType.REQ_BODY:
        this.streams.get(frame.streamId)?.pushBody(frame.payload);
        return;
      case FrameType.REQ_END:
        this.streams.get(frame.streamId)?.endBody();
        return;
      case FrameType.RESET:
        this.handleReset(frame.streamId, frame.payload);
        return;
      default:
        // RES_* frames are agent->gateway only; ignore if echoed back.
        this.logger.debug(
          `ignoring unexpected frame type 0x${frame.type.toString(16)}`,
        );
        return;
    }
  }

  private handleControl(payload: Uint8Array): void {
    let envelope: ControlEnvelope;
    try {
      envelope = decodeControl(payload);
    } catch (err) {
      this.logger.warn(
        `dropping malformed control frame: ${err instanceof Error ? err.message : String(err)}`,
      );
      return;
    }
    switch (envelope.type) {
      case ControlType.HELLO_OK:
        this.handleHelloOk(envelope.payload);
        return;
      case ControlType.HELLO_ERROR:
        this.handleHelloError(envelope.payload);
        return;
      case ControlType.PONG:
        this.logger.debug("pong");
        return;
      case ControlType.SHUTDOWN:
        this.handleShutdown(envelope.payload);
        return;
      case ControlType.PING:
        // The gateway drives pong; a ping from it is unexpected but harmless.
        this.logger.debug("received ping from gateway");
        return;
      default:
        this.logger.debug(`ignoring control type ${envelope.type}`);
        return;
    }
  }

  private handleHelloOk(payload: unknown): void {
    const ok = asHelloOk(payload);
    if (ok === null) {
      this.fail(new ClientError("gateway sent a malformed hello_ok"));
      return;
    }
    this.reconnectAttempts = 0;
    this.backoff.reset();
    // Keep the assigned subdomain so reconnects reclaim the same URL.
    this.subdomain = ok.subdomain;
    // A raw tunnel (tcp/tls) is reached at a host:port, not the http URL.
    const publicAddr =
      ok.bind_addr !== undefined
        ? `${this.protocol}://${ok.bind_addr}`
        : ok.url;
    // http and https both proxy HTTP over the tunnel; the scheme shown reflects
    // the local upstream, so https reads `https://host:port`. A raw tunnel
    // (tcp/tls) has no scheme and is shown as a bare host:port.
    const localAddr =
      this.protocol === "http" || this.protocol === "https"
        ? `${this.protocol}://${this.config.host}:${this.port}`
        : `${this.config.host}:${this.port}`;
    if (!this.established) {
      this.established = true;
      const session: SessionInfo = {
        version: VERSION,
        url: publicAddr,
        forwardTo: localAddr,
        gateway: this.gatewayHost(),
        tunnelId: ok.tunnel_id,
      };
      this.logger.session?.(session);
      // session() only paints the header panel; emit a scrolling log line too so
      // the event log shows the connection landing (mirrors `reconnected` below).
      this.logger.info(`connected: ${publicAddr}`);
    } else {
      this.logger.info(`reconnected: ${publicAddr}`);
    }
    this.logger.status?.("online");
    // The gateway may speak a newer protocol than this build; the handshake
    // still succeeded (we are within its supported range), but flag it once so
    // an out-of-date agent knows an upgrade exists.
    if (
      ok.protocol_version !== undefined &&
      ok.protocol_version > PROTOCOL_VERSION
    ) {
      this.logger.warn(
        `the gateway speaks protocol v${ok.protocol_version}; this rift speaks v${PROTOCOL_VERSION} — a newer rift may be available`,
      );
    }
    this.startHeartbeat(ok.heartbeat_interval_ms);
  }

  /** Human-facing gateway host derived from the server URL (best effort). */
  private gatewayHost(): string {
    try {
      return new URL(this.config.server).host;
    } catch {
      return this.config.server;
    }
  }

  /**
   * Whether to skip certificate verification when dialing a local HTTPS
   * upstream. The explicit flag forces it; otherwise a loopback upstream is
   * trusted without verification (a self-signed cert is the norm for a local
   * dev server), while a non-loopback upstream is verified against a real cert.
   * This is entirely separate from config.insecure, which governs the gateway
   * wss connection only.
   */
  private resolveUpstreamInsecure(): boolean {
    return this.config.upstreamInsecure || isLoopbackHost(this.config.host);
  }

  private handleHelloError(payload: unknown): void {
    const err = asHelloError(payload);
    // A version gap cannot be retried away and is not the user's config fault;
    // surface the gateway's guidance (which says which side to upgrade) plainly.
    if (err?.code === HelloErrorCode.UNSUPPORTED_VERSION) {
      this.fail(
        new ClientError(`incompatible with the gateway: ${err.message}`),
      );
      return;
    }
    // An auto-generated subdomain that another client claimed during a
    // disconnect is recoverable: drop it so the reconnect is issued a fresh one.
    // A subdomain the user explicitly asked for is not -- failing tells them it
    // is taken rather than silently moving them to a different URL.
    if (
      !this.requestedByUser &&
      this.subdomain !== undefined &&
      (err?.code === HelloErrorCode.SUBDOMAIN_TAKEN ||
        err?.code === HelloErrorCode.SUBDOMAIN_RESERVED)
    ) {
      this.logger.warn(
        `subdomain ${this.subdomain} is no longer available; requesting a new one`,
      );
      // Clearing it makes the next hello omit the subdomain, so the gateway
      // generates a fresh one. The socket closes after hello_error, which
      // triggers the normal reconnect path.
      this.subdomain = undefined;
      return;
    }
    const detail =
      err !== null ? `${err.code}: ${err.message}` : "unknown reason";
    // Any other handshake rejection is a configuration problem that a retry
    // cannot fix (bad token, taken/invalid subdomain, unsupported protocol).
    this.fail(new ClientError(`handshake rejected: ${detail}`));
  }

  private handleShutdown(payload: unknown): void {
    const shutdown = asShutdown(payload);
    const reason = shutdown?.reason ?? "unknown";
    if (
      reason === ShutdownReason.TOKEN_REVOKED ||
      reason === ShutdownReason.REPLACED
    ) {
      this.fail(new ClientError(`gateway shut down tunnel: ${reason}`));
      return;
    }
    // server_shutdown / heartbeat_timeout / unknown: transient, reconnect.
    this.logger.warn(`gateway shutdown (${reason}); will reconnect`);
  }

  private handleReqHead(streamId: bigint, payload: Uint8Array): void {
    let parsed: unknown;
    try {
      parsed = decodeJson(payload);
    } catch (err) {
      this.logger.warn(
        `dropping REQ_HEAD with bad JSON on stream ${streamId}: ${err instanceof Error ? err.message : String(err)}`,
      );
      return;
    }
    const head = asRequestHead(parsed);
    if (head === null) {
      this.logger.warn(`dropping malformed REQ_HEAD on stream ${streamId}`);
      this.sink.sendJson(FrameType.RESET, streamId, {
        code: ResetCode.INTERNAL,
        message: "malformed request head",
      });
      return;
    }
    // Stream IDs are unique per connection; a collision means a protocol bug.
    const existing = this.streams.get(streamId);
    if (existing !== undefined) {
      existing.reset(ResetCode.INTERNAL);
      this.streams.delete(streamId);
    }
    this.totalRequests++;
    const deps = {
      target: {
        host: this.config.host,
        port: this.port,
        // `https` dials the local upstream over TLS; `http` (and raw tunnels)
        // do not. See resolveUpstreamInsecure for the verification policy.
        tls: PROTOCOL_DIALER[this.protocol].upstreamTls,
        insecure: this.resolveUpstreamInsecure(),
      },
      sink: this.sink,
      logger: this.logger,
      onDone: (id: bigint) => {
        this.streams.delete(id);
      },
    };
    let stream: Stream;
    if (head.raw) {
      this.logger.debug(`REQ_HEAD raw stream ${streamId}`);
      stream = new UpgradeStream(streamId, head, deps);
    } else if (head.upgrade) {
      this.logger.debug(
        `REQ_HEAD upgrade ${head.method} ${head.path} stream ${streamId}`,
      );
      stream = new UpgradeStream(streamId, head, deps);
    } else {
      this.logger.debug(
        `REQ_HEAD ${head.method} ${head.path} stream ${streamId}`,
      );
      stream = new RequestStream(streamId, head, deps);
    }
    this.streams.set(streamId, stream);
  }

  private handleReset(streamId: bigint, payload: Uint8Array): void {
    let code: string = ResetCode.CANCELED;
    try {
      const reset = asStreamReset(decodeJson(payload));
      if (reset !== null) {
        code = reset.code;
      }
    } catch {
      // Fall back to a generic reset code on a malformed payload.
    }
    const stream = this.streams.get(streamId);
    if (stream !== undefined) {
      stream.reset(code);
      this.streams.delete(streamId);
    }
  }

  private startHeartbeat(intervalMs: number): void {
    this.stopHeartbeat();
    const interval = intervalMs > 0 ? intervalMs : RECONNECT.CAP_MS;
    this.heartbeatTimer = setInterval(() => {
      const beat: Heartbeat = { ts: Date.now() };
      this.sendRaw(encodeControl(ControlType.PING, beat));
    }, interval);
  }

  private stopHeartbeat(): void {
    if (this.heartbeatTimer !== null) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
  }

  private onClose(code: number, reason: string): void {
    if (this.settled) {
      return;
    }
    this.stopHeartbeat();
    this.resetAllStreams();
    this.ws = null;

    if (this.fatal !== null) {
      this.finishRun(this.fatal);
      return;
    }
    if (this.stopped) {
      this.finishRun();
      return;
    }
    this.scheduleReconnect(code, reason);
  }

  private scheduleReconnect(code: number, reason: string): void {
    const delay = this.backoff.next();
    this.reconnectAttempts++;
    const why = reason !== "" ? ` ${reason}` : "";
    this.logger.status?.("reconnecting", `retry in ${formatRetryDelay(delay)}`);
    this.logger.warn(
      `connection closed (code ${code}${why}); reconnecting in ${delay}ms ` +
        `(attempt ${this.reconnectAttempts})`,
    );
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  private resetAllStreams(): void {
    for (const stream of this.streams.values()) {
      stream.reset(ResetCode.CLIENT_DISCONNECTED);
    }
    this.streams.clear();
  }

  private sendRaw(bytes: Uint8Array): void {
    const ws = this.ws;
    if (ws !== null && ws.readyState === WebSocket.OPEN) {
      ws.send(bytes);
    } else {
      this.logger.debug("dropping frame: socket not open");
    }
  }

  /** Record a fatal error and tear the socket down; run() will reject. */
  private fail(err: Error): void {
    if (this.fatal !== null) {
      return;
    }
    this.fatal = err;
    this.clearReconnectTimer();
    this.stopHeartbeat();
    const ws = this.ws;
    if (
      ws !== null &&
      (ws.readyState === WebSocket.OPEN ||
        ws.readyState === WebSocket.CONNECTING)
    ) {
      ws.close(1000, "client fatal");
    } else {
      this.finishRun(err);
    }
  }

  private finishRun(err?: Error): void {
    if (this.settled) {
      return;
    }
    this.settled = true;
    this.clearReconnectTimer();
    this.stopHeartbeat();
    this.resetAllStreams();
    const done = this.done;
    if (done === null) {
      return;
    }
    if (err !== undefined) {
      done.reject(err);
    } else {
      done.resolve();
    }
  }

  private clearReconnectTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }
}
