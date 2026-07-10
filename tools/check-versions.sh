#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

# check-versions.sh -- assert the toolchain versions pinned across the repo agree.
#
# The Bun and Go versions are declared in several files (mise.toml, the release
# and CLI Dockerfiles, the release workflow). If they drift, the binary an
# operator installs can be compiled by a different toolchain than CI tested with
# -- a silent, hard-to-trace class of "works in CI, breaks in the wild". This
# reconciles them: it collects each declared value and fails if a tool has more
# than one. Run by `make lint` and the CI scripts job.

usage() {
	cat >&2 <<'EOF'
Usage: tools/check-versions.sh

Verify that the Bun and Go versions pinned across the repo all agree. Exits
non-zero and prints the offending files if any tool has more than one version.
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
esac

cd "$RIFT_REPO_ROOT"

# extract REGEX FILE -- print "value<TAB>file" for the first capture group of
# REGEX in FILE, if the file exists and matches. Best-effort: a missing file or
# no match contributes nothing (a version declared in fewer places is fine; two
# DIFFERENT values is the failure).
extract() {
	local re="$1" file="$2" val
	[ -f "$file" ] || return 0
	val="$(sed -nE "s/.*${re}.*/\1/p" "$file" | head -n 1)"
	[ -n "$val" ] && printf '%s\t%s\n' "$val" "$file"
}

# assert_single TOOL <lines...> -- die if the collected "value<TAB>file" lines
# hold more than one distinct value.
assert_single() {
	local tool="$1" lines="$2" distinct
	if [ -z "$lines" ]; then
		die "$tool: found no version declarations to check (a pattern likely went stale)"
	fi
	distinct="$(printf '%s\n' "$lines" | cut -f1 | sort -u | wc -l)"
	if [ "$distinct" -ne 1 ]; then
		log_error "$tool version drift -- these disagree:"
		printf '%s\n' "$lines" | while IFS=$'\t' read -r v f; do
			log_error "  $v  ($f)"
		done
		die "pin every $tool declaration to one version"
	fi
	log_info "$tool pinned consistently at $(printf '%s\n' "$lines" | head -n1 | cut -f1)"
}

# --- Bun -------------------------------------------------------------------
bun_lines="$(
	extract 'bun = "([0-9]+\.[0-9]+\.[0-9]+)"' mise.toml
	extract 'ARG BUN_VERSION=([0-9]+\.[0-9]+\.[0-9]+)' deploy/Dockerfile.release
	extract 'FROM oven\/bun:([0-9]+\.[0-9]+\.[0-9]+)' deploy/Dockerfile.cli
	extract 'bun-version: ([0-9]+\.[0-9]+\.[0-9]+)' .github/workflows/release.yml
)"
assert_single Bun "$bun_lines"

# --- Go --------------------------------------------------------------------
# mise pins the minor (1.25); the Dockerfile tags the same minor. Compare on the
# major.minor so a patch-tagged base image does not read as drift.
go_lines="$(
	extract 'go = "([0-9]+\.[0-9]+)' mise.toml
	extract 'FROM golang:([0-9]+\.[0-9]+)' deploy/Dockerfile
)"
assert_single Go "$go_lines"

log_info "toolchain versions are consistent"
