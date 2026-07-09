import { describe, expect, test } from "bun:test";

import { CLI_SPEC } from "../src/cli-spec.ts";
import {
  renderBashCompletion,
  renderCompletion,
  renderFishCompletion,
  renderHelp,
  renderManPage,
  renderZshCompletion,
} from "../src/docgen.ts";

// The whole point of generating docs from the spec is that they can never omit
// a real part of the CLI surface. These assert the current surface is covered.
const PROTOCOLS = ["http", "https", "tcp", "tls"];
const FLAGS = [
  "--token",
  "--server",
  "--host",
  "--log-level",
  "--insecure",
  "--upstream-insecure",
  "--set-token",
  "--set-server",
  "--set-host",
  "--set-log-level",
];

describe("man page", () => {
  const man = renderManPage();

  test("has the standard groff header sections", () => {
    expect(man).toContain(".TH RIFT 1");
    expect(man).toContain(".SH NAME");
    expect(man).toContain(".SH SYNOPSIS");
    expect(man).toContain(".SH OPTIONS");
  });

  test("documents every protocol and flag (hyphens groff-escaped)", () => {
    for (const p of PROTOCOLS) {
      expect(man).toContain(p);
    }
    for (const f of FLAGS) {
      // groff escapes hyphens as \-, so --set-token appears as \-\-set\-token.
      expect(man).toContain(f.replace(/-/g, "\\-"));
    }
  });
});

describe("shell completions", () => {
  test("bash completes the protocols and every flag", () => {
    const bash = renderBashCompletion();
    for (const p of PROTOCOLS) {
      expect(bash).toContain(p);
    }
    for (const f of FLAGS) {
      expect(bash).toContain(f);
    }
  });

  test("zsh is a compdef with the protocols", () => {
    const zsh = renderZshCompletion();
    expect(zsh).toContain("#compdef rift");
    expect(zsh).toContain("http https tcp tls");
  });

  test("fish registers per-flag completions", () => {
    const fish = renderFishCompletion();
    expect(fish).toContain("complete -c rift");
    expect(fish).toContain("set-token");
  });

  test("renderCompletion dispatches by shell", () => {
    expect(renderCompletion("bash")).toBe(renderBashCompletion());
    expect(renderCompletion("zsh")).toBe(renderZshCompletion());
    expect(renderCompletion("fish")).toBe(renderFishCompletion());
  });
});

describe("help", () => {
  test("renders from the spec and lists protocols + flags", () => {
    const help = renderHelp();
    expect(help).toContain("rift");
    for (const p of PROTOCOLS) {
      expect(help).toContain(p);
    }
    expect(help).toContain("--set-token");
  });
});

describe("spec integrity", () => {
  test("every option has a unique long flag", () => {
    const flags = CLI_SPEC.options.map((o) => o.long);
    expect(new Set(flags).size).toBe(flags.length);
  });
});
