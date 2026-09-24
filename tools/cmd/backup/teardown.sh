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

usage() {
	cat >&2 <<EOF
Usage: rift-ops backup teardown [--backup] [--yes] [--state-file F] [--dry-run]

Tear down the rift instance recorded in the state file:
  1. (with --backup) back up the stack on the instance and download it to
     ./backups/ first; any backup failure aborts before destroying anything;
  2. destroy the cloud instance (provider DELETE; idempotent);
  3. remove the local state file and stale SSH control sockets.

Options:
  --backup        Back up the instance's stack and pull it to ./backups/ first.
  --yes           Do not prompt for confirmation (for scripts).
  --state-file F  State file to read the instance id from (default: \$RIFT_STATE_FILE,
                  else .rift/state.json).
  --dry-run       Print what would happen, change nothing.

Environment: RIFT_LINODE_TOKEN (or the provider's token) for the destroy call.
EOF
}

do_backup=false assume_yes=false dry_run=false state_file=""
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
state_file="${state_file:-$(rift_state_file)}"

[ -f "$state_file" ] || die "no state file at $state_file -- nothing recorded to tear down"
instance_id="$(rift_state_get "$state_file" instance_id)"
provider="$(rift_state_get "$state_file" provider)"
name="$(rift_state_get "$state_file" name)"
ipv4="$(rift_state_get "$state_file" ipv4)"
[ -n "$instance_id" ] || die "state file $state_file has no instance_id"

log_warn "about to DESTROY instance '$name' (id $instance_id, $ipv4) via provider '${provider:-?}'"
if [ "$assume_yes" != true ] && [ "$dry_run" != true ]; then
	printf 'Type the instance id (%s) to confirm destruction: ' "$instance_id" >&2
	IFS= read -r reply || true
	[ "$reply" = "$instance_id" ] || die "confirmation did not match; aborted"
fi

# 1. Final backup, taken ON the instance (the stack lives there, not here) and
# pulled down to ./backups/ before anything is destroyed. Backups on the VPS
# (/opt/rift/backups) die with it, so a backup that is not safely local is no
# backup: any failure aborts the teardown with the instance untouched.
if [ "$do_backup" = true ]; then
	host="${ipv4:-${RIFT_VPS_HOST:-}}"
	[ -n "$host" ] || die "--backup: no host (state file has no ipv4 and RIFT_VPS_HOST is unset)"
	log_info "taking a final backup on $host"
	rift_run rift_push_tools "$host" ||
		die "could not ship tools/ to $host for the backup; aborted, nothing destroyed"
	rift_run rift_ssh "$host" \
		"bash /opt/rift/tools/cmd/backup/backup.sh" ||
		die "final backup failed on $host; aborted, nothing destroyed"
	if ! is_true "${RIFT_DRY_RUN:-}"; then
		latest="$(rift_ssh "$host" \
			"ls -1d /opt/rift/backups/rift-* 2>/dev/null | tail -n 1")" ||
			die "could not locate the backup on $host; aborted, nothing destroyed"
		[ -n "$latest" ] || die "no backup found on $host after backing up; aborted, nothing destroyed"
		(umask 077 && mkdir -p "$RIFT_REPO_ROOT/backups")
		rift_scp "$host" --pull -r \
			"$latest" "$RIFT_REPO_ROOT/backups/" ||
			die "could not download $latest; aborted, nothing destroyed"
		log_info "final backup saved to $RIFT_REPO_ROOT/backups/$(basename "$latest")"
	fi
fi

# 2. Destroy the instance (idempotent: provision.sh --destroy treats an
# already-gone id as success).
log_info "destroying instance $instance_id"
rift_run bash "$RIFT_TOOLS_DIR/cmd/provision/provision.sh" --destroy "$instance_id" ${provider:+--provider "$provider"}

# 3. Local cleanup: the state file (so a later provision starts clean) and the
# SSH control sockets pointed at a host that no longer exists.
log_info "removing local state and stale SSH control sockets"
rift_run rm -f "$state_file"
rift_run rm -f "$RIFT_SSH_CONTROL_DIR"/* || true

log_info "teardown complete"
