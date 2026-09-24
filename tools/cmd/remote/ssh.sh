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

Environment (each read from the untracked .env when not already set):
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
load_env
require_env RIFT_VPS_HOST

# Key auth if the managed key exists, else `sshpass -e` (never `-p`, which would
# leak the password into argv); see rift_ssh_cmd in lib/common.sh.
rift_ssh_cmd ssh

# Allocate a TTY only for an interactive session (no remote command). Forcing a
# TTY for a piped/remote command would corrupt binary stdin/stdout.
if [ "$#" -eq 0 ]; then
	RIFT_SSH_CMD+=(-t)
fi

exec "${RIFT_SSH_CMD[@]}" "${RIFT_VPS_USER:-root}@$RIFT_VPS_HOST" "$@"
