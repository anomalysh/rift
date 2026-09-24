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

# rift_gen_secret [N] — N (default 48) random URL-safe characters from the
# kernel CSPRNG. Aborts rather than return a short/predictable value. Shared by
# the setup wizard and secret rotation so both mint identical-strength secrets.
rift_gen_secret() {
	local n="${1:-48}" s
	s="$(
		set +o pipefail
		LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c "$n"
	)"
	[ "${#s}" -eq "$n" ] || die "could not gather $n random characters from /dev/urandom"
	printf '%s' "$s"
}

# rift_ssh_auth AUTH_ARR PREFIX_ARR — resolve VPS auth ONCE for both ssh and scp.
# Populates AUTH_ARR with the auth-selecting ssh/scp options and PREFIX_ARR with
# the command prefix (empty for key auth, `sshpass -e` otherwise). The caller
# keeps the base tool and the port flag (`-p` for ssh, `-P` for scp) explicit, so
# that footgun stays visible at the call site.
#
# `sshpass -e` reads the password from the SSHPASS env var, never argv: `-p pw`
# would expose it in `ps` to every user on the machine.
rift_ssh_auth() {
	local -n __auth="$1" __prefix="$2"
	local key
	key="$(rift_ssh_key_path)"
	if [ -f "$key" ]; then
		__auth=(-i "$key" -o IdentitiesOnly=yes -o PasswordAuthentication=no)
		__prefix=()
	else
		require_cmd sshpass
		require_env RIFT_VPS_PASSWORD
		export SSHPASS="$RIFT_VPS_PASSWORD"
		__auth=(-o PubkeyAuthentication=no)
		__prefix=(sshpass -e)
	fi
}

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

# Consumed by scripts that source this lib (ssh.sh, scp.sh, ...); shellcheck
# cannot see that cross-file use.
# shellcheck disable=SC2034
RIFT_SSH_OPTS=(
	-o ConnectTimeout=15
	-o StrictHostKeyChecking=accept-new
	-o ServerAliveInterval=30
	-o ServerAliveCountMax=3
	-o ControlMaster=auto
	-o "ControlPath=$RIFT_SSH_CONTROL_DIR/%r@%h:%p"
	-o ControlPersist=5m
)

# RIFT_REMOTE_COMPOSE_PRELUDE -- a POSIX-sh snippet, run ON THE VPS from
# /opt/rift/deploy, that sets $compose_files to the compose files this host's
# .env enables. deploy.sh, rollback.sh and rotate.sh all splice it in, so a
# rollback or a secret rotation recreates riftd with exactly the overlays the
# deploy used (dropping docker-compose.tcp.yml there would silently unpublish
# every raw-tunnel port).
#
# .env is compose's env-file format, not shell -- an unquoted value containing
# spaces would break `.` -- so values are read out with sed rather than sourced.
# It strips an optional leading `export `, surrounding quotes and a trailing
# ` # comment`, as compose does, so `RIFT_TCP_ENABLED=true  # on` reads as true.
# Each feature adds ONLY its own ports, gated on its own flag, mirroring how
# harden.sh opens them. The overlays must come last: docker-compose.prod clears
# riftd's ports with `ports: !reset []`, and a later !reset would wipe the
# tunnel ports they add.
# shellcheck disable=SC2034,SC2016
RIFT_REMOTE_COMPOSE_PRELUDE='set -e
rift_env_val() {
	[ -f .env ] || return 0
	sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$1[[:space:]]*=[[:space:]]*//p" .env |
		tail -n 1 | sed "s/[[:space:]]\{1,\}#.*$//" | tr -d "\"'\''\r"
}
rift_is_true() {
	case "$(printf "%s" "${1:-}" | tr "[:upper:]" "[:lower:]")" in
	1 | true | yes | on) return 0 ;;
	*) return 1 ;;
	esac
}
compose_files="-f docker-compose.yml -f docker-compose.prod.yml"
if rift_is_true "$(rift_env_val RIFT_TCP_ENABLED)"; then
	compose_files="$compose_files -f docker-compose.tcp.yml"
fi
if rift_is_true "$(rift_env_val RIFT_TLS_TUNNEL_ENABLED)"; then
	compose_files="$compose_files -f docker-compose.tls.yml"
fi'

# rift_push_tools HOST -- replace /opt/rift/tools on HOST with this checkout's
# tools/, for the scripts that must run ON the box (harden.sh, backup.sh).
#   * It REPLACES rather than copies into: `scp -r tools /opt/rift/tools` onto an
#     existing directory nests a stale tools/tools and runs the old scripts.
#   * It never ships tools/.ssh: that private deploy key is shared by every
#     provisioned host and must not be left on one of them.
#   * Files are extracted as root-owned (--no-same-owner) so a VPS account whose
#     UID happens to match the operator's local UID cannot edit scripts root runs.
rift_push_tools() {
	local host="$1"
	tar -C "$RIFT_REPO_ROOT" --exclude='tools/.ssh' --exclude='__pycache__' -czf - tools |
		env RIFT_VPS_HOST="$host" "$RIFT_TOOLS_DIR/cmd/remote/ssh.sh" \
			"set -e; mkdir -p /opt/rift; rm -rf /opt/rift/tools.new; mkdir /opt/rift/tools.new
tar --no-same-owner -C /opt/rift/tools.new --strip-components=1 -xzf -
rm -rf /opt/rift/tools; mv /opt/rift/tools.new /opt/rift/tools"
}

# Runtime primitives (repo root, load_env, register_cleanup, rift_run). Sourced
# here so every script that sources common.sh gets them without a second source
# line; keep this last, after common.sh's own definitions it depends on.
# shellcheck source=tools/lib/runtime.sh
. "$RIFT_TOOLS_DIR/lib/runtime.sh"
