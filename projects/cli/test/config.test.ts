import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import type { FlagConfig } from "../src/args.ts";
import {
  ConfigError,
  configFilePath,
  formatAuthority,
  gatewayTransportProblem,
  isLoopbackHost,
  loadConfigFile,
  type PartialConfig,
  parseConfigFile,
  readTokenFile,
  resolveConfig,
  writeConfigValues,
} from "../src/config.ts";

const CONFIG_PATH = "/home/user/.config/rift/config.json";

function resolve(input: {
  flags?: FlagConfig;
  env?: Record<string, string | undefined>;
  file?: PartialConfig | null;
}) {
  return resolveConfig({
    flags: input.flags ?? {},
    env: input.env ?? {},
    file: input.file ?? null,
    configPath: CONFIG_PATH,
  });
}

describe("precedence: flag > env > file > default", () => {
  test("flag beats env beats file for token", () => {
    const cfg = resolve({
      flags: { token: "flag-tok" },
      env: { RIFT_TOKEN: "env-tok", RIFT_SERVER: "wss://s" },
      file: { token: "file-tok" },
    });
    expect(cfg.token).toBe("flag-tok");
  });

  test("env beats file when no flag", () => {
    const cfg = resolve({
      env: { RIFT_TOKEN: "env-tok", RIFT_SERVER: "env-srv" },
      file: { token: "file-tok", server: "file-srv" },
    });
    expect(cfg.token).toBe("env-tok");
    expect(cfg.server).toBe("env-srv");
  });

  test("file beats default for host and logLevel", () => {
    const cfg = resolve({
      env: { RIFT_TOKEN: "t", RIFT_SERVER: "s" },
      file: { host: "10.0.0.1", logLevel: "warn" },
    });
    expect(cfg.host).toBe("10.0.0.1");
    expect(cfg.logLevel).toBe("warn");
  });

  test("defaults apply when nothing else sets host or logLevel", () => {
    const cfg = resolve({ flags: { token: "t", server: "s" } });
    expect(cfg.host).toBe("127.0.0.1");
    expect(cfg.logLevel).toBe("info");
  });

  test("host precedence across all four layers", () => {
    const layered = {
      env: { RIFT_TOKEN: "t", RIFT_SERVER: "s", RIFT_HOST: "env-host" },
      file: { host: "file-host" },
    };
    expect(resolve({ ...layered, flags: { host: "flag-host" } }).host).toBe(
      "flag-host",
    );
    expect(resolve(layered).host).toBe("env-host");
    expect(
      resolve({
        file: layered.file,
        env: { RIFT_TOKEN: "t", RIFT_SERVER: "s" },
      }).host,
    ).toBe("file-host");
  });

  test("insecure comes from flags and defaults to false", () => {
    expect(resolve({ flags: { token: "t", server: "s" } }).insecure).toBe(
      false,
    );
    expect(
      resolve({ flags: { token: "t", server: "s", insecure: true } }).insecure,
    ).toBe(true);
  });

  test("upstreamInsecure comes from flags and defaults to false", () => {
    expect(
      resolve({ flags: { token: "t", server: "s" } }).upstreamInsecure,
    ).toBe(false);
    expect(
      resolve({ flags: { token: "t", server: "s", upstreamInsecure: true } })
        .upstreamInsecure,
    ).toBe(true);
  });
});

describe("missing required settings", () => {
  test("missing token throws a clear ConfigError", () => {
    expect(() => resolve({ env: { RIFT_SERVER: "s" } })).toThrow(ConfigError);
    try {
      resolve({ env: { RIFT_SERVER: "s" } });
    } catch (err) {
      expect(err).toBeInstanceOf(ConfigError);
      expect((err as ConfigError).message).toContain("token");
      expect((err as ConfigError).message).toContain("RIFT_TOKEN");
      expect((err as ConfigError).message).toContain(CONFIG_PATH);
    }
  });

  test("missing server throws a clear ConfigError", () => {
    try {
      resolve({ env: { RIFT_TOKEN: "t" } });
      throw new Error("expected ConfigError");
    } catch (err) {
      expect(err).toBeInstanceOf(ConfigError);
      expect((err as ConfigError).message).toContain("server");
      expect((err as ConfigError).message).toContain("RIFT_SERVER");
    }
  });

  test("empty string env values are treated as unset", () => {
    expect(() =>
      resolve({ env: { RIFT_TOKEN: "", RIFT_SERVER: "s" } }),
    ).toThrow(ConfigError);
  });
});

describe("env validation", () => {
  test("invalid RIFT_LOG_LEVEL is rejected", () => {
    expect(() =>
      resolve({
        env: { RIFT_TOKEN: "t", RIFT_SERVER: "s", RIFT_LOG_LEVEL: "loud" },
      }),
    ).toThrow(ConfigError);
  });

  test("valid RIFT_LOG_LEVEL is honoured", () => {
    expect(
      resolve({
        env: { RIFT_TOKEN: "t", RIFT_SERVER: "s", RIFT_LOG_LEVEL: "error" },
      }).logLevel,
    ).toBe("error");
  });
});

describe("configFilePath", () => {
  test("honours XDG_CONFIG_HOME", () => {
    expect(configFilePath({ XDG_CONFIG_HOME: "/xdg" })).toBe(
      "/xdg/rift/config.json",
    );
  });

  test("falls back to HOME/.config", () => {
    expect(configFilePath({ HOME: "/home/user" })).toBe(
      "/home/user/.config/rift/config.json",
    );
  });

  test("XDG_CONFIG_HOME wins over HOME", () => {
    expect(
      configFilePath({ XDG_CONFIG_HOME: "/xdg", HOME: "/home/user" }),
    ).toBe("/xdg/rift/config.json");
  });
});

describe("parseConfigFile", () => {
  test("parses a valid file", () => {
    const cfg = parseConfigFile(
      JSON.stringify({ token: "t", server: "s", host: "h", logLevel: "debug" }),
      CONFIG_PATH,
    );
    expect(cfg).toEqual({
      token: "t",
      server: "s",
      host: "h",
      logLevel: "debug",
    });
  });

  test("ignores unknown keys and empty strings", () => {
    const cfg = parseConfigFile(
      JSON.stringify({ token: "", extra: 1, host: "h" }),
      CONFIG_PATH,
    );
    expect(cfg).toEqual({ host: "h" });
  });

  test("rejects invalid JSON", () => {
    expect(() => parseConfigFile("{not json", CONFIG_PATH)).toThrow(
      ConfigError,
    );
  });

  test("rejects a non-object top level", () => {
    expect(() => parseConfigFile("[]", CONFIG_PATH)).toThrow(ConfigError);
  });

  test("rejects a non-string field", () => {
    expect(() =>
      parseConfigFile(JSON.stringify({ token: 5 }), CONFIG_PATH),
    ).toThrow(ConfigError);
  });

  test("rejects an invalid logLevel", () => {
    expect(() =>
      parseConfigFile(JSON.stringify({ logLevel: "loud" }), CONFIG_PATH),
    ).toThrow(ConfigError);
  });
});

describe("writeConfigValues", () => {
  let dir: string;
  let env: Record<string, string | undefined>;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "rift-cfg-"));
    env = { XDG_CONFIG_HOME: dir };
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  test("creates the file and round-trips through loadConfigFile", () => {
    const { path, keys } = writeConfigValues(env, { token: "rift_abc" });
    expect(keys).toEqual(["token"]);
    expect(path).toBe(configFilePath(env));
    expect(loadConfigFile(env)).toEqual({ token: "rift_abc" });
  });

  test("writes the config file 0600 (owner-only, holds a secret)", () => {
    const { path } = writeConfigValues(env, { token: "rift_abc" });
    // Low 9 permission bits.
    expect(statSync(path).mode & 0o777).toBe(0o600);
  });

  test("merges onto existing keys instead of clobbering them", () => {
    writeConfigValues(env, { server: "wss://gw", host: "10.0.0.1" });
    writeConfigValues(env, { token: "rift_abc" });
    expect(loadConfigFile(env)).toEqual({
      server: "wss://gw",
      host: "10.0.0.1",
      token: "rift_abc",
    });
  });

  test("tightens permissions on an already-loose file", () => {
    const path = configFilePath(env);
    // Simulate a pre-existing world-readable config.
    mkdirSync(join(dir, "rift"), { recursive: true });
    writeFileSync(path, JSON.stringify({ server: "wss://old" }), {
      mode: 0o644,
    });
    writeConfigValues(env, { token: "rift_abc" });
    expect(statSync(path).mode & 0o777).toBe(0o600);
  });

  test("rejects a corrupt existing config file", () => {
    const path = configFilePath(env);
    mkdirSync(join(dir, "rift"), { recursive: true });
    writeFileSync(path, "{not json", "utf8");
    expect(() => writeConfigValues(env, { token: "rift_abc" })).toThrow(
      ConfigError,
    );
  });
});

describe("config file hardening", () => {
  let dir: string;
  let env: Record<string, string | undefined>;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "rift-cfg-"));
    env = { XDG_CONFIG_HOME: dir };
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  test("the rift config directory is forced to 0700", () => {
    mkdirSync(join(dir, "rift"), { mode: 0o755 });
    chmodSync(join(dir, "rift"), 0o755);
    writeConfigValues(env, { token: "rift_abc" });
    expect(statSync(join(dir, "rift")).mode & 0o777).toBe(0o700);
  });

  test("the write is atomic: no temp file is left behind", () => {
    writeConfigValues(env, { token: "rift_abc" });
    writeConfigValues(env, { server: "wss://gw" });
    expect(readdirSync(join(dir, "rift"))).toEqual(["config.json"]);
  });

  test("a group/world-readable config holding a token warns on load", () => {
    const path = configFilePath(env);
    mkdirSync(join(dir, "rift"), { recursive: true });
    writeFileSync(path, JSON.stringify({ token: "t" }), { mode: 0o644 });
    chmodSync(path, 0o644);
    const warnings: string[] = [];
    loadConfigFile(env, (m) => warnings.push(m));
    expect(warnings).toHaveLength(1);
    expect(warnings[0]).toContain("chmod 600");

    chmodSync(path, 0o600);
    const quiet: string[] = [];
    loadConfigFile(env, (m) => quiet.push(m));
    expect(quiet).toHaveLength(0);
  });
});

describe("readTokenFile (--token-file)", () => {
  let dir: string;
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "rift-tok-"));
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  test("reads and trims the token", () => {
    const p = join(dir, "token");
    writeFileSync(p, "  rift_secret\n", { mode: 0o600 });
    expect(readTokenFile(p, () => {})).toBe("rift_secret");
  });

  test("an empty or missing file is a ConfigError", () => {
    const p = join(dir, "empty");
    writeFileSync(p, "\n", { mode: 0o600 });
    expect(() => readTokenFile(p, () => {})).toThrow(ConfigError);
    expect(() => readTokenFile(join(dir, "nope"), () => {})).toThrow(
      ConfigError,
    );
  });
});

// Regression: a ws:// gateway was dialed silently, sending the token in the
// clear to anyone on the path.
describe("gateway transport security", () => {
  test("wss:// is always allowed", () => {
    expect(
      gatewayTransportProblem("wss://gw.example/tunnel", false),
    ).toBeNull();
  });

  test("ws:// to loopback is allowed", () => {
    for (const s of [
      "ws://127.0.0.1:8081/tunnel",
      "ws://localhost:8081/tunnel",
      "ws://[::1]:8081/tunnel",
    ]) {
      expect(gatewayTransportProblem(s, false)).toBeNull();
    }
  });

  test("ws:// to a remote host is refused without the opt-in", () => {
    const problem = gatewayTransportProblem("ws://gw.example/tunnel", false);
    expect(problem).toContain("cleartext");
    expect(problem).toContain("--allow-insecure-transport");
    expect(gatewayTransportProblem("ws://gw.example/tunnel", true)).toBeNull();
  });

  test("a non-WebSocket or malformed URL is refused", () => {
    expect(gatewayTransportProblem("https://gw.example", true)).not.toBeNull();
    expect(gatewayTransportProblem("gw.example", true)).not.toBeNull();
  });

  test("the opt-in comes from the flag or RIFT_ALLOW_INSECURE_TRANSPORT", () => {
    expect(
      resolve({ flags: { token: "t", server: "s" } }).allowInsecureTransport,
    ).toBe(false);
    expect(
      resolve({
        flags: { token: "t", server: "s", allowInsecureTransport: true },
      }).allowInsecureTransport,
    ).toBe(true);
    expect(
      resolve({
        flags: { token: "t", server: "s" },
        env: { RIFT_ALLOW_INSECURE_TRANSPORT: "1" },
      }).allowInsecureTransport,
    ).toBe(true);
    expect(
      resolve({
        flags: { token: "t", server: "s" },
        env: { RIFT_ALLOW_INSECURE_TRANSPORT: "0" },
      }).allowInsecureTransport,
    ).toBe(false);
  });
});

describe("isLoopbackHost", () => {
  test("recognizes loopback names and addresses", () => {
    for (const h of [
      "127.0.0.1",
      "127.1.2.3",
      "localhost",
      "LOCALHOST",
      "a.localhost",
      "::1",
      "[::1]",
    ]) {
      expect(isLoopbackHost(h)).toBe(true);
    }
    for (const h of [
      "10.0.0.1",
      "192.168.1.1",
      "127.0.0.1.evil.com",
      "localhost.evil.com",
      "0.0.0.0",
      "::",
    ]) {
      expect(isLoopbackHost(h)).toBe(false);
    }
  });
});

// Regression: an IPv6 upstream was spliced in as "::1:3000" -- an invalid Host
// header on a replayed upgrade request and a misleading "forwarding" line.
describe("formatAuthority", () => {
  test("brackets a bare IPv6 literal and leaves everything else alone", () => {
    expect(formatAuthority("::1", 3000)).toBe("[::1]:3000");
    expect(formatAuthority("[::1]", 3000)).toBe("[::1]:3000");
    expect(formatAuthority("127.0.0.1", 3000)).toBe("127.0.0.1:3000");
    expect(formatAuthority("localhost", 80)).toBe("localhost:80");
  });
});
