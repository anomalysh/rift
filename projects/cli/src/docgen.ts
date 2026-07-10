// Renderers that turn the CLI_SPEC (cli-spec.ts) into the three user-facing
// artifacts: the `--help` banner, the groff man page, and the bash/zsh/fish
// completion scripts. Because every artifact is derived from the same spec,
// they cannot drift from one another or from the parser.
//
// The groff and shell-script formatting is written by hand on purpose: the CLI
// ships as a single Bun binary with no runtime dependencies, so pulling in a
// man-page or completion library is off the table. The escaping helpers below
// are deliberately conservative — they assume the spec text is authored, not
// attacker-controlled, but still escape the metacharacters that would otherwise
// corrupt each output format.

import { CLI_SPEC, type CliOption, type CliSpec } from "./cli-spec.ts";
import { LOG_LEVELS, type Shell } from "./constants.ts";

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

/** Options that open/shape a tunnel or short-circuit (help/version). */
function runAndMetaOptions(spec: CliSpec): CliOption[] {
  return spec.options.filter((o) => o.kind === "run" || o.kind === "meta");
}

/** The `--set-*` options that persist config and exit. */
function persistOptions(spec: CliSpec): CliOption[] {
  return spec.options.filter((o) => o.kind === "persist");
}

/** Value-taking options split into "enumerated" (completable) and free-form. */
function valueOptions(spec: CliSpec): {
  enumerated: CliOption[];
  freeform: CliOption[];
} {
  const value = spec.options.filter((o) => o.takesValue);
  return {
    enumerated: value.filter((o) => o.values !== undefined),
    freeform: value.filter((o) => o.values === undefined),
  };
}

/** Drop a trailing parenthetical (e.g. the value list) for terse UIs. */
function terseHelp(o: CliOption): string {
  if (o.values === undefined) {
    return o.help;
  }
  return o.help.replace(/\s*\([^)]*\)\s*$/, "");
}

// ---------------------------------------------------------------------------
// --help banner
// ---------------------------------------------------------------------------

/** Render the `--help` / usage banner. */
export function renderHelp(spec: CliSpec = CLI_SPEC): string {
  const lines: string[] = [];
  lines.push(`${spec.name} — ${spec.summary}`);
  lines.push("");
  lines.push("USAGE");
  lines.push(`  ${spec.name} <protocol> <port> [subdomain] [flags]`);
  lines.push("");

  lines.push("EXAMPLES");
  const exWidth = Math.max(...spec.examples.map((e) => e.cmd.length));
  for (const ex of spec.examples) {
    lines.push(`  ${ex.cmd.padEnd(exWidth)}   ${ex.desc}`);
  }
  lines.push("");

  lines.push("ARGUMENTS");
  const posDisplay = (name: string, required: boolean): string =>
    required ? `<${name}>` : `[${name}]`;
  const posWidth = Math.max(
    ...spec.positionals.map((p) => posDisplay(p.name, p.required).length),
  );
  for (const p of spec.positionals) {
    lines.push(
      `  ${posDisplay(p.name, p.required).padEnd(posWidth)}  ${p.help}`,
    );
  }
  lines.push("");

  lines.push("FLAGS");
  const runMeta = runAndMetaOptions(spec);
  const flagDisplay = (o: CliOption): string => {
    let d = o.long;
    if (o.takesValue) {
      d += ` <${o.placeholder ?? "value"}>`;
    }
    if (o.short !== undefined) {
      d += `, ${o.short}`;
    }
    return d;
  };
  const flagWidth = Math.max(...runMeta.map((o) => flagDisplay(o).length));
  for (const o of runMeta) {
    lines.push(`  ${flagDisplay(o).padEnd(flagWidth)}  ${annotatedHelp(o)}`);
  }
  lines.push("");

  lines.push("CONFIG");
  lines.push(
    `  These write ${spec.configFiles[spec.configFiles.length - 1]} and exit (open no tunnel):`,
  );
  const persist = persistOptions(spec);
  const persistWidth = Math.max(...persist.map((o) => flagDisplay(o).length));
  for (const o of persist) {
    lines.push(`  ${flagDisplay(o).padEnd(persistWidth)}  ${o.help}`);
  }
  lines.push("");
  lines.push(
    "  Precedence (highest first): flags > env vars > " +
      `${spec.configFiles[spec.configFiles.length - 1]} > defaults.`,
  );
  lines.push(
    "  token and server have no default; one must be supplied or rift exits with an error.",
  );
  lines.push("");

  lines.push("COMMANDS");
  lines.push(
    `  ${spec.name} completions <bash|zsh|fish>   print a shell completion script`,
  );
  lines.push(
    `  ${spec.name} man                           print the man page (groff)`,
  );

  return lines.join("\n");
}

/** Help text plus a trailing "(env …, default …)" note where applicable. */
function annotatedHelp(o: CliOption): string {
  const notes: string[] = [];
  if (o.env !== undefined) {
    notes.push(`env ${o.env}`);
  }
  if (o.default !== undefined) {
    notes.push(`default ${o.default}`);
  }
  return notes.length > 0 ? `${o.help} (${notes.join(", ")})` : o.help;
}

// ---------------------------------------------------------------------------
// groff man page
// ---------------------------------------------------------------------------

// The .TH "date" field is fixed rather than stamped from the wall clock so that
// regenerating the page (mise run gen-docs, release.sh) is reproducible and does
// not produce spurious diffs. Bump it alongside a release when the surface
// meaningfully changes.
const MAN_DATE = "2026-07-09";

/** Escape a run of text for a groff man-page body. */
function g(text: string): string {
  // Backslash first (it is groff's escape character), then hyphen/minus so
  // option dashes render literally and stay copy-pasteable.
  return text.replace(/\\/g, "\\e").replace(/-/g, "\\-");
}

/** A sentence: capitalized, period-terminated, then groff-escaped. */
function sentence(text: string): string {
  const t = text.charAt(0).toUpperCase() + text.slice(1);
  return g(t.endsWith(".") ? t : `${t}.`);
}

/** Render a single option as a `.TP` entry for the OPTIONS/CONFIG sections. */
function manOption(o: CliOption): string[] {
  const long = g(o.long);
  const out: string[] = [".TP"];
  if (o.takesValue) {
    out.push(`.BI ${long} " ${g(o.placeholder ?? "value")}"`);
  } else if (o.short !== undefined) {
    out.push(`.BR ${long} ", " ${g(o.short)}`);
  } else {
    out.push(`.B ${long}`);
  }
  let body = sentence(o.help);
  if (o.env !== undefined) {
    body += ` Overrides \\fB${g(o.env)}\\fR.`;
  }
  if (o.default !== undefined) {
    body += ` Defaults to \\fB${g(o.default)}\\fR.`;
  }
  out.push(body);
  return out;
}

/** Render the complete groff man page. */
export function renderManPage(spec: CliSpec = CLI_SPEC): string {
  const out: string[] = [];
  out.push(`.\\" Manual page for ${spec.name}. Generated from cli-spec.ts;`);
  out.push(`.\\" regenerate with: ${spec.name} man  (or mise run gen-docs).`);
  out.push(
    `.TH ${spec.name.toUpperCase()} 1 "${MAN_DATE}" "${spec.name} ${spec.version}" "User Commands"`,
  );

  out.push(".SH NAME");
  out.push(`${spec.name} \\- ${g(spec.summary)}`);

  out.push(".SH SYNOPSIS");
  spec.synopsis.forEach((line, idx) => {
    const rest = line.startsWith(`${spec.name} `)
      ? line.slice(spec.name.length + 1)
      : line;
    out.push(`.B ${g(spec.name)}`);
    out.push(`.RI "${g(rest)}"`);
    if (idx < spec.synopsis.length - 1) {
      out.push(".br");
    }
  });

  out.push(".SH DESCRIPTION");
  spec.description.forEach((para, idx) => {
    if (idx > 0) {
      out.push(".PP");
    }
    out.push(g(para));
  });

  out.push(".SH ARGUMENTS");
  for (const p of spec.positionals) {
    out.push(".TP");
    out.push(`.I ${g(p.name)}`);
    out.push(sentence(p.help));
    if (p.name === "protocol") {
      out.push(".RS");
      for (const proto of spec.protocols) {
        out.push(".TP");
        out.push(`.B ${g(proto.name)}`);
        out.push(sentence(proto.blurb));
      }
      out.push(".RE");
    }
  }

  out.push(".SH OPTIONS");
  out.push("Value options accept both");
  out.push(`.BI \\-\\-flag " value"`);
  out.push("and");
  out.push(`.BI \\-\\-flag= value`);
  out.push("forms.");
  for (const o of runAndMetaOptions(spec)) {
    out.push(...manOption(o));
  }

  out.push(".SH CONFIG");
  out.push("The");
  out.push(".B \\-\\-set\\-*");
  out.push("flags write a value to");
  out.push(`.B ${g(spec.configFiles[spec.configFiles.length - 1] ?? "")}`);
  out.push("and exit without opening a tunnel.");
  for (const o of persistOptions(spec)) {
    out.push(...manOption(o));
  }
  out.push(".PP");
  out.push(
    "Precedence, highest first: command\\-line flags, then environment " +
      "variables, then the config file, then built\\-in defaults. " +
      "token and server have no default; a missing one is a hard error.",
  );

  out.push(".SH ENVIRONMENT");
  for (const e of spec.env) {
    out.push(".TP");
    out.push(`.B ${g(e.name)}`);
    out.push(sentence(e.help));
  }

  out.push(".SH FILES");
  spec.configFiles.forEach((file, idx) => {
    out.push(".TP");
    out.push(`.B ${g(file)}`);
    if (idx < spec.configFiles.length - 1) {
      out.push(".PD 0");
    }
  });
  out.push(".PD");
  out.push(
    "JSON config file providing " +
      "token, server, host, and logLevel. " +
      "Unknown keys are ignored; an invalid type or logLevel is a hard error.",
  );

  out.push(".SH EXAMPLES");
  for (const ex of spec.examples) {
    out.push(".TP");
    out.push(`${sentence(ex.desc)}`);
    out.push(`.B ${g(ex.cmd)}`);
  }

  out.push(".SH EXIT STATUS");
  for (const s of spec.exitStatuses) {
    out.push(".TP");
    out.push(`.B ${s.code}`);
    out.push(g(s.meaning));
  }

  out.push(".SH SEE ALSO");
  out.push(g(spec.seeAlso));

  return `${out.join("\n")}\n`;
}

// ---------------------------------------------------------------------------
// Completion scripts
// ---------------------------------------------------------------------------

/** Strip the leading dashes from a flag: "--log-level" -> "log-level". */
function bareFlag(flag: string): string {
  return flag.replace(/^-+/, "");
}

const LEVELS_SPACED = LOG_LEVELS.join(" ");

/** bash completion script. */
export function renderBashCompletion(spec: CliSpec = CLI_SPEC): string {
  const allTokens = spec.options.flatMap((o) =>
    o.short !== undefined ? [o.long, o.short] : [o.long],
  );
  const { enumerated, freeform } = valueOptions(spec);
  const valueFlagLongs = spec.options
    .filter((o) => o.takesValue)
    .map((o) => o.long);
  const protocols = spec.protocols.map((p) => p.name).join(" ");
  const enumCase = enumerated.map((o) => o.long).join("|");
  const freeformCase = freeform.map((o) => o.long).join("|");
  const skipCase = valueFlagLongs.join("|");

  return `# bash completion for ${spec.name}.
#   Generated by \`${spec.name} completions bash\` from projects/cli/src/cli-spec.ts;
#   do not edit by hand. Regenerate with \`mise run gen-docs\`.
#
#   Source directly, or drop into a bash-completion directory, e.g.
#   /usr/share/bash-completion/completions/${spec.name}
_${spec.name}() {
    local cur prev
    cur="\${COMP_WORDS[COMP_CWORD]}"
    prev="\${COMP_WORDS[COMP_CWORD-1]}"
    COMPREPLY=()

    local flags="${allTokens.join(" ")}"
    local protocols="${protocols}"
    local levels="${LEVELS_SPACED}"

    # Value-taking flags: complete their argument, not another flag.
    case "$prev" in
        ${enumCase})
            mapfile -t COMPREPLY < <(compgen -W "$levels" -- "$cur")
            return 0
            ;;
        ${freeformCase})
            # Free-form value; nothing sensible to suggest.
            return 0
            ;;
    esac

    if [[ "$cur" == -* ]]; then
        mapfile -t COMPREPLY < <(compgen -W "$flags" -- "$cur")
        return 0
    fi

    # Count positional (non-flag) words so far, skipping value-flag arguments,
    # to decide which positional the cursor is on: 1=protocol, 2=port, 3=subdomain.
    local i pos=0 skip=0
    for ((i = 1; i < COMP_CWORD; i++)); do
        local w="\${COMP_WORDS[i]}"
        if (( skip )); then
            skip=0
            continue
        fi
        case "$w" in
            ${skipCase})
                skip=1
                ;;
            --*=*|-*)
                ;;
            *)
                ((pos++)) || true
                ;;
        esac
    done

    case "$pos" in
        0)
            mapfile -t COMPREPLY < <(compgen -W "$protocols" -- "$cur")
            ;;
        *)
            # port (2nd) and subdomain (3rd) are free-form.
            ;;
    esac
    return 0
}

complete -F _${spec.name} ${spec.name}
`;
}

/** Escape a description for a zsh `_arguments` bracket. */
function zshDesc(text: string): string {
  // Inside '[...]' a literal ':' ends the message early and ']' closes it.
  return text
    .replace(/[\\]/g, "\\\\")
    .replace(/:/g, "\\:")
    .replace(/[[\]]/g, "");
}

/** zsh completion script (`#compdef`). */
export function renderZshCompletion(spec: CliSpec = CLI_SPEC): string {
  const lines: string[] = [];
  lines.push(`#compdef ${spec.name}`);
  lines.push(`# zsh completion for ${spec.name}.`);
  lines.push(
    `#   Generated by \`${spec.name} completions zsh\` from projects/cli/src/cli-spec.ts;`,
  );
  lines.push("#   do not edit by hand. Regenerate with `mise run gen-docs`.");
  lines.push("#");
  lines.push(`#   Install as _${spec.name} somewhere on $fpath, e.g.`);
  lines.push(`#   /usr/share/zsh/site-functions/_${spec.name}`);
  lines.push(`_${spec.name}() {`);
  lines.push("    _arguments -s -S \\");

  const argLines: string[] = [];
  // Meta flags first: they are exclusive of everything else.
  for (const o of spec.options.filter((x) => x.kind === "meta")) {
    if (o.short !== undefined) {
      argLines.push(`'(- *)'{${o.short},${o.long}}'[${zshDesc(o.help)}]'`);
    } else {
      argLines.push(`'(- *)'${o.long}'[${zshDesc(o.help)}]'`);
    }
  }
  // Remaining options (run + persist), in spec order.
  for (const o of spec.options.filter((x) => x.kind !== "meta")) {
    const desc = zshDesc(terseHelp(o));
    if (!o.takesValue) {
      argLines.push(`'${o.long}[${desc}]'`);
      continue;
    }
    const placeholder = o.placeholder ?? "value";
    let action = "";
    if (o.values !== undefined) {
      action = `(${o.values.join(" ")})`;
    } else if (o.placeholder === "host") {
      action = "_hosts";
    }
    argLines.push(`'${o.long}[${desc}]:${placeholder}:${action}'`);
  }
  // Positionals.
  spec.positionals.forEach((p, idx) => {
    const action =
      p.name === "protocol"
        ? `(${spec.protocols.map((x) => x.name).join(" ")})`
        : "";
    argLines.push(`'${idx + 1}:${p.name}:${action}'`);
  });

  argLines.forEach((line) => {
    lines.push(`        ${line} \\`);
  });
  lines.push("        && return 0");
  lines.push("}");
  lines.push("");
  lines.push(`_${spec.name} "$@"`);
  return `${lines.join("\n")}\n`;
}

/** Escape a description for a fish single-quoted string. */
function fishDesc(text: string): string {
  return text.replace(/\\/g, "\\\\").replace(/'/g, "\\'");
}

/** fish completion script. */
export function renderFishCompletion(spec: CliSpec = CLI_SPEC): string {
  const lines: string[] = [];
  lines.push(`# fish completion for ${spec.name}.`);
  lines.push(
    `#   Generated by \`${spec.name} completions fish\` from projects/cli/src/cli-spec.ts;`,
  );
  lines.push("#   do not edit by hand. Regenerate with `mise run gen-docs`.");
  lines.push("#");
  lines.push(`#   Install into a fish completions dir, e.g.`);
  lines.push(`#   ~/.config/fish/completions/${spec.name}.fish`);
  lines.push("");
  lines.push(`# ${spec.name} takes a protocol, port, and subdomain: no files.`);
  lines.push(`complete -c ${spec.name} -f`);
  lines.push("");

  for (const o of spec.options) {
    const parts = [`complete -c ${spec.name}`];
    if (o.short !== undefined) {
      parts.push(`-s ${bareFlag(o.short)}`);
    }
    parts.push(`-l ${bareFlag(o.long)}`);
    if (o.takesValue) {
      if (o.values !== undefined) {
        parts.push(`-x -a '${fishDesc(o.values.join(" "))}'`);
      } else {
        parts.push("-r");
      }
    }
    parts.push(`-d '${fishDesc(terseHelp(o))}'`);
    lines.push(parts.join(" "));
  }
  lines.push("");
  lines.push("# First positional is the protocol.");
  for (const proto of spec.protocols) {
    lines.push(
      `complete -c ${spec.name} -n 'test (count (commandline -opc)) -eq 1' ` +
        `-a '${fishDesc(proto.name)}' -d '${fishDesc(proto.blurb)}'`,
    );
  }
  return `${lines.join("\n")}\n`;
}

/** Render the completion script for a given shell. */
export function renderCompletion(
  shell: Shell,
  spec: CliSpec = CLI_SPEC,
): string {
  switch (shell) {
    case "bash":
      return renderBashCompletion(spec);
    case "zsh":
      return renderZshCompletion(spec);
    case "fish":
      return renderFishCompletion(spec);
  }
}
