#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=tools/lib/e2e-harness.sh
. "$SCRIPT_DIR/lib/e2e-harness.sh"

# smoke.sh -- a near-free regression net over the 23 hand-rolled argument
# parsers in tools/. Every operator script must answer `--help` with exit 0 and a
# non-empty usage message. This catches the whole family of parser breakages --
# a usage() that no longer renders, a `case` that fell through, a source line
# that errors before the help short-circuit -- in one cheap pass, with no Docker
# and no side effects. Run by `make lint` and the CI scripts job.

usage() {
	cat >&2 <<'EOF'
Usage: tools/smoke.sh

Assert every tools/*.sh answers --help with exit 0 and non-empty output. No
containers, no network, no side effects. Exits non-zero if any tool fails.
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
esac

# check_help TOOL -- run `TOOL --help` in a subshell and assert exit 0 with
# output. The subshell contains any stray side effect a broken tool might have.
check_help() {
	local tool="$1" out rc=0
	out="$(bash "$tool" --help 2>&1)" || rc=$?
	if [ "$rc" -ne 0 ]; then
		printf '    FAIL  %s --help exited %d\n' "$tool" "$rc"
		fail=$((fail + 1))
	elif [ -z "$out" ]; then
		printf '    FAIL  %s --help printed nothing\n' "$tool"
		fail=$((fail + 1))
	else
		printf '    ok    %s\n' "$tool"
		pass=$((pass + 1))
	fi
}

printf '=== --help smoke over the operator tools ===\n'
# The root .sh tools, plus every operator command under cmd/<group>/. The e2e-*
# harnesses (also root .sh) answer --help too, so they ride along.
for tool in "$RIFT_TOOLS_DIR"/*.sh "$RIFT_TOOLS_DIR"/cmd/*/*.sh; do
	# smoke.sh itself is included; running its own --help is a fine self-test.
	check_help "$tool"
done
# The rift-ops dispatcher has no .sh extension but is the same contract.
check_help "$RIFT_TOOLS_DIR/rift-ops"

printf '\n=== referenced scripts exist ===\n'
# --help never reaches the code paths that call sibling scripts, so a rename
# (tools/release.sh -> cmd/release/cli.sh, ...) left callers pointing at files
# that no longer exist, failing only in the release, e2e and teardown runs that
# needed them. Resolve every RIFT_TOOLS_DIR- and SCRIPT_DIR-relative .sh path
# literal against the directory it names and assert the file is there.
while IFS=: read -r file _ ref; do
	case "$ref" in
	'$RIFT_TOOLS_DIR/'*) path="$RIFT_TOOLS_DIR/${ref#\$RIFT_TOOLS_DIR/}" ;;
	'$SCRIPT_DIR/'*) path="$(dirname "$file")/${ref#\$SCRIPT_DIR/}" ;;
	*) continue ;;
	esac
	if [ -f "$path" ]; then
		pass=$((pass + 1))
	else
		printf '    FAIL  %s references missing %s\n' "${file#"$RIFT_REPO_ROOT"/}" "$ref"
		fail=$((fail + 1))
	fi
done < <(grep -rnoE '\$(RIFT_TOOLS_DIR|SCRIPT_DIR)/[A-Za-z0-9_./-]+\.sh' \
	"$RIFT_TOOLS_DIR" --include='*.sh' --include=rift-ops)

printf '\n=== documented script paths exist ===\n'
# The same rename left help text, comments, compose files and the manual telling
# operators to run harden.sh, remote-deploy.sh, ssh.sh, ... straight from tools/.
# Every repo-relative tools/<path>.sh mentioned outside .github/ (whose edits go
# through a separate review) must name a file that exists.
while IFS=: read -r file line ref; do
	ref="tools/${ref#*tools/}"
	if [ -f "$RIFT_REPO_ROOT/$ref" ]; then
		pass=$((pass + 1))
	else
		printf '    FAIL  %s:%s mentions missing %s\n' "$file" "$line" "$ref"
		fail=$((fail + 1))
	fi
done < <(cd "$RIFT_REPO_ROOT" && grep -rnoE '(^|[^A-Za-z0-9_./-])tools/[A-Za-z0-9_./-]+\.sh' \
	tools deploy mise-tasks Makefile .env.example README.md RELEASING.md docs projects/manual/src/content \
	2>/dev/null)

print_summary "smoke test"
