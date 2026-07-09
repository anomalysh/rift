#!/usr/bin/env bash
# Shared helpers for the rift operator scripts. This file is SOURCED, not
# executed; keep it side-effect free at source time apart from the definitions
# below. Sourcing scripts are expected to run under `set -euo pipefail`.

# Absolute path to the tools/ directory (this file lives in tools/lib/).
RIFT_TOOLS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- logging (always to stderr so stdout carries only real output) ----------
_rift_log() { printf '%s %s\n' "$1" "$2" >&2; }
log_info()  { _rift_log "[info] " "$*"; }
log_warn()  { _rift_log "[warn] " "$*"; }
log_error() { _rift_log "[error]" "$*"; }
die()       { log_error "$*"; exit 1; }

# require_cmd CMD...  — abort unless every command is on PATH.
require_cmd() {
	local missing=0 c
	for c in "$@"; do
		if ! command -v "$c" >/dev/null 2>&1; then
			log_error "required command not found: $c"
			missing=1
		fi
	done
	[ "$missing" -eq 0 ] || die "install the missing command(s) and retry"
}

# require_env VAR...  — abort unless every named variable is set and non-empty.
# Only the NAME is ever printed, never the value, so this is safe for secrets.
require_env() {
	local missing=0 v
	for v in "$@"; do
		if [ -z "${!v:-}" ]; then
			log_error "required environment variable not set: $v"
			missing=1
		fi
	done
	[ "$missing" -eq 0 ] || die "set the missing variable(s) (see .env.example) and retry"
}

# is_true VALUE — succeeds for 1/true/yes/on (case-insensitive).
is_true() {
	case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
	1 | true | yes | on) return 0 ;;
	*) return 1 ;;
	esac
}

# rift_ssh_key_path — path to the managed deploy key (may not exist yet).
rift_ssh_key_path() { printf '%s/.ssh/id_ed25519' "$RIFT_TOOLS_DIR"; }

# Shared ssh/scp options.
#   accept-new records an unknown host key on first contact but still refuses a
#   CHANGED key afterwards (catches a later MITM). ServerAlive* keeps a long
#   deploy session from being dropped by an idle NAT.
# Consumed by the scripts that source this file, hence "unused" here.
# shellcheck disable=SC2034
# Multiplex over one connection. A deploy makes a dozen ssh calls in a few
# seconds, and sshd's MaxStartups drops the later ones with
# "kex_exchange_identification: Connection reset by peer" -- which looks like a
# network fault and is really rate limiting. One master connection avoids it,
# and makes every subsequent call near-instant.
#
# The socket path must stay under the ~104-byte sun_path limit, so it lives in
# the user's home rather than beside the repo.
RIFT_SSH_CONTROL_DIR="${RIFT_SSH_CONTROL_DIR:-$HOME/.ssh/rift-cm}"
mkdir -p "$RIFT_SSH_CONTROL_DIR" 2>/dev/null || true
chmod 700 "$RIFT_SSH_CONTROL_DIR" 2>/dev/null || true

RIFT_SSH_OPTS=(
	-o ConnectTimeout=15
	-o StrictHostKeyChecking=accept-new
	-o ServerAliveInterval=30
	-o ServerAliveCountMax=3
	-o ControlMaster=auto
	-o "ControlPath=$RIFT_SSH_CONTROL_DIR/%r@%h:%p"
	-o ControlPersist=5m
)
