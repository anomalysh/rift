import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { parseArgs } from "../src/args.ts";
import { CLI_SPEC } from "../src/cli-spec.ts";
import {
  PROJECT_FORBIDDEN_KEYS,
  PROJECT_KEYS,
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
    // The subdomain follows the port; boolean flags have no value; values are
    // single --flag=value tokens; arrays repeat.
    expect(r.argv.slice(0, 3)).toEqual(["http", "3000", "myweb"]);
    expect(r.argv).toContain("--cors");
    expect(r.argv).toContain("--basic-auth=u:p");
    expect(r.argv).toContain("--domain=app.acme.com");

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

  test("an unknown key is rejected", () => {
    const r = tunnelToArgv("web", { port: 3000, bogusflag: "x" });
    expect(r).toHaveProperty("error");
    if ("error" in r) expect(r.error).toContain("bogusflag");
  });
});

// Regression: a cloned repository's rift.yml used to be able to set any flag,
// and flags beat env/config -- so `server: ws://attacker/` made `rift start`
// hand the user's token to the attacker.
describe("project files cannot redirect or weaken the token's path", () => {
  for (const [key, value] of [
    ["server", "ws://attacker.example/tunnel"],
    ["token", "rift_stolen"],
    ["token-file", "/etc/passwd"],
    ["insecure", true],
    ["upstream-insecure", true],
    ["allow-insecure-transport", true],
  ] as const) {
    test(`"${key}" is refused with an explanation`, () => {
      const r = tunnelToArgv("web", { port: 3000, [key]: value });
      expect(r).toHaveProperty("error");
      if ("error" in r) {
        expect(r.error).toContain(key);
        expect(r.error).toContain("project file");
      }
    });
  }

  test("set-* keys are not project keys", () => {
    expect(
      tunnelToArgv("web", { port: 3000, "set-server": "ws://x" }),
    ).toHaveProperty("error");
  });

  test("host is allowed only on loopback", () => {
    for (const ok of ["127.0.0.1", "localhost", "::1", "127.0.0.2"]) {
      const r = tunnelToArgv("web", { port: 3000, host: ok });
      if ("error" in r) throw new Error(r.error);
      expect(r.argv).toContain(`--host=${ok}`);
    }
    for (const bad of [
      "192.168.1.1",
      "10.0.0.1",
      "example.com",
      "169.254.169.254",
    ]) {
      const r = tunnelToArgv("web", { port: 3000, host: bad });
      expect(r).toHaveProperty("error");
    }
  });

  test("no value can smuggle a flag through a positional or boolean key", () => {
    // A subdomain or proto that looks like a flag would otherwise be parsed as one.
    expect(
      tunnelToArgv("web", { port: 3000, subdomain: "--server=ws://evil" }),
    ).toHaveProperty("error");
    expect(
      tunnelToArgv("web", { port: 3000, proto: "--server=ws://evil" }),
    ).toHaveProperty("error");
    // A boolean flag given a string must not emit that string as its own arg.
    expect(
      tunnelToArgv("web", { port: 3000, cors: "--insecure" }),
    ).toHaveProperty("error");
    // A value flag's value stays glued to its flag.
    const r = tunnelToArgv("web", { port: 3000, ttl: "--server=ws://evil" });
    if ("error" in r) throw new Error(r.error);
    const parsed = parseArgs(r.argv);
    if (parsed.kind === "run") {
      expect(parsed.flags.server).toBeUndefined();
      expect(parsed.flags.ttl).toBe("--server=ws://evil");
    }
  });

  test("every run flag is classified as allowed or forbidden", () => {
    // A new CLI flag must be consciously placed in one list or the other.
    for (const o of CLI_SPEC.options) {
      if (o.kind !== "run") continue;
      const key = o.long.slice(2);
      expect(PROJECT_KEYS.has(key) || PROJECT_FORBIDDEN_KEYS.has(key)).toBe(
        true,
      );
    }
  });
});

// Regression: a malformed rift.yml escaped runStart as a raw SyntaxError, so
// `rift start` crashed with a stack trace that did not name the file.
describe("malformed project file", () => {
  test("parse errors name the file", () => {
    expect(() =>
      parseProjectConfig("tunnels:\n  web: [unclosed\n", "/x/rift.yml"),
    ).toThrow(/^\/x\/rift\.yml: /);
    expect(() => parseProjectConfig("{", "/x/rift.json")).toThrow(
      /^\/x\/rift\.json: /,
    );
  });

  test("rift start reports a usage error, not a stack trace", async () => {
    const dir = mkdtempSync(join(tmpdir(), "rift-start-"));
    try {
      writeFileSync(join(dir, "rift.yml"), "tunnels:\n  web: [unclosed\n");
      const proc = Bun.spawn(
        [process.execPath, join(import.meta.dir, "../src/index.ts"), "start"],
        { cwd: dir, stdout: "pipe", stderr: "pipe" },
      );
      const [code, stderr] = await Promise.all([
        proc.exited,
        new Response(proc.stderr).text(),
      ]);
      expect(code).toBe(2);
      expect(stderr).toStartWith(`rift: ${join(dir, "rift.yml")}: `);
      expect(stderr).not.toContain("    at ");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
