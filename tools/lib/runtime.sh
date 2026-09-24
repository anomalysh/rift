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

# rift_env_keys FILE — the variable names FILE assigns (a leading `export ` is
# allowed), sorted and unique, one per line. Names only: no value is printed.
rift_env_keys() {
	sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}\([A-Za-z_][A-Za-z0-9_]*\)=.*/\2/p' "$1" 2>/dev/null |
		sort -u
}

# load_env [FILE] — source the operator's untracked .env into the environment,
# EXPORTED so child processes (docker compose, ssh, the providers) inherit it.
#
# The file to read is FILE, else RIFT_ENV_FILE, else <repo>/.env. A missing file
# is not an error. This is the single loader: before it, scripts inlined their
# own copy that ignored RIFT_ENV_FILE, and the Makefile pre-sourced ./.env for
# some targets only, so `rift-ops deploy deploy` never read .env at all.
#
# A variable already set to a non-empty value wins over the file, as it does for
# compose's own interpolation: `RIFT_VPS_HOST=1.2.3.4 rift-ops deploy status`
# targets that host, and ship.sh/teardown.sh can hand a sub-script the host from
# the state file without the sub-script's own load_env clobbering it. A file
# already loaded by a parent script is not re-read, so the many ssh.sh calls of
# one deploy stay quiet.
load_env() {
	local _le_file="${1:-${RIFT_ENV_FILE:-$RIFT_REPO_ROOT/.env}}" _le_name _le_i _le_names _le_vals
	[ -f "$_le_file" ] || return 0
	[ "${_RIFT_ENV_LOADED:-}" != "$_le_file" ] || return 0
	log_info "reading $_le_file"
	_le_names=()
	_le_vals=()
	for _le_name in $(rift_env_keys "$_le_file"); do
		if [ -n "${!_le_name:-}" ]; then
			_le_names+=("$_le_name")
			_le_vals+=("${!_le_name}")
		fi
	done
	set -a
	# operator-supplied, not tracked in the repo
	# shellcheck disable=SC1090
	. "$_le_file"
	set +a
	for ((_le_i = 0; _le_i < ${#_le_names[@]}; _le_i++)); do
		export "${_le_names[_le_i]}=${_le_vals[_le_i]}"
	done
	export _RIFT_ENV_LOADED="$_le_file"
}

# rift_state_file — the pipeline state file provision.sh writes and ship.sh and
# teardown.sh read: RIFT_STATE_FILE, else <repo>/.rift/state.json.
rift_state_file() { printf '%s' "${RIFT_STATE_FILE:-$RIFT_REPO_ROOT/.rift/state.json}"; }

# rift_state_get FILE KEY — one top-level field of the JSON state FILE, or empty
# if the file, the field or valid JSON is missing. FILE and KEY reach Python as
# argv, never spliced into its source, so no path can break (or inject into) it.
rift_state_get() {
	[ -f "$1" ] || return 0
	python3 - "$1" "$2" <<'PY' 2>/dev/null || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
print(d.get(sys.argv[2]) or "")
PY
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

# rift_mktemp_dir VAR [TEMPLATE] — make a temp directory, store its path in the
# variable named VAR, and register its removal, so a script never leaks one on an
# early exit or a signal. It assigns by name rather than printing the path
# because `d="$(rift_mktemp_dir)"` would run register_cleanup inside the command
# substitution's subshell, whose EXIT trap deletes the directory the moment the
# substitution returns.
rift_mktemp_dir() {
	local _rift_d
	_rift_d="$(mktemp -d "${2:-${TMPDIR:-/tmp}/rift.XXXXXX}")"
	register_cleanup "rm -rf \"$_rift_d\""
	printf -v "$1" '%s' "$_rift_d"
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
