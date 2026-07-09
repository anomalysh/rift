#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SSH="$SCRIPT_DIR/ssh.sh"

REMOTE_DIR="/opt/rift"

usage() {
	cat >&2 <<EOF
Usage: tools/remote-deploy.sh [--dry-run]

Deploy the rift stack to the VPS. Idempotent.
  1. Ships projects/server/ and deploy/ to $REMOTE_DIR (tar over ssh, preserving layout
     so compose's build context '..' resolves to $REMOTE_DIR on the VPS).
  2. Runs: docker compose -f docker-compose.yml -f docker-compose.prod.yml
           up -d --build   (from $REMOTE_DIR/deploy)

Your untracked secrets file must already exist on the VPS at
$REMOTE_DIR/deploy/.env (e.g. tools/scp.sh .env $REMOTE_DIR/deploy/.env). It is
never part of the tarball, so it is preserved across deploys.

Options:
  --dry-run   Print the actions without touching the VPS.

Environment: RIFT_VPS_HOST (required), RIFT_VPS_USER, RIFT_VPS_PORT, and either
tools/.ssh/id_ed25519 (preferred) or RIFT_VPS_PASSWORD.
EOF
}

dry_run=false
case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
--dry-run) dry_run=true ;;
"") ;;
*) die "unexpected argument: $1 (see --help)" ;;
esac

require_cmd ssh tar
require_env RIFT_VPS_HOST

log_info "target: ${RIFT_VPS_USER:-root}@$RIFT_VPS_HOST:$REMOTE_DIR (dry-run=$dry_run)"

# 1. Ensure the remote layout exists.
if [ "$dry_run" = true ]; then
	log_info "[dry-run] would ensure $REMOTE_DIR/deploy exists"
else
	"$SSH" "mkdir -p '$REMOTE_DIR/deploy'"
fi

# 2. Best-effort check that the operator's secrets file is present.
if [ "$dry_run" != true ]; then
	if "$SSH" "test -f '$REMOTE_DIR/deploy/.env'"; then
		log_info "found $REMOTE_DIR/deploy/.env"
	else
		log_warn "no $REMOTE_DIR/deploy/.env on the VPS; compose will fail on required vars."
		log_warn "Create it first, e.g.: tools/scp.sh .env $REMOTE_DIR/deploy/.env"
	fi
fi

# 3. Ship projects/server/ and deploy/ (excluding local-only artifacts). The remote .env
#    is not in our tree, so extraction overlays files without clobbering it.
tar_cmd=(tar -C "$REPO_ROOT"
	--exclude='deploy/caddy/data'
	--exclude='deploy/caddy/config'
	--exclude='deploy/.env'
	--exclude='projects/server/riftd'
	--exclude='projects/server/bin'
	-czf - projects/server deploy)

if [ "$dry_run" = true ]; then
	log_info "[dry-run] would run: ${tar_cmd[*]} | ssh 'tar -C $REMOTE_DIR -xzf -'"
else
	log_info "syncing projects/server/ and deploy/ to $REMOTE_DIR"
	"${tar_cmd[@]}" | "$SSH" "tar -C '$REMOTE_DIR' -xzf -"
fi

# 4. Build and (re)start the stack.
compose_up="cd '$REMOTE_DIR/deploy' && docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build"
if [ "$dry_run" = true ]; then
	log_info "[dry-run] would run on VPS: $compose_up"
else
	log_info "building and starting the stack on the VPS"
	"$SSH" "$compose_up"
fi

# 5. Reload Caddy.
#
# The Caddyfile is a bind mount, so `compose up` sees an unchanged service spec
# and leaves Caddy running with its old configuration. A Caddyfile edit would
# deploy silently and never take effect. `caddy reload` applies it gracefully,
# keeping the certificate cache and dropping no connections; a restart is the
# fallback when the admin API is unreachable.
caddy_reload="docker exec rift-caddy-1 caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile"
caddy_restart="cd '$REMOTE_DIR/deploy' && docker compose -f docker-compose.yml -f docker-compose.prod.yml restart caddy"
if [ "$dry_run" = true ]; then
	log_info "[dry-run] would reload Caddy: $caddy_reload"
else
	log_info "reloading Caddy configuration"
	if ! "$SSH" "$caddy_reload"; then
		log_warn "caddy reload failed; restarting the container instead"
		"$SSH" "$caddy_restart"
	fi
fi

log_info "deploy complete"
