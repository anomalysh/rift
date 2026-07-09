import { describe, expect, test } from "bun:test";

import type { FlagConfig } from "../src/args.ts";
import {
  ConfigError,
  configFilePath,
  parseConfigFile,
  resolveConfig,
  type PartialConfig,
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
    expect(resolve({ ...layered, flags: { host: "flag-host" } }).host).toBe("flag-host");
    expect(resolve(layered).host).toBe("env-host");
    expect(resolve({ file: layered.file, env: { RIFT_TOKEN: "t", RIFT_SERVER: "s" } }).host).toBe(
      "file-host",
    );
  });

  test("insecure comes from flags and defaults to false", () => {
    expect(resolve({ flags: { token: "t", server: "s" } }).insecure).toBe(false);
    expect(resolve({ flags: { token: "t", server: "s", insecure: true } }).insecure).toBe(true);
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
    expect(() => resolve({ env: { RIFT_TOKEN: "", RIFT_SERVER: "s" } })).toThrow(ConfigError);
  });
});

describe("env validation", () => {
  test("invalid RIFT_LOG_LEVEL is rejected", () => {
    expect(() =>
      resolve({ env: { RIFT_TOKEN: "t", RIFT_SERVER: "s", RIFT_LOG_LEVEL: "loud" } }),
    ).toThrow(ConfigError);
  });

  test("valid RIFT_LOG_LEVEL is honoured", () => {
    expect(
      resolve({ env: { RIFT_TOKEN: "t", RIFT_SERVER: "s", RIFT_LOG_LEVEL: "error" } }).logLevel,
    ).toBe("error");
  });
});

describe("configFilePath", () => {
  test("honours XDG_CONFIG_HOME", () => {
    expect(configFilePath({ XDG_CONFIG_HOME: "/xdg" })).toBe("/xdg/rift/config.json");
  });

  test("falls back to HOME/.config", () => {
    expect(configFilePath({ HOME: "/home/user" })).toBe(
      "/home/user/.config/rift/config.json",
    );
  });

  test("XDG_CONFIG_HOME wins over HOME", () => {
    expect(configFilePath({ XDG_CONFIG_HOME: "/xdg", HOME: "/home/user" })).toBe(
      "/xdg/rift/config.json",
    );
  });
});

describe("parseConfigFile", () => {
  test("parses a valid file", () => {
    const cfg = parseConfigFile(
      JSON.stringify({ token: "t", server: "s", host: "h", logLevel: "debug" }),
      CONFIG_PATH,
    );
    expect(cfg).toEqual({ token: "t", server: "s", host: "h", logLevel: "debug" });
  });

  test("ignores unknown keys and empty strings", () => {
    const cfg = parseConfigFile(JSON.stringify({ token: "", extra: 1, host: "h" }), CONFIG_PATH);
    expect(cfg).toEqual({ host: "h" });
  });

  test("rejects invalid JSON", () => {
    expect(() => parseConfigFile("{not json", CONFIG_PATH)).toThrow(ConfigError);
  });

  test("rejects a non-object top level", () => {
    expect(() => parseConfigFile("[]", CONFIG_PATH)).toThrow(ConfigError);
  });

  test("rejects a non-string field", () => {
    expect(() => parseConfigFile(JSON.stringify({ token: 5 }), CONFIG_PATH)).toThrow(ConfigError);
  });

  test("rejects an invalid logLevel", () => {
    expect(() => parseConfigFile(JSON.stringify({ logLevel: "loud" }), CONFIG_PATH)).toThrow(
      ConfigError,
    );
  });
});
