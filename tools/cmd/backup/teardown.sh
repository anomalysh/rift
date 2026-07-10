#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

# teardown.sh -- decommission a rift deployment in one safe command: an optional
# final backup, destroy the cloud instance (idempotent, via the same provider
# code provision.sh uses), then clean up local state so a later provision starts
# fresh. Destructive and irreversible for the instance, so it always confirms
# unless --yes is given, mirroring restore.sh's gate.

STATE_FILE_DEFAULT="$RIFT_REPO_ROOT/.rift/state.json"

usage() {
	cat >&2 <<EOF
Usage: rift-ops backup teardown [--backup] [--yes] [--state-file F] [--dry-run]

Tear down the rift instance recorded in the state file:
  1. (with --backup) take a final backup of the running stack first;
  2. destroy the cloud instance (provider DELETE; idempotent);
  3. remove the local state file and stale SSH control sockets.

Options:
  --backup        Run 'make backup' against the local stack before destroying.
  --yes           Do not prompt for confirmation (for scripts).
  --state-file F  State file to read the instance id from (default: $STATE_FILE_DEFAULT).
  --dry-run       Print what would happen, change nothing.

Environment: RIFT_LINODE_TOKEN (or the provider's token) for the destroy call.
EOF
}

do_backup=false assume_yes=false dry_run=false state_file="$STATE_FILE_DEFAULT"
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--backup) do_backup=true ;;
	--yes) assume_yes=true ;;
	--state-file)
		shift
		state_file="${1:-}"
		;;
	--dry-run) dry_run=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_cmd python3
load_env
export RIFT_DRY_RUN="$dry_run"

# state_get KEY — read one string field from the JSON state file, or empty.
state_get() {
	[ -f "$state_file" ] || return 0
	python3 -c "
import json, sys
try:
    d = json.load(open('$state_file'))
except Exception:
    sys.exit(0)
print(d.get('$1', '') or '')
" 2>/dev/null || true
}

[ -f "$state_file" ] || die "no state file at $state_file -- nothing recorded to tear down"
instance_id="$(state_get instance_id)"
provider="$(state_get provider)"
name="$(state_get name)"
ipv4="$(state_get ipv4)"
[ -n "$instance_id" ] || die "state file $state_file has no instance_id"

log_warn "about to DESTROY instance '$name' (id $instance_id, $ipv4) via provider '${provider:-?}'"
if [ "$assume_yes" != true ] && [ "$dry_run" != true ]; then
	printf 'Type the instance id (%s) to confirm destruction: ' "$instance_id" >&2
	IFS= read -r reply || true
	[ "$reply" = "$instance_id" ] || die "confirmation did not match; aborted"
fi

# 1. Final backup (best effort; a failed backup must not block the destroy the
# operator explicitly asked for, but it is loud about it).
if [ "$do_backup" = true ]; then
	log_info "taking a final backup of the local stack"
	if ! rift_run bash "$RIFT_TOOLS_DIR/backup.sh"; then
		log_warn "final backup failed; continuing with teardown (instance still destroyed)"
	fi
fi

# 2. Destroy the instance (idempotent: provision.sh --destroy treats an
# already-gone id as success).
log_info "destroying instance $instance_id"
rift_run bash "$RIFT_TOOLS_DIR/provision.sh" --destroy "$instance_id" ${provider:+--provider "$provider"}

# 3. Local cleanup: the state file (so a later provision starts clean) and the
# SSH control sockets pointed at a host that no longer exists.
log_info "removing local state and stale SSH control sockets"
rift_run rm -f "$state_file"
rift_run rm -f "${RIFT_SSH_CONTROL_DIR:-$HOME/.ssh/rift-cm}"/* 2>/dev/null || true

log_info "teardown complete"
