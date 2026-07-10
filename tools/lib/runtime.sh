#!/usr/bin/env bash
# Runtime primitives shared by the rift operator scripts: repo-root resolution,
# the untracked .env loader, a composable cleanup trap, and a dry-run wrapper.
#
# SOURCED, not executed, and only via lib/common.sh (which sources this at the
# end so every script that already sources common.sh gets these for free). Keep
# it side-effect free at source time — it may only DEFINE things. It relies on
# common.sh having already defined log_info/die/is_true and RIFT_TOOLS_DIR.

# Absolute path to the repository root (tools/ lives directly under it). Computed
# once here so scripts stop each re-deriving `REPO_ROOT` by hand.
RIFT_REPO_ROOT="$(cd "$RIFT_TOOLS_DIR/.." && pwd)"

# load_env [FILE] — source the operator's untracked .env into the environment,
# EXPORTED so child processes (docker compose, ssh, the providers) inherit it.
#
# The file to read is FILE, else RIFT_ENV_FILE, else <repo>/.env. A missing file
# is not an error. This is the single loader: before it, four scripts inlined
# their own copy that ignored RIFT_ENV_FILE, so `RIFT_ENV_FILE=... make verify`
# silently read the wrong file. Values in the file overwrite the current
# environment; a caller that must let an explicit env var win should snapshot it
# before calling and re-apply after (see provision.sh).
load_env() {
	local file="${1:-${RIFT_ENV_FILE:-$RIFT_REPO_ROOT/.env}}"
	[ -f "$file" ] || return 0
	log_info "reading $file"
	set -a
	# operator-supplied, not tracked in the repo
	# shellcheck disable=SC1090
	. "$file"
	set +a
}

# --- composable cleanup trap ------------------------------------------------
#
# register_cleanup CMD queues a shell command to run when the script exits, for
# any reason. Commands run in reverse registration order (LIFO, like Go's defer)
# so teardown unwinds setup. The first registration installs one trap covering
# EXIT plus INT/TERM (which exit with the conventional 130/143 and thereby fire
# the EXIT handler) — so a signal can never skip cleanup, the omission that left
# backup.sh trapping EXIT only.
_RIFT_CLEANUP_CMDS=()
_rift_run_cleanup() {
	local status=$? i
	for ((i = ${#_RIFT_CLEANUP_CMDS[@]} - 1; i >= 0; i--)); do
		eval "${_RIFT_CLEANUP_CMDS[$i]}" || true
	done
	return "$status"
}
register_cleanup() {
	if [ "${#_RIFT_CLEANUP_CMDS[@]}" -eq 0 ]; then
		trap _rift_run_cleanup EXIT
		trap 'exit 130' INT
		trap 'exit 143' TERM
	fi
	_RIFT_CLEANUP_CMDS+=("$1")
}

# rift_mktemp_dir [TEMPLATE] — make a temp directory and register its removal, so
# a script never leaks one on an early exit or a signal.
rift_mktemp_dir() {
	local d
	d="$(mktemp -d "${1:-${TMPDIR:-/tmp}/rift.XXXXXX}")"
	register_cleanup "rm -rf \"$d\""
	printf '%s' "$d"
}

# rift_run CMD... — run a mutating command, unless RIFT_DRY_RUN is truthy, in
# which case print what WOULD run and skip it. Namespaced (not `run`) because
# several scripts already define their own `run` with other signatures. Use for
# side-effecting steps; read-only calls run directly.
rift_run() {
	if is_true "${RIFT_DRY_RUN:-}"; then
		log_info "[dry-run] $*"
		return 0
	fi
	"$@"
}

# rift_enable_errtrace — opt-in: on an uncaught error under `set -e`, print the
# line, the command, and its exit status before the shell unwinds. Turns the
# bare "set -e aborted somewhere" into a pointer at the actual failure. Call it
# right after sourcing common.sh in a script being debugged, or set
# RIFT_ERRTRACE=1 in the environment to arm it everywhere.
rift_enable_errtrace() {
	set -o errtrace
	trap 'log_error "failed (exit $?) at line $LINENO: $BASH_COMMAND"' ERR
}
is_true "${RIFT_ERRTRACE:-}" && rift_enable_errtrace || true
