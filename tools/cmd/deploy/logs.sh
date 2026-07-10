#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

REMOTE_DIR="/opt/rift"

# logs.sh -- tail the deployed stack's logs over SSH, the first thing anyone
# reaches for during an incident. A thin, safe wrapper over `docker compose
# logs` on the VPS (read-only), so nobody has to remember the -f file list or
# the compose project name. Reuses tools/ssh.sh (and its ControlMaster mux).

usage() {
	cat >&2 <<EOF
Usage: rift-ops deploy logs [--follow] [--tail N] [--since T] [SERVICE]

Show logs from the deployed rift stack at $REMOTE_DIR on the VPS. With no
SERVICE, shows every service (riftd, caddy, postgres, ...).

Options:
  -f, --follow   Stream new log lines (Ctrl-C to stop).
  --tail N       Show the last N lines per service (default: 200).
  --since T      Only lines newer than T (e.g. 10m, 2h, 2026-07-10T00:00:00).
  SERVICE        Limit to one compose service.

Environment: RIFT_VPS_HOST (required); see tools/ssh.sh for auth and the rest.
EOF
}

follow=false tail_n="200" since="" service=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	-f | --follow) follow=true ;;
	--tail)
		shift
		tail_n="${1:-200}"
		;;
	--since)
		shift
		since="${1:-}"
		;;
	-*) die "unknown option: $1 (see --help)" ;;
	*)
		[ -z "$service" ] || die "only one SERVICE may be given (got '$service' and '$1')"
		service="$1"
		;;
	esac
	shift
done

require_env RIFT_VPS_HOST

# Build the remote command. base+prod is the running stack; the tcp/tls overlays
# add only ports, so they change nothing about `logs`. Quote-safe: the args are
# constrained (a service name, a numeric tail, a since token).
remote="cd '$REMOTE_DIR/deploy' && docker compose -f docker-compose.yml -f docker-compose.prod.yml logs"
[ "$follow" = true ] && remote="$remote --follow"
[ -n "$tail_n" ] && remote="$remote --tail '$tail_n'"
[ -n "$since" ] && remote="$remote --since '$since'"
[ -n "$service" ] && remote="$remote '$service'"

exec env RIFT_VPS_HOST="$RIFT_VPS_HOST" "$RIFT_TOOLS_DIR/cmd/remote/ssh.sh" "$remote"
