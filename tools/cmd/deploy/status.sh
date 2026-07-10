#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

REMOTE_DIR="/opt/rift"

# status.sh -- a read-only, at-a-glance snapshot of the deployed stack: which
# containers are up, their health, and the host's disk/memory headroom. The
# visibility gap between "deploy succeeded" and "is it actually healthy right
# now" -- answered without opening an SSH shell. Read-only; --strict turns an
# unhealthy or down container into a non-zero exit for monitoring.

usage() {
	cat >&2 <<EOF
Usage: rift-ops deploy status [--strict]

Print a live snapshot of the rift stack at $REMOTE_DIR on the VPS: container
state and health, plus host disk and memory headroom. Read-only.

Options:
  --strict   Exit non-zero if any container is not running/healthy.

Environment: RIFT_VPS_HOST (required); see tools/ssh.sh for auth.
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

require_cmd awk
require_env RIFT_VPS_HOST

# One round trip: run a small script on the VPS that prints container state (one
# "name<TAB>state<TAB>health" line per rift container) framed by markers, then
# the host's disk and memory. The compose project is labelled 'rift', so a
# label filter finds the containers without needing the compose files.
remote_script='
set -e
echo "@@CONTAINERS@@"
docker ps -a --filter "label=com.docker.compose.project=rift" \
  --format "{{.Names}}\t{{.State}}\t{{.Status}}" 2>/dev/null || true
echo "@@HOST@@"
printf "disk_root\t%s\n" "$(df -h / | awk "NR==2{print \$4\" free of \"\$2\" (\"\$5\" used)\"}")"
if command -v free >/dev/null 2>&1; then
  printf "memory\t%s\n" "$(free -m | awk "/^Mem:/{print \$7\" MiB available of \"\$2\" MiB\"}")"
fi
'

out="$(env RIFT_VPS_HOST="$RIFT_VPS_HOST" "$RIFT_TOOLS_DIR/cmd/remote/ssh.sh" "$remote_script")" ||
	die "could not reach the VPS (check RIFT_VPS_HOST and your key)"

section="" down=0 seen=0
printf '=== containers ===\n'
while IFS=$'\t' read -r a b c; do
	case "$a" in
	@@CONTAINERS@@)
		section=containers
		continue
		;;
	@@HOST@@)
		section=host
		printf '\n=== host ===\n'
		continue
		;;
	esac
	[ -z "$a" ] && continue
	if [ "$section" = containers ]; then
		seen=$((seen + 1))
		# b is the State (running/exited/...); c is the human Status (may say
		# "(healthy)"/"(unhealthy)").
		if [ "$b" = running ] && [[ "$c" != *unhealthy* ]]; then
			printf '  \033[32mok\033[0m    %-24s %s\n' "$a" "$c"
		else
			printf '  \033[31mdown\033[0m  %-24s %s (%s)\n' "$a" "$c" "$b"
			down=$((down + 1))
		fi
	elif [ "$section" = host ]; then
		printf '  %-12s %s\n' "$a" "$b"
	fi
done <<<"$out"

[ "$seen" -eq 0 ] && printf '  (no rift containers found -- is the stack deployed?)\n'

printf '\n=== summary ===\n'
if [ "$down" -eq 0 ] && [ "$seen" -gt 0 ]; then
	log_info "all $seen container(s) up and healthy"
	exit 0
fi
[ "$seen" -eq 0 ] && { log_warn "no containers running"; }
[ "$down" -gt 0 ] && log_warn "$down container(s) not running/healthy"
[ "$strict" = true ] && exit 1
exit 0
