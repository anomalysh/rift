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

# wait_until TRIES CMD... — run CMD once a second until it succeeds; return 1 if
# it has not after TRIES attempts. The bounded poll every harness uses instead
# of a fixed sleep; the caller decides whether a timeout is fatal.
wait_until() {
	local tries="$1" _
	shift
	for _ in $(seq 1 "$tries"); do
		"$@" && return 0
		sleep 1
	done
	return 1
}

# tcp_open PORT — succeeds if PORT on localhost accepts a TCP connection. Uses
# bash's /dev/tcp, so it needs no nc.
tcp_open() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# wait_for_tcp PORT WHAT — block until PORT accepts a TCP connection on
# localhost, or die after ~60s.
wait_for_tcp() {
	wait_until 60 tcp_open "$1" || die "$2 did not come up on port $1"
}

# e2e_build LOG WHAT — `compose build` (the harness's own compose function),
# output to LOG. BuildKit runs the build inside its own container, which fails
# on hosts whose container runtime is misconfigured (a stale nvidia hook, for
# instance). The legacy builder does not, and produces the same image, so fall
# back to it rather than making the whole harness unusable on such a machine.
e2e_build() {
	local log="$1" what="$2"
	if compose build >"$log" 2>&1; then
		return 0
	fi
	log_warn "buildkit build failed; retrying with the legacy builder"
	if DOCKER_BUILDKIT=0 compose build >>"$log" 2>&1; then
		return 0
	fi
	tail -20 "$log" >&2
	die "could not build $what"
}
