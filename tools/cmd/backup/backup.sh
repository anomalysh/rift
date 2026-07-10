#!/usr/bin/env bash
# rift-ops backup backup — back up the rift production stack.
#
# Captures the two pieces of state that cannot be regenerated:
#   * Postgres — every access TOKEN and subdomain RESERVATION. (The `tunnels`
#     table is ephemeral: the reaper collects stale rows and agents reconnect,
#     so its rows are not worth protecting, but the table is dumped anyway.)
#   * caddy_data — issued TLS certificates and the ACME account key. Losing it
#     forces certificate re-issuance and can trip Let's Encrypt's per-week
#     duplicate-certificate rate limit.
#
# Idempotent and safe to run unattended from cron: it writes a fresh timestamped
# backup each run and exits non-zero on any failure, because a cron job that
# fails silently is worse than no cron job.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"
# shellcheck source=tools/recovery/lib.sh
. "$SCRIPT_DIR/../../recovery/lib.sh"

BACKUP_DIR="/opt/rift/backups"
RETAIN=7
RIFT_PROJECT="rift"
RIFT_PG_SERVICE="postgres"
CADDY_VOLUME=""
COMPOSE_FILES=()

usage() {
	cat >&2 <<EOF
Usage: rift-ops backup backup [options]

Back up the rift Postgres database and the Caddy data volume to a timestamped
directory. Runs pg_dump INSIDE the running postgres container (no client version
skew, no password on any argv) and archives caddy_data via a throwaway container.

Options:
  --dir DIR              Destination directory      (default $BACKUP_DIR)
  --retain N             Keep only the newest N backups afterwards (default $RETAIN)
  --project NAME         Compose project name       (default $RIFT_PROJECT)
  --compose-file FILE    Compose file (repeatable; default deploy/docker-compose.yml)
  --postgres-service N   Postgres service name      (default $RIFT_PG_SERVICE)
  --caddy-volume NAME    Docker volume for caddy_data (default <project>_caddy_data)
  -h, --help             Show this help and exit.

Each run writes DIR/rift-<utc-timestamp>/ containing:
  rift-<ts>-db.dump        Postgres custom-format dump (pg_restore --format=custom)
  rift-<ts>-caddy.tar.gz   gzipped tar of the caddy_data volume
  MANIFEST                 timestamp, project, schema version, sha256 of each file

Restore with tools/restore.sh --from DIR/rift-<utc-timestamp>.
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--dir)
		shift
		[ "$#" -gt 0 ] || die "--dir needs a value"
		BACKUP_DIR="$1"
		;;
	--retain)
		shift
		[ "$#" -gt 0 ] || die "--retain needs a value"
		RETAIN="$1"
		;;
	--project)
		shift
		[ "$#" -gt 0 ] || die "--project needs a value"
		RIFT_PROJECT="$1"
		;;
	--compose-file)
		shift
		[ "$#" -gt 0 ] || die "--compose-file needs a value"
		COMPOSE_FILES+=("$1")
		;;
	--postgres-service)
		shift
		[ "$#" -gt 0 ] || die "--postgres-service needs a value"
		RIFT_PG_SERVICE="$1"
		;;
	--caddy-volume)
		shift
		[ "$#" -gt 0 ] || die "--caddy-volume needs a value"
		CADDY_VOLUME="$1"
		;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

case "$RETAIN" in
'' | *[!0-9]*) die "--retain must be a non-negative integer, got: $RETAIN" ;;
esac

require_cmd docker sha256sum tar

rc_resolve_stack "$RIFT_REPO_ROOT/deploy/docker-compose.yml"

rc_require_pg_running

# The dump's user/name: an override DSN, else what the container was built with.
IFS=$'\t' read -r DB_USER DB_NAME < <(rc_resolve_db_identity)

rc_volume_exists "$CADDY_VOLUME" ||
	die "caddy volume '$CADDY_VOLUME' does not exist (pass --caddy-volume if it is named differently)"

TS="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="$BACKUP_DIR/rift-$TS"
DB_FILE="rift-$TS-db.dump"
CADDY_FILE="rift-$TS-caddy.tar.gz"

# Stage into a sibling .partial directory and move it into place only once every
# artifact is written and verified, so an interrupted run never leaves a
# half-written backup that looks complete to restore.sh or to retention.
STAGE="$BACKUP_DIR/.rift-$TS.partial"
mkdir -p "$STAGE"

failed=1
cleanup() {
	# Never prune on failure: a failed run must not delete a prior good backup.
	[ "$failed" -eq 0 ] || {
		rm -rf "$STAGE"
		log_error "backup FAILED; no files kept, retention not run"
	}
}
# EXIT alone would let a Ctrl-C during staging leave a half-written backup dir;
# the signal traps mark the run failed and exit, so the EXIT handler removes it.
trap cleanup EXIT
trap 'failed=1; exit 130' INT
trap 'failed=1; exit 143' TERM

log_info "dumping database '$DB_NAME' as '$DB_USER' from project '$RIFT_PROJECT'"
rc_pg_dump "$DB_USER" "$DB_NAME" >"$STAGE/$DB_FILE"
[ -s "$STAGE/$DB_FILE" ] || die "pg_dump produced an empty file"

log_info "verifying the dump is readable (pg_restore --list)"
rc_pg_dump_verify <"$STAGE/$DB_FILE" >"$STAGE/.toc" 2>/dev/null ||
	die "the dump did not verify -- pg_restore could not read it"
[ -s "$STAGE/.toc" ] || die "the dump verified empty (no objects) -- refusing to trust it"
rm -f "$STAGE/.toc"

log_info "archiving caddy volume '$CADDY_VOLUME'"
rc_caddy_archive "$CADDY_VOLUME" >"$STAGE/$CADDY_FILE"
[ -s "$STAGE/$CADDY_FILE" ] || die "caddy archive produced an empty file"

SCHEMA_VERSION="$(rc_schema_version "$DB_USER" "$DB_NAME")"
DB_SHA="$(rc_sha256 "$STAGE/$DB_FILE")"
CADDY_SHA="$(rc_sha256 "$STAGE/$CADDY_FILE")"

cat >"$STAGE/MANIFEST" <<EOF
# rift backup MANIFEST -- generated by rift-ops backup backup, do not edit.
# Restore with: tools/restore.sh --from <this directory>
manifest_version=1
timestamp=$TS
project=$RIFT_PROJECT
database=$DB_NAME
schema_migrations_version=$SCHEMA_VERSION
db_dump=$DB_FILE
db_dump_sha256=$DB_SHA
caddy_archive=$CADDY_FILE
caddy_archive_sha256=$CADDY_SHA
EOF

# Atomic publish: the fully-formed backup appears at its final path in one step.
mv "$STAGE" "$DEST"
failed=0

DB_BYTES="$(wc -c <"$DEST/$DB_FILE" | tr -d ' ')"
CADDY_BYTES="$(wc -c <"$DEST/$CADDY_FILE" | tr -d ' ')"

printf '\n=== backup complete ===\n'
printf '  %-26s %12s  %s\n' "artifact" "bytes" "sha256"
printf '  %-26s %12s  %s\n' "$DB_FILE" "$DB_BYTES" "$DB_SHA"
printf '  %-26s %12s  %s\n' "$CADDY_FILE" "$CADDY_BYTES" "$CADDY_SHA"
printf '  location: %s\n' "$DEST"
printf '  schema_migrations version: %s\n' "$SCHEMA_VERSION"

# Retention runs only here, past every check above, so a failed backup can never
# prune a good one. Keep the newest RETAIN backups (0 = keep all).
if [ "$RETAIN" -gt 0 ]; then
	mapfile -t all < <(find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -name 'rift-*' -printf '%f\n' | sort -r)
	for ((i = RETAIN; i < ${#all[@]}; i++)); do
		rm -rf -- "${BACKUP_DIR:?}/${all[i]}"
		log_info "pruned old backup ${all[i]}"
	done
fi

log_info "backup ok"
