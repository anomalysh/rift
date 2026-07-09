#!/usr/bin/env bash
# tools/restore.sh — restore a rift backup produced by tools/backup.sh.
#
# Restores the Postgres database (pg_restore --clean --if-exists) and/or the
# caddy_data volume (issued TLS certificates + ACME account key) from a backup
# directory. Both operations are DESTRUCTIVE: they overwrite live state.
#
# Every sha256 in the MANIFEST is checked BEFORE anything is touched, and the
# restore refuses to run on any mismatch -- a corrupted or truncated backup must
# never be written over good production data.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=tools/recovery/lib.sh
. "$SCRIPT_DIR/recovery/lib.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

FROM=""
ASSUME_YES=false
DRY_RUN=false
DB_ONLY=false
CERTS_ONLY=false
RIFT_PROJECT="rift"
RIFT_PG_SERVICE="postgres"
CADDY_VOLUME=""
COMPOSE_FILES=()

usage() {
	cat >&2 <<EOF
Usage: tools/restore.sh --from <backup-dir-or-MANIFEST> [options]

Restore a backup written by tools/backup.sh. Verifies every sha256 in the
MANIFEST first and refuses on mismatch.

Required:
  --from PATH            A backup directory, or a MANIFEST file inside one.

Options:
  --yes                  Proceed without the interactive confirmation. REQUIRED
                         when stdin is not a terminal (e.g. from a script). This
                         gate exists because restore OVERWRITES live data.
  --dry-run              Verify and print exactly what would happen; change nothing.
  --database-only        Restore only Postgres.
  --certs-only           Restore only the caddy_data volume.
  --project NAME         Compose project name       (default $RIFT_PROJECT)
  --compose-file FILE    Compose file (repeatable; default deploy/docker-compose.yml)
  --postgres-service N   Postgres service name      (default $RIFT_PG_SERVICE)
  --caddy-volume NAME    Docker volume for caddy_data (default <project>_caddy_data)
  -h, --help             Show this help and exit.
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--from)
		shift
		[ "$#" -gt 0 ] || die "--from needs a value"
		FROM="$1"
		;;
	--yes) ASSUME_YES=true ;;
	--dry-run) DRY_RUN=true ;;
	--database-only) DB_ONLY=true ;;
	--certs-only) CERTS_ONLY=true ;;
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

[ -n "$FROM" ] || die "--from is required (see --help)"
{ [ "$DB_ONLY" = true ] && [ "$CERTS_ONLY" = true ]; } &&
	die "--database-only and --certs-only are mutually exclusive"

require_cmd docker sha256sum tar

[ "${#COMPOSE_FILES[@]}" -gt 0 ] || COMPOSE_FILES=("$REPO_ROOT/deploy/docker-compose.yml")
RIFT_COMPOSE_ARGS=()
for f in "${COMPOSE_FILES[@]}"; do
	[ -f "$f" ] || die "compose file not found: $f"
	RIFT_COMPOSE_ARGS+=(-f "$f")
done
[ -n "$CADDY_VOLUME" ] || CADDY_VOLUME="${RIFT_PROJECT}_caddy_data"

# Resolve --from to a backup directory + its MANIFEST.
if [ -d "$FROM" ]; then
	BDIR="$FROM"
	MANIFEST="$FROM/MANIFEST"
elif [ -f "$FROM" ]; then
	BDIR="$(cd "$(dirname "$FROM")" && pwd)"
	MANIFEST="$FROM"
else
	die "no such backup directory or MANIFEST: $FROM"
fi
[ -f "$MANIFEST" ] || die "MANIFEST not found at $MANIFEST"

DB_FILE="$(rc_manifest_get db_dump "$MANIFEST")"
DB_SHA="$(rc_manifest_get db_dump_sha256 "$MANIFEST")"
CADDY_FILE="$(rc_manifest_get caddy_archive "$MANIFEST")"
CADDY_SHA="$(rc_manifest_get caddy_archive_sha256 "$MANIFEST")"
SCHEMA_VERSION="$(rc_manifest_get schema_migrations_version "$MANIFEST")"
[ -n "$DB_FILE" ] && [ -n "$DB_SHA" ] || die "MANIFEST is missing the database entry"
[ -n "$CADDY_FILE" ] && [ -n "$CADDY_SHA" ] || die "MANIFEST is missing the caddy entry"

DB_PATH="$BDIR/$DB_FILE"
CADDY_PATH="$BDIR/$CADDY_FILE"

do_db=true
do_certs=true
[ "$CERTS_ONLY" = true ] && do_db=false
[ "$DB_ONLY" = true ] && do_certs=false

# Integrity gate: recompute and compare EVERY artifact this run will touch,
# before touching anything. A single mismatch aborts with a non-zero exit and no
# change to the database or the volume.
verify() {
	local path="$1" want="$2" got
	[ -f "$path" ] || die "missing backup artifact: $path"
	got="$(rc_sha256 "$path")"
	[ "$got" = "$want" ] ||
		die "checksum mismatch for $(basename "$path"): manifest=$want actual=$got (refusing to restore)"
	log_info "verified $(basename "$path")"
}
[ "$do_db" = true ] && verify "$DB_PATH" "$DB_SHA"
[ "$do_certs" = true ] && verify "$CADDY_PATH" "$CADDY_SHA"

log_info "backup verified: project=$(rc_manifest_get project "$MANIFEST") schema_migrations=$SCHEMA_VERSION"

if [ "$DRY_RUN" = true ]; then
	printf '\n=== dry run -- nothing will be changed ===\n'
	printf '  target project:  %s\n' "$RIFT_PROJECT"
	printf '  postgres service: %s\n' "$RIFT_PG_SERVICE"
	printf '  caddy volume:    %s\n' "$CADDY_VOLUME"
	[ "$do_db" = true ] && printf '  would pg_restore --clean --if-exists < %s\n' "$DB_PATH"
	[ "$do_certs" = true ] && printf '  would wipe and re-extract %s into volume %s\n' "$CADDY_PATH" "$CADDY_VOLUME"
	exit 0
fi

# Destructive-operation gate. restore.sh overwrites live tokens, reservations
# and TLS certificates, so it refuses to proceed without explicit consent:
# --yes, or an interactive "yes" typed at a terminal. Never assume yes.
if [ "$ASSUME_YES" != true ]; then
	if [ -t 0 ]; then
		printf 'This will OVERWRITE the live database and/or caddy volume of project "%s".\n' "$RIFT_PROJECT" >&2
		printf 'Type "yes" to proceed: ' >&2
		read -r reply
		[ "$reply" = "yes" ] || die "aborted (no confirmation)"
	else
		die "refusing to run without --yes (restore is destructive and stdin is not a terminal)"
	fi
fi

rc_require_pg_running

if [ "$do_db" = true ]; then
	# DB user/name for the *target* stack come from the running container (or an
	# override DSN), NOT from the backup: you may be restoring into a database
	# named differently from the one the dump came from.
	DB_USER=""
	DB_NAME=""
	if [ -n "${RIFT_POSTGRES_DSN:-}" ]; then
		DB_USER="$(rc_dsn_user "$RIFT_POSTGRES_DSN")"
		DB_NAME="$(rc_dsn_db "$RIFT_POSTGRES_DSN")"
	fi
	if [ -z "$DB_USER" ] || [ -z "$DB_NAME" ]; then
		IFS=$'\t' read -r env_user env_db < <(rc_pg_env) || true
		[ -n "$DB_USER" ] || DB_USER="$env_user"
		[ -n "$DB_NAME" ] || DB_NAME="$env_db"
	fi
	[ -n "$DB_USER" ] && [ -n "$DB_NAME" ] || die "could not determine target database user/name"

	log_info "restoring database '$DB_NAME' as '$DB_USER' into project '$RIFT_PROJECT'"
	rc_pg_restore "$DB_USER" "$DB_NAME" <"$DB_PATH"
	log_info "database restored (schema_migrations version $(rc_schema_version "$DB_USER" "$DB_NAME"))"
fi

if [ "$do_certs" = true ]; then
	rc_volume_exists "$CADDY_VOLUME" ||
		die "caddy volume '$CADDY_VOLUME' does not exist (create the stack, or pass --caddy-volume)"
	log_info "restoring caddy volume '$CADDY_VOLUME' from $CADDY_FILE"
	rc_caddy_extract "$CADDY_VOLUME" <"$CADDY_PATH"
	log_info "caddy volume restored"
fi

log_info "restore ok"
