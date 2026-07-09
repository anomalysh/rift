import { describe, expect, test } from "bun:test";

import { parseArgs } from "../src/args.ts";

describe("valid invocations", () => {
  test("protocol and port only", () => {
    const parsed = parseArgs(["http", "3000"]);
    expect(parsed).toEqual({
      kind: "run",
      protocol: "http",
      port: 3000,
      flags: {},
    });
  });

  test("with a subdomain", () => {
    const parsed = parseArgs(["http", "3000", "myapp"]);
    expect(parsed.kind).toBe("run");
    if (parsed.kind === "run") {
      expect(parsed.subdomain).toBe("myapp");
      expect(parsed.port).toBe(3000);
    }
  });

  test("all flags, space-separated", () => {
    const parsed = parseArgs([
      "http",
      "8080",
      "site",
      "--token",
      "tok",
      "--server",
      "wss://gw.example.com",
      "--host",
      "0.0.0.0",
      "--log-level",
      "debug",
      "--insecure",
    ]);
    expect(parsed.kind).toBe("run");
    if (parsed.kind === "run") {
      expect(parsed.subdomain).toBe("site");
      expect(parsed.flags).toEqual({
        token: "tok",
        server: "wss://gw.example.com",
        host: "0.0.0.0",
        logLevel: "debug",
        insecure: true,
      });
    }
  });

  test("flags in --flag=value form", () => {
    const parsed = parseArgs(["http", "3000", "--token=abc", "--server=ws://x"]);
    expect(parsed.kind).toBe("run");
    if (parsed.kind === "run") {
      expect(parsed.flags.token).toBe("abc");
      expect(parsed.flags.server).toBe("ws://x");
    }
  });

  test("flags may precede positionals", () => {
    const parsed = parseArgs(["--token", "abc", "http", "3000"]);
    expect(parsed.kind).toBe("run");
    if (parsed.kind === "run") {
      expect(parsed.port).toBe(3000);
      expect(parsed.flags.token).toBe("abc");
    }
  });

  test("port boundaries 1 and 65535 are accepted", () => {
    expect(parseArgs(["http", "1"]).kind).toBe("run");
    expect(parseArgs(["http", "65535"]).kind).toBe("run");
  });
});

describe("bad port", () => {
  // Every one of these is rejected. "-1" reads as a flag, "" as a missing
  // port; the rest reach the port validator.
  for (const bad of ["0", "70000", "65536", "abc", "-1", "3000.5", "0x10", ""]) {
    test(`rejects port ${JSON.stringify(bad)}`, () => {
      expect(parseArgs(["http", bad]).kind).toBe("error");
    });
  }

  test("out-of-range port names the port and the valid range", () => {
    const parsed = parseArgs(["http", "70000"]);
    expect(parsed.kind).toBe("error");
    if (parsed.kind === "error") {
      expect(parsed.message).toContain("port");
      expect(parsed.message).toContain("65535");
    }
  });
});

describe("bad protocol", () => {
  test("rejects tcp and names what is supported", () => {
    const parsed = parseArgs(["tcp", "3000"]);
    expect(parsed.kind).toBe("error");
    if (parsed.kind === "error") {
      expect(parsed.message).toContain("http");
    }
  });
});

describe("help and version", () => {
  test("--help and -h", () => {
    expect(parseArgs(["--help"]).kind).toBe("help");
    expect(parseArgs(["-h"]).kind).toBe("help");
    expect(parseArgs(["http", "3000", "--help"]).kind).toBe("help");
  });

  test("--version and -v", () => {
    expect(parseArgs(["--version"]).kind).toBe("version");
    expect(parseArgs(["-v"]).kind).toBe("version");
  });
});

describe("other errors", () => {
  test("missing positionals", () => {
    expect(parseArgs([]).kind).toBe("error");
  });

  test("unknown flag", () => {
    const parsed = parseArgs(["http", "3000", "--nope"]);
    expect(parsed.kind).toBe("error");
    if (parsed.kind === "error") {
      expect(parsed.message).toContain("--nope");
    }
  });

  test("value flag missing its value", () => {
    const parsed = parseArgs(["http", "3000", "--token"]);
    expect(parsed.kind).toBe("error");
  });

  test("invalid log level", () => {
    const parsed = parseArgs(["http", "3000", "--log-level", "chatty"]);
    expect(parsed.kind).toBe("error");
    if (parsed.kind === "error") {
      expect(parsed.message).toContain("log-level");
    }
  });

  test("too many positionals", () => {
    const parsed = parseArgs(["http", "3000", "sub", "extra"]);
    expect(parsed.kind).toBe("error");
  });
});
