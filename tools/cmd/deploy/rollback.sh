#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

REMOTE_DIR="/opt/rift"

# rollback.sh -- restore the previous riftd image after a bad deploy. Each
# remote-deploy tags the running image :rollback before it rebuilds, so this
# just re-points riftd at that tag and restarts it WITHOUT a rebuild, then
# re-runs the verify gate. Turns "verify failed" from a dead-end alarm into a
# one-command recovery. Single-container, so this is a fast rollback, not a
# zero-downtime one -- riftd blips while it restarts.

usage() {
	cat >&2 <<EOF
Usage: rift-ops deploy rollback [--yes] [--no-verify]

Roll the deployed riftd back to the image saved (:rollback) by the last deploy,
restart it without rebuilding, and re-run tools/verify-deploy.sh.

Options:
  --yes         Do not prompt for confirmation.
  --no-verify   Skip the post-rollback verify gate.

Environment: RIFT_VPS_HOST (required); see tools/ssh.sh for auth. verify reads
RIFT_BASE_DOMAIN / RIFT_GATEWAY_HOSTNAME from .env.
EOF
}

assume_yes=false do_verify=true
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--yes) assume_yes=true ;;
	--no-verify) do_verify=false ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_env RIFT_VPS_HOST
load_env

# Fail early if there is no rollback image to restore.
if ! env RIFT_VPS_HOST="$RIFT_VPS_HOST" "$RIFT_TOOLS_DIR/cmd/remote/ssh.sh" \
	"docker image inspect rift-riftd:rollback >/dev/null 2>&1"; then
	die "no rollback image on the host (rift-riftd:rollback). A deploy must run first to save one."
fi

log_warn "about to roll riftd back to the previous image on $RIFT_VPS_HOST"
if [ "$assume_yes" != true ]; then
	printf 'Proceed with rollback? [y/N]: ' >&2
	IFS= read -r reply || true
	case "$reply" in y | Y | yes | YES) ;; *) die "aborted" ;; esac
fi

# Re-point the deploy tag at the saved image and restart riftd without a rebuild,
# so compose uses the restored image rather than recompiling the current source.
rollback_cmd="set -e
cd '$REMOTE_DIR/deploy'
docker tag rift-riftd:rollback rift-riftd
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --no-build --force-recreate riftd"
log_info "restoring rift-riftd:rollback and restarting riftd"
env RIFT_VPS_HOST="$RIFT_VPS_HOST" "$RIFT_TOOLS_DIR/cmd/remote/ssh.sh" "$rollback_cmd" ||
	die "rollback failed while restarting riftd"

if [ "$do_verify" = true ] && [ -x "$RIFT_TOOLS_DIR/cmd/deploy/verify.sh" ]; then
	log_info "verifying the rolled-back deployment"
	if bash "$RIFT_TOOLS_DIR/cmd/deploy/verify.sh"; then
		log_info "rollback complete and verified"
	else
		log_warn "riftd was rolled back, but verify-deploy reported problems -- investigate"
		exit 1
	fi
else
	log_info "rollback complete (verify skipped)"
fi
