// Agent-side traffic policy (T1-T3, T5, T6): the transformations rift applies to
// a request/response as it forwards, without the local service knowing. Parsed
// once from flags into a TrafficController that the forwarder consults per
// stream:
//   T1 header rewrite   -- add/remove request and response headers
//   T2 CORS             -- answer preflights, decorate actual responses
//   T3 mock / redirect  -- short-circuit a path with a fixed response
//   T5 path routing     -- send a path prefix to a different local port
//   T6 circuit breaking -- stop hammering a dead upstream, fail fast with 503
//
// Visitor-access policy (auth, IP, rate) is server-side and lives in policy.ts;
// this module is purely local and never leaves the agent.

import type { FlagConfig } from "./args.ts";
import type { HeaderMap } from "./protocol.ts";

/** A response the agent produces itself, bypassing the upstream fetch. */
export interface SyntheticResponse {
  readonly status: number;
  readonly headers: HeaderMap;
  readonly body: Uint8Array;
}

interface SetHeader {
  readonly name: string;
  readonly value: string;
}
interface MockRule {
  readonly method: string | null; // null = any method
  readonly path: string;
  readonly status: number;
  readonly body: string;
  readonly contentType: string;
}
interface RedirectRule {
  readonly path: string;
  readonly status: number;
  readonly location: string;
}
interface Route {
  readonly prefix: string;
  readonly port: number;
}

export interface TrafficPolicy {
  readonly setRequestHeaders: readonly SetHeader[];
  readonly delRequestHeaders: readonly string[];
  readonly setResponseHeaders: readonly SetHeader[];
  readonly delResponseHeaders: readonly string[];
  readonly cors: boolean;
  readonly mocks: readonly MockRule[];
  readonly redirects: readonly RedirectRule[];
  readonly routes: readonly Route[];
  readonly breakerThreshold: number | null; // null = breaker off
}

/** How long a tripped circuit stays open before a probe is let through (T6). */
const BREAKER_COOLDOWN_MS = 10_000;
const DEFAULT_BREAKER_THRESHOLD = 5;

const TEXT = new TextEncoder();

/** Split "Name: value" (or "Name=value") into a trimmed name and value. */
function splitHeaderAssign(raw: string): SetHeader | null {
  const idx = ((): number => {
    const c = raw.indexOf(":");
    const e = raw.indexOf("=");
    if (c === -1) return e;
    if (e === -1) return c;
    return Math.min(c, e);
  })();
  if (idx <= 0) return null;
  const name = raw.slice(0, idx).trim();
  const value = raw.slice(idx + 1).trim();
  if (name === "") return null;
  return { name, value };
}

function looksLikeJson(body: string): boolean {
  const t = body.trimStart();
  return t.startsWith("{") || t.startsWith("[");
}

// Parse "<path>=<status>:<body>" or "<path>=<body>" (status defaults to 200).
// An optional "METHOD <path>" prefix scopes the mock to one method.
function parseRespond(raw: string): MockRule | { error: string } {
  const eq = raw.indexOf("=");
  if (eq <= 0) {
    return {
      error: `expected <path>=<status>:<body>, got ${JSON.stringify(raw)}`,
    };
  }
  let pathPart = raw.slice(0, eq).trim();
  let method: string | null = null;
  const sp = pathPart.indexOf(" ");
  if (sp > 0) {
    method = pathPart.slice(0, sp).trim().toUpperCase();
    pathPart = pathPart.slice(sp + 1).trim();
  }
  if (!pathPart.startsWith("/")) {
    return {
      error: `--respond path must start with "/", got ${JSON.stringify(pathPart)}`,
    };
  }
  const rest = raw.slice(eq + 1);
  let status = 200;
  let body = rest;
  const m = rest.match(/^(\d{3}):([\s\S]*)$/);
  if (m?.[1] !== undefined) {
    status = Number.parseInt(m[1], 10);
    body = m[2] ?? "";
  }
  if (status < 100 || status > 599) {
    return { error: `--respond status ${status} is out of range (100-599)` };
  }
  return {
    method,
    path: pathPart,
    status,
    body,
    contentType: looksLikeJson(body)
      ? "application/json"
      : "text/plain; charset=utf-8",
  };
}

// Parse "<path>=<location>" or "<path>=<status>:<location>" (status default 302).
function parseRedirect(raw: string): RedirectRule | { error: string } {
  const eq = raw.indexOf("=");
  if (eq <= 0) {
    return { error: `expected <path>=<location>, got ${JSON.stringify(raw)}` };
  }
  const path = raw.slice(0, eq).trim();
  if (!path.startsWith("/")) {
    return {
      error: `--redirect path must start with "/", got ${JSON.stringify(path)}`,
    };
  }
  const rest = raw.slice(eq + 1).trim();
  let status = 302;
  let location = rest;
  const m = rest.match(/^(\d{3}):(.*)$/);
  if (m?.[1] !== undefined) {
    status = Number.parseInt(m[1], 10);
    location = m[2] ?? "";
  }
  if (status < 300 || status > 399) {
    return { error: `--redirect status ${status} is not a 3xx redirect code` };
  }
  if (location === "") {
    return { error: `--redirect ${JSON.stringify(raw)} has an empty location` };
  }
  return { path, status, location };
}

// Parse "<prefix>=<port>" into a route to a local port.
function parseRoute(raw: string): Route | { error: string } {
  const eq = raw.indexOf("=");
  if (eq <= 0) {
    return { error: `expected <prefix>=<port>, got ${JSON.stringify(raw)}` };
  }
  const prefix = raw.slice(0, eq).trim();
  if (!prefix.startsWith("/")) {
    return {
      error: `--route prefix must start with "/", got ${JSON.stringify(prefix)}`,
    };
  }
  const portRaw = raw.slice(eq + 1).trim();
  if (!/^\d+$/.test(portRaw)) {
    return { error: `--route ${JSON.stringify(raw)}: port must be a number` };
  }
  const port = Number.parseInt(portRaw, 10);
  if (port < 1 || port > 65535) {
    return { error: `--route port ${port} is out of range (1-65535)` };
  }
  return { prefix, port };
}

/**
 * Parse the traffic flags into a TrafficPolicy, or return the first error. A
 * result with no active rules yields no policy at all, so the forwarder's hot
 * path stays untouched on an ordinary tunnel.
 */
export function buildTrafficPolicy(
  flags: FlagConfig,
): { policy?: TrafficPolicy } | { error: string } {
  const setRequestHeaders: SetHeader[] = [];
  for (const raw of flags.setRequestHeader ?? []) {
    const h = splitHeaderAssign(raw);
    if (h === null)
      return {
        error: `invalid --set-request-header ${JSON.stringify(raw)}: expected "Name: value"`,
      };
    setRequestHeaders.push(h);
  }
  const setResponseHeaders: SetHeader[] = [];
  for (const raw of flags.setResponseHeader ?? []) {
    const h = splitHeaderAssign(raw);
    if (h === null)
      return {
        error: `invalid --set-response-header ${JSON.stringify(raw)}: expected "Name: value"`,
      };
    setResponseHeaders.push(h);
  }
  const delRequestHeaders = (flags.delRequestHeader ?? []).map((n) => n.trim());
  const delResponseHeaders = (flags.delResponseHeader ?? []).map((n) =>
    n.trim(),
  );

  const mocks: MockRule[] = [];
  for (const raw of flags.respond ?? []) {
    const r = parseRespond(raw);
    if ("error" in r) return { error: r.error };
    mocks.push(r);
  }
  const redirects: RedirectRule[] = [];
  for (const raw of flags.redirect ?? []) {
    const r = parseRedirect(raw);
    if ("error" in r) return { error: r.error };
    redirects.push(r);
  }
  const routes: Route[] = [];
  for (const raw of flags.route ?? []) {
    const r = parseRoute(raw);
    if ("error" in r) return { error: r.error };
    routes.push(r);
  }
  // Longest prefix first so the most specific route wins.
  routes.sort((a, b) => b.prefix.length - a.prefix.length);

  let breakerThreshold: number | null = null;
  if (flags.breakerThreshold !== undefined) {
    if (!/^\d+$/.test(flags.breakerThreshold)) {
      return {
        error: `invalid --breaker-threshold ${JSON.stringify(flags.breakerThreshold)}: expected a whole number`,
      };
    }
    const n = Number.parseInt(flags.breakerThreshold, 10);
    if (n < 1) return { error: "--breaker-threshold must be at least 1" };
    breakerThreshold = n;
  } else if (flags.breaker === true) {
    breakerThreshold = DEFAULT_BREAKER_THRESHOLD;
  }

  const policy: TrafficPolicy = {
    setRequestHeaders,
    delRequestHeaders,
    setResponseHeaders,
    delResponseHeaders,
    cors: flags.cors === true,
    mocks,
    redirects,
    routes,
    breakerThreshold,
  };
  if (
    setRequestHeaders.length === 0 &&
    delRequestHeaders.length === 0 &&
    setResponseHeaders.length === 0 &&
    delResponseHeaders.length === 0 &&
    !policy.cors &&
    mocks.length === 0 &&
    redirects.length === 0 &&
    routes.length === 0 &&
    breakerThreshold === null
  ) {
    return {};
  }
  return { policy };
}

interface BreakerEntry {
  failures: number;
  openedAt: number;
}

/**
 * Holds the compiled traffic policy plus the per-port breaker state, shared
 * across every stream on a connection. The forwarder calls it at four points:
 * routePort (choose the upstream), synthesize (short-circuit), decorateRequest
 * / decorateResponse (rewrite headers), and the breaker record* pair.
 */
export class TrafficController {
  private readonly breaker = new Map<number, BreakerEntry>();

  constructor(
    private readonly policy: TrafficPolicy,
    // Injectable clock keeps the breaker's timing testable.
    private readonly now: () => number = Date.now,
  ) {}

  /** T5: the port a path routes to, or null to use the default target. */
  routePort(path: string): number | null {
    for (const r of this.policy.routes) {
      if (pathMatchesPrefix(path, r.prefix)) return r.port;
    }
    return null;
  }

  /**
   * T2/T3: a response the agent serves itself for this request, or null to
   * forward upstream. Order: CORS preflight, then redirects, then mocks.
   */
  synthesize(
    method: string,
    path: string,
    reqHeaders: HeaderMap,
  ): SyntheticResponse | null {
    const cleanPath = stripQuery(path);
    if (
      this.policy.cors &&
      method === "OPTIONS" &&
      firstHeader(reqHeaders, "access-control-request-method") !== null
    ) {
      return this.preflight(reqHeaders);
    }
    for (const r of this.policy.redirects) {
      if (r.path === cleanPath) {
        return {
          status: r.status,
          headers: this.decorateResponse(
            { location: [r.location] },
            reqHeaders,
          ),
          body: new Uint8Array(0),
        };
      }
    }
    for (const m of this.policy.mocks) {
      if (m.path !== cleanPath) continue;
      if (m.method !== null && m.method !== method) continue;
      const body = TEXT.encode(m.body);
      return {
        status: m.status,
        headers: this.decorateResponse(
          {
            "content-type": [m.contentType],
            "content-length": [String(body.length)],
          },
          reqHeaders,
        ),
        body,
      };
    }
    return null;
  }

  /** T1: rewrite the outbound request headers in place (delete, then set). */
  decorateRequest(headers: Headers): void {
    for (const name of this.policy.delRequestHeaders) headers.delete(name);
    for (const { name, value } of this.policy.setRequestHeaders)
      headers.set(name, value);
  }

  /**
   * T1/T2: return the response header map with deletes, sets, and (if enabled)
   * CORS decoration applied. The upstream map is not mutated.
   */
  decorateResponse(headers: HeaderMap, reqHeaders: HeaderMap): HeaderMap {
    const out: HeaderMap = { ...headers };
    for (const name of this.policy.delResponseHeaders) {
      delete out[name.toLowerCase()];
    }
    for (const { name, value } of this.policy.setResponseHeaders) {
      out[name.toLowerCase()] = [value];
    }
    if (this.policy.cors) applyCorsResponse(out, reqHeaders);
    return out;
  }

  /** T6: whether the circuit to a port is open (skip the dial, fail fast). */
  breakerTripped(port: number): boolean {
    const threshold = this.policy.breakerThreshold;
    if (threshold === null) return false;
    const e = this.breaker.get(port);
    if (e === undefined || e.failures < threshold) return false;
    if (this.now() - e.openedAt >= BREAKER_COOLDOWN_MS) return false; // probe
    return true;
  }

  /** T6: record a transport-level result against a port's circuit. */
  recordResult(port: number, ok: boolean): void {
    if (this.policy.breakerThreshold === null) return;
    if (ok) {
      this.breaker.delete(port);
      return;
    }
    const threshold = this.policy.breakerThreshold;
    const e = this.breaker.get(port) ?? { failures: 0, openedAt: 0 };
    e.failures++;
    if (e.failures === threshold) {
      e.openedAt = this.now();
    } else if (
      e.failures > threshold &&
      this.now() - e.openedAt >= BREAKER_COOLDOWN_MS
    ) {
      e.openedAt = this.now(); // a post-cooldown probe failed: re-open
    }
    this.breaker.set(port, e);
  }

  /** The 503 body served while a circuit is open (T6). */
  breakerResponse(reqHeaders: HeaderMap): SyntheticResponse {
    const body = TEXT.encode("upstream circuit open");
    return {
      status: 503,
      headers: this.decorateResponse(
        {
          "content-type": ["text/plain; charset=utf-8"],
          "content-length": [String(body.length)],
          "retry-after": [String(Math.ceil(BREAKER_COOLDOWN_MS / 1000))],
        },
        reqHeaders,
      ),
      body,
    };
  }

  private preflight(reqHeaders: HeaderMap): SyntheticResponse {
    const headers: HeaderMap = {
      "access-control-allow-methods": [
        firstHeader(reqHeaders, "access-control-request-method") ??
          "GET, POST, PUT, PATCH, DELETE, OPTIONS",
      ],
      "access-control-allow-headers": [
        firstHeader(reqHeaders, "access-control-request-headers") ?? "*",
      ],
      "access-control-max-age": ["86400"],
      "content-length": ["0"],
    };
    applyCorsResponse(headers, reqHeaders);
    return { status: 204, headers, body: new Uint8Array(0) };
  }
}

/** A prefix matches a whole path segment: "/api" matches "/api" and "/api/x". */
function pathMatchesPrefix(path: string, prefix: string): boolean {
  const p = stripQuery(path);
  if (prefix === "/") return true;
  if (!p.startsWith(prefix)) return false;
  return p.length === prefix.length || p[prefix.length] === "/";
}

function stripQuery(path: string): string {
  const q = path.indexOf("?");
  return q === -1 ? path : path.slice(0, q);
}

function firstHeader(headers: HeaderMap, name: string): string | null {
  const v = headers[name.toLowerCase()];
  return v?.[0] ?? null;
}

/**
 * Add the CORS headers an actual (non-preflight) response needs. The request's
 * Origin is echoed back so credentialed requests work; without an Origin the
 * response is same-origin and a wildcard is harmless. Vary: Origin keeps caches
 * from serving one origin's response to another.
 */
function applyCorsResponse(out: HeaderMap, reqHeaders: HeaderMap): void {
  const origin = firstHeader(reqHeaders, "origin");
  out["access-control-allow-origin"] = [origin ?? "*"];
  if (origin !== null) {
    out["access-control-allow-credentials"] = ["true"];
    const existingVary = out.vary?.[0];
    out.vary = [existingVary ? `${existingVary}, Origin` : "Origin"];
  }
}
