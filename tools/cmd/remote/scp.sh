#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

usage() {
	cat >&2 <<'EOF'
Usage: rift-ops ssh scp [--pull] [-r|--recursive] SRC DST

Copy files to/from the rift VPS using the same auth logic as `rift-ops ssh ssh`.
  push (default): copy local SRC -> VPS:DST
  --pull:         copy VPS:SRC  -> local DST

SRC and DST are plain paths; the VPS user@host is supplied from the environment,
so do NOT prefix them with user@host:.

Environment (each read from the untracked .env when not already set):
  RIFT_VPS_HOST      (required) VPS hostname or IP
  RIFT_VPS_USER      SSH user            (default: root)
  RIFT_VPS_PORT      SSH port            (default: 22)
  RIFT_VPS_PASSWORD  (required only when no key exists yet)
EOF
}

direction=push
recursive=false
positionals=()

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--pull) direction=pull ;;
	--push) direction=push ;;
	-r | --recursive) recursive=true ;;
	--)
		shift
		while [ "$#" -gt 0 ]; do
			positionals+=("$1")
			shift
		done
		break
		;;
	-*) die "unknown option: $1 (see --help)" ;;
	*) positionals+=("$1") ;;
	esac
	shift
done

[ "${#positionals[@]}" -eq 2 ] || die "expected exactly SRC and DST (see --help)"
src="${positionals[0]}"
dst="${positionals[1]}"

require_cmd scp
load_env
require_env RIFT_VPS_HOST

# Same auth and options as ssh.sh, shared via rift_ssh_cmd in lib/common.sh.
rift_ssh_cmd scp
if [ "$recursive" = true ]; then
	RIFT_SSH_CMD+=(-r)
fi

remote="${RIFT_VPS_USER:-root}@$RIFT_VPS_HOST"
case "$direction" in
push) exec "${RIFT_SSH_CMD[@]}" "$src" "$remote:$dst" ;;
pull) exec "${RIFT_SSH_CMD[@]}" "$remote:$src" "$dst" ;;
esac
