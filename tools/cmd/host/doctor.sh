#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"
# shellcheck source=tools/lib/preflight.sh
. "$SCRIPT_DIR/../../lib/preflight.sh"

# doctor.sh -- one-shot workstation readiness check for driving rift's operator
# tooling. It collapses the "failed four stages deep" onboarding experience into
# a single report: the commands the pipeline needs, the resources a build needs,
# and whether the untracked .env is present and shaped like .env.example. Purely
# advisory by default (exit 0); --strict makes any problem a non-zero exit so it
# can gate CI or a pre-flight script.

usage() {
	cat >&2 <<'EOF'
Usage: rift-ops host doctor [--strict]

Check this workstation is ready to run rift's deploy/provision/backup tooling:
required commands, build resources, and a present, well-shaped .env. Advisory by
default; --strict exits non-zero if anything is missing or misconfigured.

Options:
  --strict   Exit non-zero on any problem (default: report and exit 0).
EOF
}

strict=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--strict) strict=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

problems=0
ok() { printf '  \033[32mok\033[0m    %s\n' "$*"; }
warn() {
	printf '  \033[33mwarn\033[0m  %s\n' "$*"
	problems=$((problems + 1))
}

# have CMD... — report each command's presence; every miss is a problem.
have() {
	local c
	for c in "$@"; do
		if command -v "$c" >/dev/null 2>&1; then
			ok "$c is installed"
		else
			warn "$c is not on PATH"
		fi
	done
}

printf '=== required commands ===\n'
# The core toolchain the operator scripts shell out to. bun/go come via mise, so
# they are not required here; these are the host commands.
have docker git ssh curl openssl tar sha256sum python3
printf '  (sshpass is only needed before a deploy key exists; jq is optional)\n'

printf '\n=== build resources ===\n'
if require_memory 1024 "a container build" >/dev/null 2>&1; then
	ok "memory: $(rift_mib "$(preflight_mem_available_mb)") available (>= 1 GiB)"
else
	warn "less than 1 GiB memory available; container builds may be killed"
fi
if require_docker_disk 3072 "the e2e image builds" >/dev/null 2>&1; then
	ok "disk: $(rift_mib "$(preflight_disk_free_mb "$(preflight_docker_root)")") free on the docker root"
else
	warn "less than 3 GiB free on the docker root; image builds may fail"
fi

printf '\n=== configuration (.env) ===\n'
env_file="${RIFT_ENV_FILE:-$RIFT_REPO_ROOT/.env}"
example="$RIFT_REPO_ROOT/.env.example"
if [ ! -f "$env_file" ]; then
	warn "no .env at $env_file -- run 'make setup' to generate one"
else
	ok ".env present at $env_file"
	# Name-only parity, in the high-signal direction: a key SET in .env that
	# .env.example does not document (even commented-out) is most likely a typo
	# riftd silently ignores. The reverse -- example keys absent from .env -- is
	# noise, because .env.example intentionally lists every optional tunable.
	# Values are never read here.
	set_keys() { grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$1" 2>/dev/null | sed 's/=$//' | sort -u; }
	known_keys() { grep -oE '^#?[A-Za-z_][A-Za-z0-9_]*=' "$1" 2>/dev/null | sed 's/^#//;s/=$//' | sort -u; }
	if [ -f "$example" ]; then
		unknown="$(comm -23 <(set_keys "$env_file") <(known_keys "$example"))"
		if [ -n "$unknown" ]; then
			warn "keys in your .env that .env.example does not document (typo?):"
			printf '          %s\n' "$unknown"
		else
			ok "every key in .env is one .env.example documents"
		fi
	fi
fi

printf '\n=== dns (advisory) ===\n'
if [ -x "$RIFT_TOOLS_DIR/check-dns.sh" ] && [ -f "$env_file" ]; then
	# check-dns is itself advisory and never fails; surface a one-line summary.
	if bash "$RIFT_TOOLS_DIR/check-dns.sh" >/dev/null 2>&1; then
		ok "check-dns.sh ran (see 'make check-dns' for detail)"
	else
		warn "check-dns.sh reported an issue (run 'make check-dns' for detail)"
	fi
else
	printf '  skipped (needs a .env; run make check-dns manually)\n'
fi

printf '\n=== summary ===\n'
if [ "$problems" -eq 0 ]; then
	log_info "workstation looks ready"
	exit 0
fi
log_warn "$problems item(s) need attention"
[ "$strict" = true ] && exit 1
exit 0
