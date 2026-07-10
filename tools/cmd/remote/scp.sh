#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

usage() {
	cat >&2 <<'EOF'
Usage: rift-ops ssh scp [--pull] [-r|--recursive] SRC DST

Copy files to/from the rift VPS using the same auth logic as tools/ssh.sh.
  push (default): copy local SRC -> VPS:DST
  --pull:         copy VPS:SRC  -> local DST

SRC and DST are plain paths; the VPS user@host is supplied from the environment,
so do NOT prefix them with user@host:.

Environment:
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
require_env RIFT_VPS_HOST

host="$RIFT_VPS_HOST"
user="${RIFT_VPS_USER:-root}"
port="${RIFT_VPS_PORT:-22}"

# scp uses uppercase -P for the port.
scp_args=("${RIFT_SSH_OPTS[@]}" -P "$port")
if [ "$recursive" = true ]; then
	scp_args+=(-r)
fi

# Same auth logic as tools/ssh.sh, shared via rift_ssh_auth in lib/common.sh.
auth=() prefix=()
rift_ssh_auth auth prefix
cmd=("${prefix[@]}" scp "${scp_args[@]}" "${auth[@]}")

remote="$user@$host"
case "$direction" in
push) exec "${cmd[@]}" "$src" "$remote:$dst" ;;
pull) exec "${cmd[@]}" "$remote:$src" "$dst" ;;
esac
