#!/usr/bin/env bash
# Shared helpers for the rift backup/restore tooling (tools/backup.sh and
# tools/restore.sh). SOURCED, not executed; side-effect free at source time.
# Sourcing scripts run under `set -euo pipefail` and must have already sourced
# tools/lib/common.sh (for log_info/die/require_cmd).
#
# The functions below read their target stack from three globals the sourcing
# script is expected to set before calling them:
#   RIFT_COMPOSE_ARGS  array of `-f <file>` pairs identifying the compose stack
#   RIFT_PROJECT       compose project name
#   RIFT_PG_SERVICE    name of the postgres service inside that stack
# and the throwaway container image used for volume tar work:
#   RIFT_ALPINE_IMAGE  (default alpine:3.20)
RIFT_ALPINE_IMAGE="${RIFT_ALPINE_IMAGE:-alpine:3.20}"

# rc_compose ARGS... — docker compose against the configured stack.
rc_compose() {
	docker compose "${RIFT_COMPOSE_ARGS[@]}" -p "$RIFT_PROJECT" "$@"
}

# rc_resolve_stack DEFAULT_COMPOSE_FILE — turn the caller's COMPOSE_FILES array
# (possibly empty) into RIFT_COMPOSE_ARGS, defaulting to DEFAULT_COMPOSE_FILE,
# and default CADDY_VOLUME to "<project>_caddy_data". backup.sh and restore.sh
# resolve their target stack identically; this is that shared step.
#
# The default is the BASE compose file only: it defines the postgres service
# without the prod overlay's required `RIFT_TLS_MODE:?`, so a cron environment
# with none of the .env set can still reach the running container.
rc_resolve_stack() {
	local default_file="$1" f
	[ "${#COMPOSE_FILES[@]}" -gt 0 ] || COMPOSE_FILES=("$default_file")
	RIFT_COMPOSE_ARGS=()
	for f in "${COMPOSE_FILES[@]}"; do
		[ -f "$f" ] || die "compose file not found: $f"
		RIFT_COMPOSE_ARGS+=(-f "$f")
	done
	[ -n "$CADDY_VOLUME" ] || CADDY_VOLUME="${RIFT_PROJECT}_caddy_data"
}

# rc_resolve_db_identity — print "<user>\t<db>" for the target stack. An explicit
# RIFT_POSTGRES_DSN wins; otherwise the values the running container was
# initialised with (rc_pg_env). Aborts if neither yields a user and a database.
# Used for both dump (source) and restore (target) identity.
rc_resolve_db_identity() {
	local user="" db="" env_user="" env_db=""
	if [ -n "${RIFT_POSTGRES_DSN:-}" ]; then
		user="$(rc_dsn_user "$RIFT_POSTGRES_DSN")"
		db="$(rc_dsn_db "$RIFT_POSTGRES_DSN")"
	fi
	if [ -z "$user" ] || [ -z "$db" ]; then
		IFS=$'\t' read -r env_user env_db < <(rc_pg_env) || true
		[ -n "$user" ] || user="$env_user"
		[ -n "$db" ] || db="$env_db"
	fi
	[ -n "$user" ] && [ -n "$db" ] || die "could not determine database user/name"
	# Trailing newline so a `read` consuming this returns 0 rather than tripping
	# set -e on the EOF-without-newline it would otherwise hit.
	printf '%s\t%s\n' "$user" "$db"
}

# rc_require_pg_running — abort unless the postgres service has a container up.
# A clearer failure than letting `exec` error out three calls later.
rc_require_pg_running() {
	local cid
	cid="$(rc_compose ps -q "$RIFT_PG_SERVICE" 2>/dev/null || true)"
	[ -n "$cid" ] ||
		die "postgres service '$RIFT_PG_SERVICE' is not running in project '$RIFT_PROJECT' (bring the stack up first)"
}

# rc_pg_env — prints "<user>\t<db>" as configured INSIDE the running postgres
# container. This is the "compose environment" source of the DB name and user:
# the values the database was actually initialised with, read from the container
# rather than guessed from the host.
rc_pg_env() {
	rc_compose exec -T "$RIFT_PG_SERVICE" \
		sh -c 'printf "%s\t%s\n" "${POSTGRES_USER:-postgres}" "${POSTGRES_DB:-postgres}"'
}

# The password is NEVER placed on an argv. `ps` on the host would expose it, and
# so would pg_dump's own process line if it were passed as part of a connection
# URI. Instead every psql/pg_dump/pg_restore invocation below expands
# $POSTGRES_PASSWORD *inside the container's own shell*, reading the container's
# environment, so the secret never crosses the host's process table.
#
# DB user/name, by contrast, are not secret; they ride in on -e so an operator
# can override them (e.g. from RIFT_POSTGRES_DSN) without editing this file.

# rc_pg_dump USER DB  (>stdout: a custom-format archive)
# --format=custom so pg_restore can list and selectively restore it.
rc_pg_dump() {
	rc_compose exec -T -e RC_USER="$1" -e RC_DB="$2" "$RIFT_PG_SERVICE" sh -c '
		export PGPASSWORD="${POSTGRES_PASSWORD:-}"
		exec pg_dump \
			-U "${RC_USER:-${POSTGRES_USER:-postgres}}" \
			-d "${RC_DB:-${POSTGRES_DB:-postgres}}" \
			--format=custom --no-owner --no-privileges'
}

# rc_pg_dump_verify  (<stdin: an archive; >stdout: its table of contents)
# `pg_restore --list` parses the archive header and TOC without connecting to a
# database. A dump that will not list will not restore either -- so we read every
# dump once before calling the backup a success. A backup never read is not one.
rc_pg_dump_verify() {
	rc_compose exec -T "$RIFT_PG_SERVICE" pg_restore --list
}

# rc_pg_restore USER DB  (<stdin: an archive)
# --clean --if-exists drops each object before recreating it, so restoring over a
# populated database is idempotent and does not error on already-absent objects.
rc_pg_restore() {
	rc_compose exec -T -e RC_USER="$1" -e RC_DB="$2" "$RIFT_PG_SERVICE" sh -c '
		export PGPASSWORD="${POSTGRES_PASSWORD:-}"
		exec pg_restore --clean --if-exists --no-owner --no-privileges \
			-U "${RC_USER:-${POSTGRES_USER:-postgres}}" \
			-d "${RC_DB:-${POSTGRES_DB:-postgres}}"'
}

# rc_schema_version USER DB — highest applied schema_migrations version, or the
# literal "unknown" if the table is absent (fresh/never-migrated database).
rc_schema_version() {
	local out
	out="$(rc_compose exec -T -e RC_USER="$1" -e RC_DB="$2" "$RIFT_PG_SERVICE" sh -c '
		export PGPASSWORD="${POSTGRES_PASSWORD:-}"
		exec psql -tAX -v ON_ERROR_STOP=1 \
			-U "${RC_USER:-${POSTGRES_USER:-postgres}}" \
			-d "${RC_DB:-${POSTGRES_DB:-postgres}}" \
			-c "SELECT COALESCE(max(version),0) FROM schema_migrations"' 2>/dev/null || true)"
	out="$(printf '%s' "$out" | tr -d '[:space:]')"
	[ -n "$out" ] && printf '%s' "$out" || printf 'unknown'
}

# rc_volume_exists NAME
rc_volume_exists() { docker volume inspect "$1" >/dev/null 2>&1; }

# rc_caddy_archive VOLUME  (>stdout: a gzipped tar of the volume)
# Mounted read-only: archiving must never mutate live certificate data. Streamed
# to stdout so the resulting file is owned by the invoking user, not container
# root.
#
# caddy_data holds every ISSUED TLS CERTIFICATE and the ACME ACCOUNT KEY. Lose it
# and Caddy must re-issue from scratch, which can trip Let's Encrypt's per-week
# duplicate-certificate rate limit -- unforgiving, and the volume people forget.
rc_caddy_archive() {
	docker run --rm -v "$1:/data:ro" "$RIFT_ALPINE_IMAGE" tar czf - -C /data .
}

# rc_caddy_extract VOLUME  (<stdin: a gzipped tar)
# Wipes the volume's contents first so the result is byte-for-byte the archive,
# with no stale files left behind from whatever was there before. Destructive by
# design -- restore.sh only reaches here past its --yes gate.
rc_caddy_extract() {
	docker run --rm -i -v "$1:/data" "$RIFT_ALPINE_IMAGE" sh -c '
		find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} +
		exec tar xzf - -C /data'
}

# rc_sha256 FILE — bare hex digest.
rc_sha256() { sha256sum "$1" | cut -d' ' -f1; }

# rc_manifest_get KEY FILE — value of a `key=value` line, or empty.
rc_manifest_get() { grep -m1 "^$1=" "$2" 2>/dev/null | cut -d= -f2-; }

# rc_dsn_user DSN / rc_dsn_db DSN — pull the user and database out of a
# postgres://user:pass@host:port/db?opts connection string. Best-effort: empty
# output means "not present", and callers fall back to the container's env.
rc_dsn_user() {
	local rest="${1#*://}"
	rest="${rest%%@*}"
	case "$1" in *://*@*) printf '%s' "${rest%%:*}" ;; esac
}
rc_dsn_db() {
	local rest="${1#*://}"
	case "$rest" in
	*/*)
		rest="${rest#*/}"
		rest="${rest%%\?*}"
		printf '%s' "$rest"
		;;
	esac
}
