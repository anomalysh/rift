#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

REMOTE_DIR="/opt/rift"

# rotate.sh -- rotate a rift secret on the live VPS: mint a fresh value,
# rewrite exactly one key in the remote .env atomically, and restart the service
# that reads it. Closes a real incident-response gap (a leaked admin token today
# means editing .env by hand and hoping). The new secret travels to the box over
# stdin, never on a command line, so it never lands in `ps` or a shell history.

usage() {
	cat >&2 <<EOF
Usage: rift-ops secret rotate <admin|peer> [--yes] [--dry-run]

Rotate a secret in the deployed .env at $REMOTE_DIR/deploy/.env and restart the
service that uses it:
  admin   RIFT_ADMIN_TOKEN  -> restart riftd  (existing tunnel tokens are
          unaffected; any admin API caller must be handed the new token)
  peer    RIFT_PEER_SECRET  -> restart riftd  (multi-node only; rotate on every
          node together, or peers will fail to authenticate to each other)

Options:
  --yes       Do not prompt for confirmation.
  --dry-run   Print what would happen, change nothing.

Postgres password rotation is NOT automated: it must ALTER the database role and
the DSN in lockstep, and a mistake locks riftd out of its own database. Run that
by hand (ALTER USER ... PASSWORD, then update RIFT_POSTGRES_DSN + POSTGRES_PASSWORD).

Environment: RIFT_VPS_HOST (required); see rift-ops ssh ssh --help for auth.
EOF
}

which="" assume_yes=false dry_run=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--yes) assume_yes=true ;;
	--dry-run) dry_run=true ;;
	admin | peer)
		[ -z "$which" ] || die "give exactly one secret to rotate"
		which="$1"
		;;
	postgres)
		die "postgres rotation is not automated (see --help); do it by hand"
		;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

[ -n "$which" ] || {
	usage
	die "name a secret to rotate: admin or peer"
}
load_env
require_env RIFT_VPS_HOST

case "$which" in
admin) key="RIFT_ADMIN_TOKEN" ;;
peer) key="RIFT_PEER_SECRET" ;;
esac

log_warn "rotating $key on $RIFT_VPS_HOST and restarting riftd"
if [ "$assume_yes" != true ] && [ "$dry_run" != true ]; then
	printf 'Proceed? Existing admin API callers will need the new value. [y/N]: ' >&2
	IFS= read -r reply || true
	case "$reply" in y | Y | yes | YES) ;; *) die "aborted" ;; esac
fi

new_secret="$(rift_gen_secret 48)"

if [ "$dry_run" = true ]; then
	log_info "[dry-run] would rewrite $key in $REMOTE_DIR/deploy/.env and restart riftd"
	exit 0
fi

# The remote script reads the key (line 1) and the new value (line 2) from
# STDIN, so neither lands on a command line. It rewrites exactly that key
# atomically: filter the old line out, append the new one, mv into place at mode
# 600. Single-quoted so nothing expands locally; all $vars are the remote shell's.
remote_rewrite='
set -eu
f="/opt/rift/deploy/.env"
[ -f "$f" ] || { echo "no $f on the host" >&2; exit 1; }
IFS= read -r key
IFS= read -r val
tmp="$(mktemp "$f.rotate.XXXXXX")"
chmod 600 "$tmp"
grep -v "^${key}=" "$f" >"$tmp" || true
printf "%s=%s\n" "$key" "$val" >>"$tmp"
mv "$tmp" "$f"
echo "rewrote $key" >&2
'

printf '%s\n%s\n' "$key" "$new_secret" |
	rift_ssh "$RIFT_VPS_HOST" "$remote_rewrite" ||
	die "failed to rewrite $key on the host"

# `docker compose restart` keeps the existing container and so the environment
# it was CREATED with: the old secret would stay live. Only recreating the
# container re-reads .env. Use the deploy's overlay set so the recreate does not
# drop the raw-tunnel ports.
log_info "recreating riftd to pick up the new $key"
recreate_cmd="cd '$REMOTE_DIR/deploy' || exit 1
$RIFT_REMOTE_COMPOSE_PRELUDE
docker compose \$compose_files up -d --no-build --force-recreate riftd"
rift_ssh "$RIFT_VPS_HOST" "$recreate_cmd" ||
	die "riftd recreate failed; the new $key is written but not yet active"

printf '%s\n' "$new_secret" >&2
log_info "$key rotated. The value above is the ONLY copy shown; it is now in $REMOTE_DIR/deploy/.env."
[ "$which" = admin ] && log_warn "hand the new admin token to any admin-API caller; the old one no longer works."
exit 0
