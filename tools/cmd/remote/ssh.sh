#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

usage() {
	cat >&2 <<'EOF'
Usage: rift-ops ssh ssh [REMOTE_COMMAND...]

SSH to the rift VPS. With no arguments this opens an interactive shell; with
arguments it runs them as a remote command and exits with the command's status.

Authentication:
  - If tools/.ssh/id_ed25519 exists, key auth is used (password auth refused).
  - Otherwise it falls back to password auth via sshpass, reading the password
    from RIFT_VPS_PASSWORD.

Environment:
  RIFT_VPS_HOST      (required) VPS hostname or IP
  RIFT_VPS_USER      SSH user            (default: root)
  RIFT_VPS_PORT      SSH port            (default: 22)
  RIFT_VPS_PASSWORD  (required only when no key exists yet)
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
esac

require_cmd ssh
require_env RIFT_VPS_HOST

host="$RIFT_VPS_HOST"
user="${RIFT_VPS_USER:-root}"
port="${RIFT_VPS_PORT:-22}"

ssh_args=("${RIFT_SSH_OPTS[@]}" -p "$port")

# Allocate a TTY only for an interactive session (no remote command). Forcing a
# TTY for a piped/remote command would corrupt binary stdin/stdout.
if [ "$#" -eq 0 ]; then
	ssh_args+=(-t)
fi

# Key auth if the managed key exists, else `sshpass -e` (never `-p`, which would
# leak the password into argv). rift_ssh_auth fills both arrays by nameref.
auth=() prefix=()
rift_ssh_auth auth prefix
exec "${prefix[@]}" ssh "${ssh_args[@]}" "${auth[@]}" "$user@$host" "$@"
