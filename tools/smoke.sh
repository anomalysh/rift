#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

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

pass=0
fail=0

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

printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || die "smoke test failed"
log_info "smoke test passed"
