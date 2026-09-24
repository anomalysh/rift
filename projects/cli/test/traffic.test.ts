import { describe, expect, test } from "bun:test";

import { parseArgs } from "../src/args.ts";
import type { TrafficPolicy } from "../src/traffic.ts";
import { buildTrafficPolicy, TrafficController } from "../src/traffic.ts";

function policyOrThrow(flags: Parameters<typeof buildTrafficPolicy>[0]) {
  const r = buildTrafficPolicy(flags);
  if ("error" in r) throw new Error(r.error);
  if (r.policy === undefined) throw new Error("expected a policy");
  return r.policy;
}

function decode(body: Uint8Array): string {
  return new TextDecoder().decode(body);
}

describe("traffic flag parsing", () => {
  test("repeatable traffic flags accumulate and booleans set", () => {
    const p = parseArgs([
      "http",
      "3000",
      "--set-request-header",
      "X-A: 1",
      "--set-request-header",
      "X-B: 2",
      "--route",
      "/api=4000",
      "--cors",
      "--breaker",
    ]);
    expect(p.kind).toBe("run");
    if (p.kind !== "run") return;
    expect(p.flags.setRequestHeader).toEqual(["X-A: 1", "X-B: 2"]);
    expect(p.flags.route).toEqual(["/api=4000"]);
    expect(p.flags.cors).toBe(true);
    expect(p.flags.breaker).toBe(true);
  });
});

describe("buildTrafficPolicy", () => {
  test("no traffic flags -> no policy", () => {
    expect(buildTrafficPolicy({})).toEqual({});
  });

  test("malformed rules are rejected with a clear error", () => {
    expect(
      buildTrafficPolicy({ setRequestHeader: ["noseparator"] }),
    ).toHaveProperty("error");
    expect(
      buildTrafficPolicy({ respond: ["missing-slash=200:x"] }),
    ).toHaveProperty("error");
    expect(buildTrafficPolicy({ redirect: ["/x=999:/y"] })).toHaveProperty(
      "error",
    );
    expect(buildTrafficPolicy({ route: ["/api=notaport"] })).toHaveProperty(
      "error",
    );
    expect(buildTrafficPolicy({ breakerThreshold: "0" })).toHaveProperty(
      "error",
    );
  });

  // Regression: an invalid header name was accepted here and then made
  // Headers.set throw on every forwarded request (each one reset), and a CR/LF
  // in a response-header value was relayed to the gateway verbatim.
  test("header rules must name a valid field with a safe value", () => {
    for (const flags of [
      { setRequestHeader: ["X Bad: 1"] },
      { setResponseHeader: ["X-Ok: a\r\nSet-Cookie: pwned=1"] },
      { setResponseHeader: ["X-Ok: a\u0000b"] },
      { delRequestHeader: ["bad header"] },
      { delResponseHeader: [""] },
    ]) {
      const r = buildTrafficPolicy(flags);
      expect(r).toHaveProperty("error");
      if ("error" in r) expect(r.error).toStartWith("invalid --");
    }
    const ok = policyOrThrow({
      setRequestHeader: ["X-A: 1"],
      delResponseHeader: [" server "],
    });
    expect(ok.setRequestHeaders).toEqual([{ name: "X-A", value: "1" }]);
    expect(ok.delResponseHeaders).toEqual(["server"]);
  });

  test("routes are ordered longest-prefix first", () => {
    const p = policyOrThrow({ route: ["/=3000", "/api/v2=5000", "/api=4000"] });
    expect(p.routes.map((r) => r.prefix)).toEqual(["/api/v2", "/api", "/"]);
  });

  test("--breaker defaults the threshold to 5", () => {
    expect(policyOrThrow({ breaker: true }).breakerThreshold).toBe(5);
    expect(policyOrThrow({ breakerThreshold: "3" }).breakerThreshold).toBe(3);
  });
});

describe("TrafficController routing (T5)", () => {
  test("longest matching prefix wins; unmatched paths use the default", () => {
    const p = policyOrThrow({ route: ["/=3000", "/api=4000"] });
    const c = new TrafficController(p);
    expect(c.routePort("/api/users")).toBe(4000);
    expect(c.routePort("/api")).toBe(4000);
    expect(c.routePort("/other")).toBe(3000);
    // "/apiary" must NOT match the "/api" prefix (segment boundary).
    const only = new TrafficController(policyOrThrow({ route: ["/api=4000"] }));
    expect(only.routePort("/apiary")).toBe(null);
  });
});

describe("TrafficController synthesize (T2/T3)", () => {
  test("mock response serves a fixed body with inferred content-type", () => {
    const c = new TrafficController(
      policyOrThrow({ respond: ['/health=200:{"ok":true}'] }),
    );
    const res = c.synthesize("GET", "/health?ping=1", {});
    expect(res).not.toBeNull();
    if (res === null) return;
    expect(res.status).toBe(200);
    expect(decode(res.body)).toBe('{"ok":true}');
    expect(res.headers["content-type"]?.[0]).toBe("application/json");
    // A non-matching path forwards upstream.
    expect(c.synthesize("GET", "/other", {})).toBeNull();
  });

  test("redirect returns a 3xx with Location", () => {
    const c = new TrafficController(policyOrThrow({ redirect: ["/old=/new"] }));
    const res = c.synthesize("GET", "/old", {});
    expect(res?.status).toBe(302);
    expect(res?.headers.location?.[0]).toBe("/new");
  });

  test("CORS preflight is answered with 204 and a credential-free wildcard", () => {
    const c = new TrafficController(policyOrThrow({ cors: true }));
    const res = c.synthesize("OPTIONS", "/api", {
      origin: ["https://app.example"],
      "access-control-request-method": ["POST"],
    });
    expect(res?.status).toBe(204);
    expect(res?.headers["access-control-allow-origin"]?.[0]).toBe("*");
    expect(res?.headers["access-control-allow-methods"]?.[0]).toBe("POST");
    expect(res?.headers["access-control-allow-credentials"]).toBeUndefined();
  });

  test("an allowlisted --cors-origin preflight gets its origin and credentials", () => {
    const c = new TrafficController(
      policyOrThrow({ corsOrigin: ["https://app.example"] }),
    );
    const res = c.synthesize("OPTIONS", "/api", {
      origin: ["https://app.example"],
      "access-control-request-method": ["POST"],
    });
    expect(res?.status).toBe(204);
    expect(res?.headers["access-control-allow-origin"]?.[0]).toBe(
      "https://app.example",
    );
    expect(res?.headers["access-control-allow-credentials"]?.[0]).toBe("true");
    expect(res?.headers.vary?.[0]).toContain("Origin");
  });

  test("a plain OPTIONS without a preflight header is not synthesized", () => {
    const c = new TrafficController(policyOrThrow({ cors: true }));
    expect(c.synthesize("OPTIONS", "/api", {})).toBeNull();
  });
});

describe("TrafficController header rewrite (T1)", () => {
  test("request headers: delete then set", () => {
    const c = new TrafficController(
      policyOrThrow({
        setRequestHeader: ["X-Env: prod"],
        delRequestHeader: ["X-Debug"],
      }),
    );
    const h = new Headers({ "X-Debug": "1", "X-Keep": "yes" });
    c.decorateRequest(h);
    expect(h.get("X-Debug")).toBeNull();
    expect(h.get("X-Env")).toBe("prod");
    expect(h.get("X-Keep")).toBe("yes");
  });

  test("response headers: delete, set, and CORS decoration", () => {
    const c = new TrafficController(
      policyOrThrow({
        setResponseHeader: ["X-Frame-Options: DENY"],
        delResponseHeader: ["Server"],
        cors: true,
      }),
    );
    const out = c.decorateResponse(
      { server: ["nginx"], "content-type": ["text/html"] },
      { origin: ["https://app.example"] },
    );
    expect(out.server).toBeUndefined();
    expect(out["x-frame-options"]?.[0]).toBe("DENY");
    expect(out["access-control-allow-origin"]?.[0]).toBe("*");
    expect(out["access-control-allow-credentials"]).toBeUndefined();
  });
});

// Regression: --cors used to echo ANY Origin together with
// Access-Control-Allow-Credentials: true, so every website a visitor opened
// could read the tunnel with their cookies / cached Basic auth.
describe("CORS credentials are only granted to allowlisted origins", () => {
  const c = new TrafficController(
    policyOrThrow({
      corsOrigin: ["https://app.example", "http://localhost:5173"],
    }),
  );

  test("an unlisted origin gets a wildcard and never credentials", () => {
    const out = c.decorateResponse(
      {},
      { origin: ["https://evil.example"], cookie: ["session=1"] },
    );
    expect(out["access-control-allow-origin"]?.[0]).toBe("*");
    expect(out["access-control-allow-credentials"]).toBeUndefined();
    // The answer varies by Origin, so caches must key on it.
    expect(out.vary?.[0]).toBe("Origin");
  });

  test("a listed origin is echoed with credentials", () => {
    const out = c.decorateResponse(
      { vary: ["Accept-Encoding"] },
      { origin: ["http://localhost:5173"] },
    );
    expect(out["access-control-allow-origin"]?.[0]).toBe(
      "http://localhost:5173",
    );
    expect(out["access-control-allow-credentials"]?.[0]).toBe("true");
    expect(out.vary?.[0]).toBe("Accept-Encoding, Origin");
  });

  test("an upstream Allow-Credentials is stripped for an unlisted origin", () => {
    const out = new TrafficController(
      policyOrThrow({ cors: true }),
    ).decorateResponse(
      { "access-control-allow-credentials": ["true"] },
      { origin: ["https://evil.example"] },
    );
    expect(out["access-control-allow-credentials"]).toBeUndefined();
  });

  test("--cors-origin implies --cors and normalizes the origin", () => {
    const p = policyOrThrow({ corsOrigin: ["https://App.Example/"] });
    expect(p.cors).toBe(true);
    expect(p.corsOrigins).toEqual(["https://app.example"]);
  });

  test("--cors-origin rejects anything but a bare origin", () => {
    for (const bad of [
      "*",
      "null",
      "app.example",
      "https://app.example/path",
      "https://app.example?x=1",
      "https://user@app.example",
      "ftp://app.example",
    ]) {
      expect(buildTrafficPolicy({ corsOrigin: [bad] })).toHaveProperty("error");
    }
  });

  test("--cors-origin parses as a repeatable flag", () => {
    const p = parseArgs([
      "http",
      "3000",
      "--cors-origin",
      "https://a.example",
      "--cors-origin=https://b.example",
    ]);
    expect(p.kind).toBe("run");
    if (p.kind !== "run") return;
    expect(p.flags.corsOrigin).toEqual([
      "https://a.example",
      "https://b.example",
    ]);
  });
});

describe("TrafficController breaker (T6)", () => {
  function fixedClock(): {
    c: TrafficController;
    advance: (ms: number) => void;
  } {
    let t = 0;
    const p: TrafficPolicy = policyOrThrow({ breakerThreshold: "3" });
    const c = new TrafficController(p, () => t);
    return { c, advance: (ms) => (t += ms) };
  }

  test("opens after threshold failures, then probes after cooldown", () => {
    const { c, advance } = fixedClock();
    expect(c.breakerTripped(3000)).toBe(false);
    c.recordResult(3000, false);
    c.recordResult(3000, false);
    expect(c.breakerTripped(3000)).toBe(false); // 2 < threshold
    c.recordResult(3000, false); // 3rd failure opens it
    expect(c.breakerTripped(3000)).toBe(true);

    // After the cooldown a probe is let through.
    advance(10_000);
    expect(c.breakerTripped(3000)).toBe(false);
    // A success closes the circuit.
    c.recordResult(3000, true);
    expect(c.breakerTripped(3000)).toBe(false);
  });

  test("a healthy port with no breaker configured never trips", () => {
    const c = new TrafficController(policyOrThrow({ route: ["/=3000"] }));
    c.recordResult(3000, false);
    c.recordResult(3000, false);
    expect(c.breakerTripped(3000)).toBe(false);
  });
});
