#!/usr/bin/env bash
# Shared assertion, summary, and wait helpers for the rift e2e harnesses:
# tools/e2e.sh and its e2e-*.sh siblings. SOURCED after lib/common.sh; sourcing
# scripts run under `set -euo pipefail`.
#
# Before this, each of the five harnesses re-authored its own check/counter/
# summary/wait spine, and they had drifted (one check_contains lacked the `--`
# guard, summaries differed). One reporter here means every suite counts,
# reports, and summarises identically.

# Assertion tally. Each check bumps exactly one of these.
pass=0
fail=0

# check NAME GOT WANT — pass iff GOT equals WANT (string compare).
check() {
	local name="$1" got="$2" want="$3"
	if [ "$got" = "$want" ]; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s: got [%s] want [%s]\n' "$name" "$got" "$want"
		fail=$((fail + 1))
	fi
}

# check_contains NAME HAYSTACK NEEDLE — pass iff HAYSTACK contains NEEDLE, as a
# fixed (non-regex) substring. `--` guards a NEEDLE that begins with a dash.
check_contains() {
	local name="$1" haystack="$2" needle="$3"
	if printf '%s' "$haystack" | grep -qF -- "$needle"; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s: %q does not contain %q\n' "$name" "$haystack" "$needle"
		fail=$((fail + 1))
	fi
}

# check_ge NAME GOT MIN — pass iff GOT is numeric and >= MIN.
check_ge() {
	local name="$1" got="$2" min="$3"
	if [ "${got:-0}" -ge "$min" ] 2>/dev/null; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s: got [%s] want >= [%s]\n' "$name" "$got" "$min"
		fail=$((fail + 1))
	fi
}

# print_summary LABEL — print the pass/fail tally and exit non-zero (via die) if
# anything failed; otherwise log "<LABEL> passed".
print_summary() {
	printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
	[ "$fail" -eq 0 ] || die "$1 failed"
	log_info "$1 passed"
}

# wait_for_tcp PORT WHAT — block until PORT accepts a TCP connection on
# localhost, or die after ~60s. Uses bash's /dev/tcp, so it needs no nc.
wait_for_tcp() {
	local port="$1" what="$2"
	for _ in $(seq 1 60); do
		if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
			exec 3<&- 3>&-
			return 0
		fi
		sleep 1
	done
	die "$what did not come up on port $port"
}
