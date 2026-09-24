// Wire protocol v1: frame encode/decode plus control-message types and guards.
//
// This module mirrors server/internal/tunnelproto (frame.go, control.go)
// byte-for-byte. Frames are big-endian: type(u8) | stream_id(u64) |
// length(u32) | payload. See docs/PROTOCOL.md.

import {
  CONTROL_STREAM_ID,
  ControlType,
  type ControlTypeValue,
  FrameType,
  HEADER_SIZE,
  KNOWN_FRAME_TYPES,
  MAX_FRAME_BYTES,
  MAX_PAYLOAD_BYTES,
} from "./constants.ts";
import type { WirePolicy } from "./policy.ts";

const UINT64_MAX = 0xffff_ffff_ffff_ffffn;

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

export type FrameErrorCode =
  | "short_frame"
  | "payload_too_large"
  | "length_mismatch"
  | "control_stream"
  | "data_stream"
  | "invalid_stream_id";

/** Thrown by frame encode/decode. `code` mirrors the Go sentinel errors. */
export class FrameError extends Error {
  readonly code: FrameErrorCode;
  constructor(code: FrameErrorCode, message: string) {
    super(message);
    this.name = "FrameError";
    this.code = code;
  }
}

/** A decoded wire frame. `payload` aliases the decoded buffer. */
export interface Frame {
  /** Raw frame type byte; may be a value this version does not know. */
  readonly type: number;
  // uint64 on the wire; see constants.ts for why this is a bigint, not number.
  readonly streamId: bigint;
  readonly payload: Uint8Array;
}

/** True for frame types this protocol version understands. */
export function isKnownFrameType(type: number): boolean {
  return KNOWN_FRAME_TYPES.has(type);
}

function checkStreamID(type: number, streamID: bigint): void {
  if (type === FrameType.CONTROL) {
    if (streamID !== CONTROL_STREAM_ID) {
      throw new FrameError("control_stream", "control frame must use stream 0");
    }
    return;
  }
  if (streamID === CONTROL_STREAM_ID) {
    throw new FrameError("data_stream", "data frame must not use stream 0");
  }
}

/** Serialise one frame into a freshly allocated buffer. */
export function encodeFrame(
  type: number,
  streamID: bigint,
  payload: Uint8Array,
): Uint8Array {
  if (payload.length > MAX_PAYLOAD_BYTES) {
    throw new FrameError(
      "payload_too_large",
      `payload ${payload.length} > ${MAX_PAYLOAD_BYTES}`,
    );
  }
  if (streamID < 0n || streamID > UINT64_MAX) {
    throw new FrameError(
      "invalid_stream_id",
      `stream_id ${streamID} out of uint64 range`,
    );
  }
  // Stream discipline is enforced for known types only; unknown types are for
  // forward compatibility and carry no discipline we can assert.
  if (isKnownFrameType(type)) {
    checkStreamID(type, streamID);
  }
  const buf = new Uint8Array(HEADER_SIZE + payload.length);
  const view = new DataView(buf.buffer);
  view.setUint8(0, type);
  view.setBigUint64(1, streamID, false);
  view.setUint32(9, payload.length, false);
  buf.set(payload, HEADER_SIZE);
  return buf;
}

/** Parse exactly one whole frame. The returned payload aliases `buf`. */
export function decodeFrame(buf: Uint8Array): Frame {
  if (buf.length < HEADER_SIZE) {
    throw new FrameError(
      "short_frame",
      `frame shorter than header: got ${buf.length} bytes`,
    );
  }
  if (buf.length > MAX_FRAME_BYTES) {
    throw new FrameError(
      "payload_too_large",
      `frame ${buf.length} > ${MAX_FRAME_BYTES}`,
    );
  }
  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const type = view.getUint8(0);
  const streamID = view.getBigUint64(1, false);
  const declared = view.getUint32(9, false);
  const body = buf.subarray(HEADER_SIZE);
  if (body.length !== declared) {
    throw new FrameError(
      "length_mismatch",
      `declared ${declared}, got ${body.length}`,
    );
  }
  if (isKnownFrameType(type)) {
    checkStreamID(type, streamID);
  }
  return { type, streamId: streamID, payload: body };
}

// ---------------------------------------------------------------------------
// Control messages (tunnelproto control.go).
// ---------------------------------------------------------------------------

/** Header maps preserve repeated headers; names are lowercased. */
export type HeaderMap = Record<string, string[]>;

/**
 * An empty header map with no prototype. Header names come from the network,
 * and indexing a plain `{}` with "constructor" or "__proto__" yields an Object
 * builtin rather than undefined (and assigning "__proto__" rewires the
 * prototype), which breaks the append-or-create pattern every builder uses.
 */
export function newHeaderMap(): HeaderMap {
  return Object.create(null) as HeaderMap;
}

export interface Hello {
  protocol_version: number;
  token: string;
  protocol: string;
  subdomain?: string;
  local_port?: number;
  client_version?: string;
  /** Optional visitor-access policy; omitted when unset (mirrors server). */
  policy?: WirePolicy;
  /** Optional BYO custom domains to route to this tunnel (E1). */
  domains?: readonly string[];
}

export interface HelloOk {
  tunnel_id: string;
  subdomain: string;
  hostname: string;
  url: string;
  heartbeat_interval_ms: number;
  /** Public host:port for a raw tunnel (tcp/tls); absent for http. */
  bind_addr?: string;
  /** The gateway's current protocol version, so the agent can warn if behind. */
  protocol_version?: number;
}

export interface HelloError {
  code: string;
  message: string;
}

export interface Heartbeat {
  ts: number;
}

export interface Shutdown {
  reason: string;
}

export interface RequestHead {
  method: string;
  path: string;
  headers: HeaderMap;
  host: string;
  scheme: string;
  remote_addr: string;
  has_body: boolean;
  /** A connection-upgrade request (WebSocket etc.); see server tunnelproto. */
  upgrade: boolean;
  /** A raw byte-pipe stream (tcp/tls tunnel) with no HTTP semantics. */
  raw: boolean;
}

export interface ResponseHead {
  status: number;
  headers: HeaderMap;
}

export interface StreamReset {
  code: string;
  message?: string;
}

/** Envelope wrapping every control message. `payload` is the parsed body. */
export interface ControlEnvelope {
  type: string;
  payload?: unknown;
}

/** Build a CONTROL frame carrying `type` and an optional JSON payload. */
export function encodeControl(
  type: ControlTypeValue,
  payload?: unknown,
): Uint8Array {
  // Field order (type, then payload) and payload omission mirror Go's
  // json.Marshal of ControlEnvelope with `payload,omitempty`.
  const envelope: { type: string; payload?: unknown } = { type };
  if (payload !== undefined) {
    envelope.payload = payload;
  }
  const body = textEncoder.encode(JSON.stringify(envelope));
  return encodeFrame(FrameType.CONTROL, CONTROL_STREAM_ID, body);
}

/** Parse a CONTROL frame payload into its envelope. Throws on invalid JSON. */
export function decodeControl(payload: Uint8Array): ControlEnvelope {
  const parsed: unknown = JSON.parse(textDecoder.decode(payload));
  if (
    !isRecord(parsed) ||
    typeof parsed.type !== "string" ||
    parsed.type === ""
  ) {
    throw new FrameError("length_mismatch", "control envelope missing type");
  }
  const envelope: ControlEnvelope = { type: parsed.type };
  if ("payload" in parsed && parsed.payload !== undefined) {
    envelope.payload = parsed.payload;
  }
  return envelope;
}

/** Build a non-control frame whose payload is JSON (RES_HEAD, RESET, ...). */
export function encodeJsonFrame(
  type: number,
  streamID: bigint,
  payload: unknown,
): Uint8Array {
  const body = textEncoder.encode(JSON.stringify(payload));
  return encodeFrame(type, streamID, body);
}

/** Decode a JSON frame payload (REQ_HEAD, RESET, ...) into an unknown value. */
export function decodeJson(payload: Uint8Array): unknown {
  return JSON.parse(textDecoder.decode(payload));
}

// ---------------------------------------------------------------------------
// Type guards. Every inbound control payload is validated before use so the
// rest of the codebase never touches an `any`.
// ---------------------------------------------------------------------------

export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === "string");
}

export function isHeaderMap(v: unknown): v is HeaderMap {
  if (!isRecord(v)) {
    return false;
  }
  for (const value of Object.values(v)) {
    if (!isStringArray(value)) {
      return false;
    }
  }
  return true;
}

export function asHelloOk(v: unknown): HelloOk | null {
  if (
    isRecord(v) &&
    typeof v.tunnel_id === "string" &&
    typeof v.subdomain === "string" &&
    typeof v.hostname === "string" &&
    typeof v.url === "string" &&
    typeof v.heartbeat_interval_ms === "number"
  ) {
    const ok: HelloOk = {
      tunnel_id: v.tunnel_id,
      subdomain: v.subdomain,
      hostname: v.hostname,
      url: v.url,
      heartbeat_interval_ms: v.heartbeat_interval_ms,
    };
    if (typeof v.bind_addr === "string" && v.bind_addr !== "") {
      ok.bind_addr = v.bind_addr;
    }
    if (typeof v.protocol_version === "number") {
      ok.protocol_version = v.protocol_version;
    }
    return ok;
  }
  return null;
}

export function asHelloError(v: unknown): HelloError | null {
  if (isRecord(v) && typeof v.code === "string") {
    return {
      code: v.code,
      message: typeof v.message === "string" ? v.message : "",
    };
  }
  return null;
}

export function asHeartbeat(v: unknown): Heartbeat | null {
  if (isRecord(v) && typeof v.ts === "number") {
    return { ts: v.ts };
  }
  return null;
}

export function asShutdown(v: unknown): Shutdown | null {
  if (isRecord(v) && typeof v.reason === "string") {
    return { reason: v.reason };
  }
  return null;
}

// ---------------------------------------------------------------------------
// Request-head validation. The gateway is a remote peer: everything in a
// REQ_HEAD is untrusted input that the agent splices into a local URL or a raw
// HTTP/1.1 request line. Left unchecked, a path of "@169.254.169.254/x" turns
// `http://127.0.0.1:3000` + path into userinfo and retargets the fetch at an
// arbitrary host, "1/x" changes the port, and a CR/LF in a method or header
// smuggles extra request lines to the local service.
// ---------------------------------------------------------------------------

// RFC 7230 §3.2.6 token: the grammar of both a method and a header field name.
const HTTP_TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
// Characters never legal in an origin-form request target: C0 controls, space,
// DEL, and backslash (which WHATWG URL parsing treats as "/", so "/\evil" would
// become the authority "evil").
// biome-ignore lint/suspicious/noControlCharactersInRegex: rejecting control characters is the point.
const UNSAFE_TARGET_CHARS = /[\x00-\x20\x7f\\]/;
// Field values may not carry a line break or NUL (RFC 7230 §3.2); everything
// else, including obs-text, is passed through as the gateway sent it.
// biome-ignore lint/suspicious/noControlCharactersInRegex: rejecting control characters is the point.
const UNSAFE_FIELD_VALUE_CHARS = /[\r\n\x00]/;

/** True for an RFC 7230 token (a method or a header field name). */
export function isHttpToken(s: string): boolean {
  return HTTP_TOKEN.test(s);
}

/**
 * Why a method + request target cannot be forwarded, or null if it is safe.
 * Only origin-form ("/path?query") is accepted, plus asterisk-form ("*") for
 * OPTIONS; absolute-form and authority-form never reach an agent, and a target
 * starting with "//" would parse as a scheme-relative URL naming another host.
 */
export function requestTargetProblem(
  method: string,
  path: string,
): string | null {
  if (!isHttpToken(method)) {
    return "method is not an HTTP token";
  }
  if (path === "*") {
    return method === "OPTIONS"
      ? null
      : "asterisk-form target is only valid for OPTIONS";
  }
  if (!path.startsWith("/")) {
    return "request target must start with '/'";
  }
  if (path.startsWith("//")) {
    return "request target must not start with '//'";
  }
  if (UNSAFE_TARGET_CHARS.test(path)) {
    return "request target contains a control character, space, or backslash";
  }
  return null;
}

/** Why one header field cannot be sent verbatim, or null if it is safe. */
export function headerFieldProblem(
  name: string,
  values: readonly string[],
): string | null {
  if (!isHttpToken(name)) {
    return `header name ${JSON.stringify(name)} is not an HTTP token`;
  }
  for (const value of values) {
    if (UNSAFE_FIELD_VALUE_CHARS.test(value)) {
      return `header ${name} contains CR, LF, or NUL`;
    }
  }
  return null;
}

/** Why a header map cannot be forwarded verbatim, or null if it is safe. */
export function headerMapProblem(headers: HeaderMap): string | null {
  for (const [name, values] of Object.entries(headers)) {
    const problem = headerFieldProblem(name, values);
    if (problem !== null) {
      return problem;
    }
  }
  return null;
}

export function asRequestHead(v: unknown): RequestHead | null {
  if (
    isRecord(v) &&
    typeof v.method === "string" &&
    typeof v.path === "string" &&
    typeof v.host === "string" &&
    typeof v.scheme === "string" &&
    typeof v.remote_addr === "string" &&
    typeof v.has_body === "boolean"
  ) {
    // A raw (tcp/tls) REQ_HEAD carries no HTTP semantics, so the server sends a
    // RequestHead with an empty header map; Go marshals that nil map as `null`.
    // Treat null/absent headers as empty rather than rejecting the frame, which
    // would drop every tcp/tls tunnel stream. A real HTTP request always carries
    // a populated (non-null) object here.
    let headers: HeaderMap;
    if (v.headers === null || v.headers === undefined) {
      headers = newHeaderMap();
    } else if (isHeaderMap(v.headers)) {
      // Copy onto a prototype-less map so a lookup of a name the peer did not
      // send ("constructor", "__proto__") is undefined, not an Object builtin.
      headers = Object.assign(newHeaderMap(), v.headers);
    } else {
      return null;
    }
    const raw = v.raw === true;
    // A raw (tcp/tls/udp) head carries no HTTP semantics and is never spliced
    // into a URL or request line; an HTTP or upgrade head is, so validate it.
    if (
      !raw &&
      (requestTargetProblem(v.method, v.path) !== null ||
        headerMapProblem(headers) !== null)
    ) {
      return null;
    }
    return {
      method: v.method,
      path: v.path,
      headers,
      host: v.host,
      scheme: v.scheme,
      remote_addr: v.remote_addr,
      has_body: v.has_body,
      // `upgrade` and `raw` are omitempty on the wire; absent means false.
      upgrade: v.upgrade === true,
      raw,
    };
  }
  return null;
}

export function asStreamReset(v: unknown): StreamReset | null {
  if (isRecord(v) && typeof v.code === "string") {
    const reset: StreamReset = { code: v.code };
    if (typeof v.message === "string") {
      reset.message = v.message;
    }
    return reset;
  }
  return null;
}

/** Convenience re-exports so callers name control types via one import. */
export { ControlType, FrameType };
