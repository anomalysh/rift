#!/usr/bin/env bash
# tools/e2e-recovery.sh — prove tools/backup.sh and tools/restore.sh actually
# work, in a throwaway Docker stack, never against a real deployment.
#
# It does not merely check that files appeared: it seeds known database rows and
# a fake certificate tree, backs them up, DESTROYS the originals, restores, and
# asserts every value came back byte-for-byte. It also asserts the guards --
# checksum tamper detection, retention pruning, and the --yes requirement.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

COMPOSE_FILE="$REPO_ROOT/deploy/docker-compose.recovery.yml"
PROJECT="rift-recovery"
CADDY_VOLUME="${PROJECT}_caddy_data"
MIGRATIONS_DIR="$REPO_ROOT/projects/server/internal/store/migrations"
ALPINE="alpine:3.20"

# Paths of the seeded fake certificate material, relative to the volume root.
CA_DIR="acme-v02.api.letsencrypt.org-directory"
CERT_REL="caddy/certificates/$CA_DIR/example.com/example.com.crt"
KEY_REL="caddy/certificates/$CA_DIR/example.com/example.com.key"
ACMEKEY_REL="caddy/acme/$CA_DIR/users/default/default.key"

usage() {
	cat >&2 <<EOF
Usage: tools/e2e-recovery.sh [--keep]

Exercise tools/backup.sh and tools/restore.sh end to end in a hermetic Docker
stack (deploy/docker-compose.recovery.yml). Seeds known state, backs it up,
destroys it, restores it, and asserts it all came back -- plus tamper detection,
retention, and the --yes gate.

Options:
  --keep        Leave the recovery stack and its volume up afterwards.
  -h, --help    Show this help and exit.
EOF
}

keep=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--keep) keep=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_cmd docker sha256sum python3

TMP="$(mktemp -d)"
pass=0
fail=0

cleanup() {
	local status=$?
	if [ "$keep" != true ]; then
		compose down -v --remove-orphans >/dev/null 2>&1 || true
		docker volume rm -f "$CADDY_VOLUME" >/dev/null 2>&1 || true
	else
		log_warn "stack left up (--keep); tear down with:"
		log_warn "  docker compose -f $COMPOSE_FILE -p $PROJECT down -v && docker volume rm -f $CADDY_VOLUME"
	fi
	rm -rf "$TMP"
	exit "$status"
}
trap cleanup EXIT INT TERM

compose() { docker compose -f "$COMPOSE_FILE" -p "$PROJECT" "$@"; }

check() {
	local name="$1" got="$2" want="$3"
	if [ "$got" = "$want" ]; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s: got [%s] want [%s]\n' "$name" "$got" "$want"
		fail=$((fail + 1))
	fi
}

# check_ok / check_fail assert a command's exit status, for the guard tests where
# the behaviour under test is "succeeds" or "refuses".
check_ok() {
	local name="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s (command failed, expected success)\n' "$name"
		fail=$((fail + 1))
	fi
}
check_fail() {
	local name="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		printf '    FAIL  %s (command succeeded, expected refusal)\n' "$name"
		fail=$((fail + 1))
	else
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	fi
}

# --- postgres helpers: password stays inside the container, never on an argv ---
wait_pg() {
	for _ in $(seq 1 60); do
		if compose exec -T postgres pg_isready -U rift -d rift >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	die "postgres did not become ready"
}

rsql() {
	compose exec -T -e RSQL="$1" postgres sh -c '
		export PGPASSWORD="$POSTGRES_PASSWORD"
		exec psql -tAX -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$RSQL"'
}

psql_file() {
	compose exec -T postgres sh -c '
		export PGPASSWORD="$POSTGRES_PASSWORD"
		exec psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <"$1"
}

# dump_lists: succeeds only if pg_restore can read a table of contents from the
# dump -- the same readability check backup.sh performs.
dump_lists() { compose exec -T postgres pg_restore --list <"$1" | grep -q .; }

# --- caddy volume helpers (throwaway alpine container) ------------------------
vol_sha() {
	docker run --rm -v "$CADDY_VOLUME:/data:ro" "$ALPINE" sh -c '
		if [ -f "/data/$1" ]; then sha256sum "/data/$1" | cut -d" " -f1; else echo MISSING; fi' _ "$1"
}

seed_caddy() {
	docker run --rm -v "$CADDY_VOLUME:/data" "$ALPINE" sh -c '
		set -e
		mkdir -p "/data/caddy/certificates/'"$CA_DIR"'/example.com"
		mkdir -p "/data/caddy/acme/'"$CA_DIR"'/users/default"
		printf "FAKE-LEAF-CERT-FOR-example.com-do-not-regenerate\n" > "/data/'"$CERT_REL"'"
		printf "FAKE-LEAF-PRIVATE-KEY\n" > "/data/'"$KEY_REL"'"
		printf "FAKE-ACME-ACCOUNT-PRIVATE-KEY-losing-this-costs-a-rate-limit\n" > "/data/'"$ACMEKEY_REL"'"'
}

wipe_caddy() {
	docker run --rm -v "$CADDY_VOLUME:/data" "$ALPINE" \
		sh -c 'find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} +'
}

backup() {
	"$SCRIPT_DIR/backup.sh" --project "$PROJECT" --compose-file "$COMPOSE_FILE" "$@"
}
restore() {
	"$SCRIPT_DIR/restore.sh" --project "$PROJECT" --compose-file "$COMPOSE_FILE" "$@"
}

# ============================================================================
printf '=== rift backup/restore e2e ===\n'

log_info "bringing up the recovery stack"
compose down -v --remove-orphans >/dev/null 2>&1 || true
docker volume rm -f "$CADDY_VOLUME" >/dev/null 2>&1 || true
compose up -d >/dev/null
wait_pg

# --- 1. apply the REAL schema, exactly as riftd's migration runner does -------
printf '\n1. schema + seed\n'
rsql "CREATE TABLE IF NOT EXISTS schema_migrations (
        version bigint PRIMARY KEY,
        applied_at timestamptz NOT NULL DEFAULT now())" >/dev/null
for f in "$MIGRATIONS_DIR"/*.sql; do
	base="$(basename "$f")"
	ver="$((10#${base%%_*}))" # 0001_init.sql -> 1
	psql_file "$f"
	rsql "INSERT INTO schema_migrations (version) VALUES ($ver) ON CONFLICT DO NOTHING" >/dev/null
done
check "schema applied (tokens table exists)" \
	"$(rsql "SELECT to_regclass('public.tokens')::text")" "tokens"
check "schema_migrations recorded" "$(rsql "SELECT max(version)::text FROM schema_migrations")" "1"

# --- 2. seed known rows + a fake certificate tree -----------------------------
rsql "INSERT INTO tokens (id, name, token_hash, max_tunnels, created_at) VALUES
        ('tok_known_1','primary-known-token','hash-known-1',5,'2026-01-02 03:04:05+00'),
        ('tok_known_2','secondary-known-token','hash-known-2',3,'2026-02-03 04:05:06+00')" >/dev/null
rsql "INSERT INTO reservations (subdomain, token_id, note, created_at) VALUES
        ('known-sub-1','tok_known_1','reserved for app one','2026-01-02 03:04:05+00'),
        ('known-sub-2','tok_known_2','reserved for app two','2026-02-03 04:05:06+00')" >/dev/null
seed_caddy
CERT_SHA_BEFORE="$(vol_sha "$CERT_REL")"
ACMEKEY_SHA_BEFORE="$(vol_sha "$ACMEKEY_REL")"
check "seeded cert is readable" "$([ "$CERT_SHA_BEFORE" != MISSING ] && echo yes || echo no)" "yes"

# --- 3. back it up ------------------------------------------------------------
printf '\n2. backup\n'
BACKUPS="$TMP/backups"
backup --dir "$BACKUPS" --retain 7 >"$TMP/backup.out" 2>&1 ||
	{ cat "$TMP/backup.out" >&2; die "backup.sh failed"; }
BDIR="$(find "$BACKUPS" -mindepth 1 -maxdepth 1 -type d -name 'rift-*' | sort | tail -1)"
[ -n "$BDIR" ] || die "no backup directory produced"

DB_FILE="$(grep -m1 '^db_dump=' "$BDIR/MANIFEST" | cut -d= -f2-)"
CADDY_FILE="$(grep -m1 '^caddy_archive=' "$BDIR/MANIFEST" | cut -d= -f2-)"
MAN_DB_SHA="$(grep -m1 '^db_dump_sha256=' "$BDIR/MANIFEST" | cut -d= -f2-)"
MAN_CADDY_SHA="$(grep -m1 '^caddy_archive_sha256=' "$BDIR/MANIFEST" | cut -d= -f2-)"
MAN_SCHEMA="$(grep -m1 '^schema_migrations_version=' "$BDIR/MANIFEST" | cut -d= -f2-)"

check "db dump artifact exists" "$([ -f "$BDIR/$DB_FILE" ] && echo yes || echo no)" "yes"
check "caddy archive artifact exists" "$([ -f "$BDIR/$CADDY_FILE" ] && echo yes || echo no)" "yes"
check "manifest db sha256 matches file" "$MAN_DB_SHA" "$(sha256sum "$BDIR/$DB_FILE" | cut -d' ' -f1)"
check "manifest caddy sha256 matches file" "$MAN_CADDY_SHA" "$(sha256sum "$BDIR/$CADDY_FILE" | cut -d' ' -f1)"
check "manifest records schema version" "$MAN_SCHEMA" "1"
check_ok "pg_restore --list reads the dump" dump_lists "$BDIR/$DB_FILE"

# --- 4. destroy the originals -------------------------------------------------
printf '\n3. destroy\n'
rsql "DROP TABLE IF EXISTS tunnels, reservations, tokens, schema_migrations CASCADE" >/dev/null
wipe_caddy
check "tokens table is gone" \
	"$(rsql "SELECT COALESCE(to_regclass('public.tokens')::text,'MISSING')")" "MISSING"
check "certificate is gone" "$(vol_sha "$CERT_REL")" "MISSING"

# --- 5. restore and assert everything came back -------------------------------
printf '\n4. restore\n'
restore --yes --from "$BDIR" >"$TMP/restore.out" 2>&1 ||
	{ cat "$TMP/restore.out" >&2; die "restore.sh failed"; }
check "token count restored" "$(rsql "SELECT count(*)::text FROM tokens")" "2"
check "token 1 name restored" "$(rsql "SELECT name FROM tokens WHERE id='tok_known_1'")" "primary-known-token"
check "token 1 hash restored" "$(rsql "SELECT token_hash FROM tokens WHERE id='tok_known_1'")" "hash-known-1"
check "token 1 max_tunnels restored" "$(rsql "SELECT max_tunnels::text FROM tokens WHERE id='tok_known_1'")" "5"
check "reservation count restored" "$(rsql "SELECT count(*)::text FROM reservations")" "2"
check "reservation 1 maps to its token" "$(rsql "SELECT token_id FROM reservations WHERE subdomain='known-sub-1'")" "tok_known_1"
check "reservation 1 note restored" "$(rsql "SELECT note FROM reservations WHERE subdomain='known-sub-1'")" "reserved for app one"
check "schema_migrations version restored" "$(rsql "SELECT max(version)::text FROM schema_migrations")" "$MAN_SCHEMA"
check "certificate restored byte-for-byte" "$(vol_sha "$CERT_REL")" "$CERT_SHA_BEFORE"
check "acme account key restored byte-for-byte" "$(vol_sha "$ACMEKEY_REL")" "$ACMEKEY_SHA_BEFORE"

# --- 6. tamper detection: a corrupted dump must be refused --------------------
printf '\n5. tamper detection\n'
TAMPER="$TMP/tampered"
cp -r "$BDIR" "$TAMPER"
python3 - "$TAMPER/$DB_FILE" <<'PY'
import sys
with open(sys.argv[1], "r+b") as f:
    f.seek(64)
    b = f.read(1) or b"\x00"
    f.seek(64)
    f.write(bytes([b[0] ^ 0xFF]))  # flip one byte -> guaranteed sha mismatch
PY
check_fail "restore refuses a tampered dump" restore --yes --from "$TAMPER"
check "database untouched after tamper refusal" "$(rsql "SELECT count(*)::text FROM tokens")" "2"

# --- 7. retention prunes to the newest N -------------------------------------
printf '\n6. retention\n'
RET="$TMP/retain"
mkdir -p "$RET"
for ts in 20200101T000000Z 20200102T000000Z 20200103T000000Z 20200104T000000Z 20200105T000000Z; do
	mkdir -p "$RET/rift-$ts" # N+2 = 5 fake older backups (N=3)
done
backup --dir "$RET" --retain 3 >"$TMP/retain.out" 2>&1 ||
	{ cat "$TMP/retain.out" >&2; die "retention backup failed"; }
survivors="$(find "$RET" -mindepth 1 -maxdepth 1 -type d -name 'rift-*' | wc -l | tr -d ' ')"
check "exactly N=3 backups survive" "$survivors" "3"
check "oldest pruned" "$([ -e "$RET/rift-20200101T000000Z" ] && echo present || echo gone)" "gone"
check "2nd oldest pruned" "$([ -e "$RET/rift-20200102T000000Z" ] && echo present || echo gone)" "gone"
check "3rd oldest pruned" "$([ -e "$RET/rift-20200103T000000Z" ] && echo present || echo gone)" "gone"
check "4th (kept) survives" "$([ -e "$RET/rift-20200104T000000Z" ] && echo present || echo gone)" "present"
check "newest fake survives" "$([ -e "$RET/rift-20200105T000000Z" ] && echo present || echo gone)" "present"

# --- 8. the --yes gate: a non-interactive restore must refuse -----------------
printf '\n7. --yes gate\n'
restore_no_yes() { restore --from "$BDIR" </dev/null; }
check_fail "restore without --yes refuses" restore_no_yes
check "database unchanged after --yes refusal" "$(rsql "SELECT count(*)::text FROM tokens")" "2"

# ============================================================================
printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || die "e2e-recovery failed"
log_info "e2e-recovery passed"
