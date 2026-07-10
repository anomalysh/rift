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

  test("--upstream-insecure is a boolean flag on the run form", () => {
    const parsed = parseArgs(["https", "8443", "--upstream-insecure"]);
    expect(parsed.kind).toBe("run");
    if (parsed.kind === "run") {
      expect(parsed.protocol).toBe("https");
      expect(parsed.flags.upstreamInsecure).toBe(true);
    }
  });

  test("flags in --flag=value form", () => {
    const parsed = parseArgs([
      "http",
      "3000",
      "--token=abc",
      "--server=ws://x",
    ]);
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
  for (const bad of [
    "0",
    "70000",
    "65536",
    "abc",
    "-1",
    "3000.5",
    "0x10",
    "",
  ]) {
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
  test("rejects an unsupported protocol and names what is supported", () => {
    const parsed = parseArgs(["ftp", "3000"]);
    expect(parsed.kind).toBe("error");
    if (parsed.kind === "error") {
      expect(parsed.message).toContain("http");
    }
  });

  test("accepts https, tcp, tls and udp", () => {
    for (const proto of ["https", "tcp", "tls", "udp"] as const) {
      const parsed = parseArgs([proto, "3000"]);
      expect(parsed.kind).toBe("run");
      if (parsed.kind === "run") {
        expect(parsed.protocol).toBe(proto);
      }
    }
  });
});

describe("doc subcommands", () => {
  test("man parses to the man kind", () => {
    expect(parseArgs(["man"]).kind).toBe("man");
  });

  test("man takes no arguments", () => {
    expect(parseArgs(["man", "extra"]).kind).toBe("error");
  });

  test("completions <shell> parses with the shell", () => {
    for (const shell of ["bash", "zsh", "fish"] as const) {
      const parsed = parseArgs(["completions", shell]);
      expect(parsed.kind).toBe("completions");
      if (parsed.kind === "completions") {
        expect(parsed.shell).toBe(shell);
      }
    }
  });

  test("completions with no shell is an error naming the choices", () => {
    const parsed = parseArgs(["completions"]);
    expect(parsed.kind).toBe("error");
    if (parsed.kind === "error") {
      expect(parsed.message).toContain("bash");
    }
  });

  test("completions with an unsupported shell is an error", () => {
    const parsed = parseArgs(["completions", "powershell"]);
    expect(parsed.kind).toBe("error");
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

describe("--set-* config persistence", () => {
  test("--set-token yields set-config with the token update", () => {
    const parsed = parseArgs(["--set-token", "rift_abc"]);
    expect(parsed.kind).toBe("set-config");
    if (parsed.kind === "set-config") {
      expect(parsed.updates).toEqual({ token: "rift_abc" });
    }
  });

  test("--set-*=value form and multiple keys merge", () => {
    const parsed = parseArgs([
      "--set-token=rift_abc",
      "--set-server",
      "wss://gw",
      "--set-log-level=debug",
    ]);
    expect(parsed.kind).toBe("set-config");
    if (parsed.kind === "set-config") {
      expect(parsed.updates).toEqual({
        token: "rift_abc",
        server: "wss://gw",
        logLevel: "debug",
      });
    }
  });

  test("empty --set-token value is an error", () => {
    const parsed = parseArgs(["--set-token", ""]);
    expect(parsed.kind).toBe("error");
  });

  test("invalid --set-log-level is an error", () => {
    const parsed = parseArgs(["--set-log-level", "loud"]);
    expect(parsed.kind).toBe("error");
    if (parsed.kind === "error") {
      expect(parsed.message).toContain("set-log-level");
    }
  });

  test("cannot combine --set-* with a tunnel command", () => {
    const parsed = parseArgs(["http", "3000", "--set-token", "rift_abc"]);
    expect(parsed.kind).toBe("error");
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

describe("custom domains (E1)", () => {
  test("repeated --domain accumulates", () => {
    const parsed = parseArgs([
      "http",
      "3000",
      "--domain",
      "app.acme.com",
      "--domain",
      "www.acme.com",
    ]);
    expect(parsed.kind).toBe("run");
    if (parsed.kind !== "run") return;
    expect(parsed.flags.domain).toEqual(["app.acme.com", "www.acme.com"]);
  });
});
