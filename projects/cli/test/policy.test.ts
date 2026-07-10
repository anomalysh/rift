import { describe, expect, test } from "bun:test";

import { parseArgs } from "../src/args.ts";
import { buildPolicy } from "../src/policy.ts";

describe("policy flag parsing", () => {
  test("repeated --basic-auth / --allow-ip accumulate", () => {
    const p = parseArgs([
      "http",
      "3000",
      "--basic-auth",
      "a:1",
      "--basic-auth",
      "b:2",
      "--allow-ip",
      "10.0.0.0/8",
      "--allow-ip",
      "192.168.0.0/16",
      "--deny-ip",
      "10.9.0.0/16",
      "--once",
    ]);
    expect(p.kind).toBe("run");
    if (p.kind !== "run") return;
    expect(p.flags.basicAuth).toEqual(["a:1", "b:2"]);
    expect(p.flags.allowIp).toEqual(["10.0.0.0/8", "192.168.0.0/16"]);
    expect(p.flags.denyIp).toEqual(["10.9.0.0/16"]);
    expect(p.flags.once).toBe(true);
  });
});

describe("buildPolicy", () => {
  test("no policy flags -> no policy", async () => {
    const r = await buildPolicy({});
    expect(r).toEqual({});
  });

  test("basic-auth is bcrypt-hashed, never plaintext", async () => {
    const r = await buildPolicy({ basicAuth: ["alice:s3cret"] });
    expect("policy" in r).toBe(true);
    if (!("policy" in r) || !r.policy) throw new Error("no policy");
    const cred = r.policy.basic_auth?.[0];
    expect(cred?.user).toBe("alice");
    expect(cred?.hash).toMatch(/^\$2[aby]\$/); // a bcrypt hash
    expect(cred?.hash).not.toContain("s3cret");
    // The hash verifies against the original password.
    expect(await Bun.password.verify("s3cret", cred?.hash as string)).toBe(
      true,
    );
  });

  test("ip lists, ttl, once, max-requests, rate-limit map to the wire shape", async () => {
    const r = await buildPolicy({
      allowIp: ["10.0.0.0/8"],
      denyIp: ["10.9.0.0/16"],
      ttl: "30m",
      once: true,
      maxRequests: "100",
      rateLimit: "20/s",
    });
    if (!("policy" in r) || !r.policy) throw new Error("no policy");
    expect(r.policy.allow_ips).toEqual(["10.0.0.0/8"]);
    expect(r.policy.deny_ips).toEqual(["10.9.0.0/16"]);
    expect(r.policy.ttl_seconds).toBe(1800);
    expect(r.policy.once).toBe(true);
    expect(r.policy.max_requests).toBe(100);
    expect(r.policy.rate_limit).toEqual({ rps: 20 });
  });

  test("ttl accepts h/m/s and bare seconds", async () => {
    const ttlSeconds = async (v: string): Promise<number | undefined> => {
      const r = await buildPolicy({ ttl: v });
      return "policy" in r ? r.policy?.ttl_seconds : undefined;
    };
    expect(await ttlSeconds("1h")).toBe(3600);
    expect(await ttlSeconds("90s")).toBe(90);
    expect(await ttlSeconds("120")).toBe(120);
  });

  test("malformed values are rejected with a clear error", async () => {
    expect(await buildPolicy({ basicAuth: ["nocolon"] })).toHaveProperty(
      "error",
    );
    expect(await buildPolicy({ basicAuth: ["user:"] })).toHaveProperty("error");
    expect(await buildPolicy({ ttl: "later" })).toHaveProperty("error");
    expect(await buildPolicy({ maxRequests: "-1" })).toHaveProperty("error");
    expect(await buildPolicy({ rateLimit: "fast" })).toHaveProperty("error");
  });
});
