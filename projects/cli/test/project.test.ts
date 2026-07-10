import { describe, expect, test } from "bun:test";

import { parseArgs } from "../src/args.ts";
import {
  parseProjectConfig,
  selectTunnels,
  tunnelToArgv,
} from "../src/project.ts";

describe("start subcommand parsing", () => {
  test("rift start with names", () => {
    const p = parseArgs(["start", "web", "api"]);
    expect(p).toEqual({ kind: "start", names: ["web", "api"] });
  });
  test("rift start with no names", () => {
    expect(parseArgs(["start"])).toEqual({ kind: "start", names: [] });
  });
});

describe("parseProjectConfig", () => {
  test("parses YAML with a tunnels map", () => {
    const cfg = parseProjectConfig(
      "tunnels:\n  web:\n    port: 3000\n  api:\n    port: 4000\n",
      "/x/rift.yml",
    );
    expect(Object.keys(cfg.tunnels).sort()).toEqual(["api", "web"]);
  });
  test("parses TOML", () => {
    const cfg = parseProjectConfig(
      "[tunnels.web]\nport = 3000\n",
      "/x/rift.toml",
    );
    expect(cfg.tunnels).toHaveProperty("web");
  });
  test("parses JSON", () => {
    const cfg = parseProjectConfig(
      JSON.stringify({ tunnels: { web: { port: 3000 } } }),
      "/x/rift.json",
    );
    expect(cfg.tunnels).toHaveProperty("web");
  });
  test("rejects a doc without a tunnels map", () => {
    expect(() => parseProjectConfig("nope: 1\n", "/x/rift.yml")).toThrow(
      /tunnels/,
    );
  });
});

describe("selectTunnels", () => {
  const cfg = {
    tunnels: { web: { port: 3000 }, api: { port: 4000 } },
    path: "/x/rift.yml",
  };
  test("no names selects all, sorted", () => {
    expect(selectTunnels(cfg, [])).toEqual({ names: ["api", "web"] });
  });
  test("named selection is honoured", () => {
    expect(selectTunnels(cfg, ["web"])).toEqual({ names: ["web"] });
  });
  test("an unknown name is an error", () => {
    const r = selectTunnels(cfg, ["web", "nope"]);
    expect(r).toHaveProperty("error");
    if ("error" in r) expect(r.error).toContain("nope");
  });
});

describe("tunnelToArgv", () => {
  test("maps proto/port/subdomain and flags to argv, which parseArgs accepts", () => {
    const r = tunnelToArgv("web", {
      proto: "http",
      port: 3000,
      subdomain: "myweb",
      cors: true,
      "basic-auth": ["u:p"],
      domain: "app.acme.com",
    });
    if ("error" in r) throw new Error(r.error);
    // The subdomain follows the port; boolean flags have no value; arrays repeat.
    expect(r.argv.slice(0, 3)).toEqual(["http", "3000", "myweb"]);
    expect(r.argv).toContain("--cors");
    expect(r.argv).toContain("--basic-auth");
    expect(r.argv).toContain("u:p");
    expect(r.argv).toContain("--domain");

    const parsed = parseArgs(r.argv);
    expect(parsed.kind).toBe("run");
    if (parsed.kind !== "run") return;
    expect(parsed.protocol).toBe("http");
    expect(parsed.port).toBe(3000);
    expect(parsed.subdomain).toBe("myweb");
    expect(parsed.flags.cors).toBe(true);
    expect(parsed.flags.basicAuth).toEqual(["u:p"]);
    expect(parsed.flags.domain).toEqual(["app.acme.com"]);
  });

  test("defaults proto to http and requires an integer port", () => {
    const r = tunnelToArgv("api", { port: 4000 });
    if ("error" in r) throw new Error(r.error);
    expect(r.argv.slice(0, 2)).toEqual(["http", "4000"]);

    expect(tunnelToArgv("bad", { proto: "http" })).toHaveProperty("error");
    expect(tunnelToArgv("bad", "not-a-map")).toHaveProperty("error");
    expect(tunnelToArgv("bad", { port: 3.5 })).toHaveProperty("error");
  });

  test("an unknown flag key surfaces through parseArgs validation", () => {
    const r = tunnelToArgv("web", { port: 3000, bogusflag: "x" });
    if ("error" in r) throw new Error(r.error);
    const parsed = parseArgs(r.argv);
    expect(parsed.kind).toBe("error");
  });
});
