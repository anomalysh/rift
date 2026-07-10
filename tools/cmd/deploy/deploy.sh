#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"
SSH="$RIFT_TOOLS_DIR/cmd/remote/ssh.sh"

REMOTE_DIR="/opt/rift"

usage() {
	cat >&2 <<EOF
Usage: rift-ops deploy deploy [--dry-run]

Deploy the rift stack to the VPS. Idempotent.
  1. Ships projects/server/ and deploy/ to $REMOTE_DIR (tar over ssh, preserving layout
     so compose's build context '..' resolves to $REMOTE_DIR on the VPS).
  2. Runs: docker compose -f docker-compose.yml -f docker-compose.prod.yml
           up -d --build   (from $REMOTE_DIR/deploy)
     docker-compose.tcp.yml is appended when RIFT_TCP_ENABLED is true in the
     VPS's deploy/.env, and docker-compose.tls.yml when RIFT_TLS_TUNNEL_ENABLED
     is -- publishing exactly the raw-tunnel ports whose feature is on. Open the
     same ports on the firewall too (tools/harden.sh).

Your untracked secrets file must already exist on the VPS at
$REMOTE_DIR/deploy/.env (e.g. tools/scp.sh .env $REMOTE_DIR/deploy/.env). It is
never part of the tarball, so it is preserved across deploys.

Options:
  --dry-run   Print the actions without touching the VPS.
  --plan      Read-only preview: rollback availability, the compose overlays the
              remote .env selects, and how local vs remote .env keys differ.

Environment: RIFT_VPS_HOST (required), RIFT_VPS_USER, RIFT_VPS_PORT, and either
tools/.ssh/id_ed25519 (preferred) or RIFT_VPS_PASSWORD.
EOF
}

dry_run=false
plan=false
case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
--dry-run) dry_run=true ;;
--plan) plan=true ;;
"") ;;
*) die "unexpected argument: $1 (see --help)" ;;
esac

require_cmd ssh tar
require_env RIFT_VPS_HOST

# --plan: a read-only preview (terraform-plan style) -- what a deploy WOULD do,
# touching nothing. Reports the recoverability of the current deploy, which
# compose overlays the remote .env selects, and how the local and remote .env
# key sets differ (a common "I forgot to ship the new var" footgun).
if [ "$plan" = true ]; then
	log_info "=== deploy plan for ${RIFT_VPS_USER:-root}@$RIFT_VPS_HOST:$REMOTE_DIR ==="

	"$SSH" 'docker image inspect rift-riftd:rollback >/dev/null 2>&1 &&
		echo "  rollback: rift-riftd:rollback is present (rollback.sh can restore it)" ||
		echo "  rollback: no saved image yet (first deploy, or none since this feature landed)"' || true

	# Which overlays the remote .env would add, using the same flag logic as the
	# real deploy below.
	"$SSH" "
		cd '$REMOTE_DIR/deploy' 2>/dev/null || { echo '  compose: no $REMOTE_DIR/deploy yet'; exit 0; }
		files='-f docker-compose.yml -f docker-compose.prod.yml'
		val() { [ -f .env ] && sed -n \"s/^[[:space:]]*\$1[[:space:]]*=[[:space:]]*//p\" .env | tail -n1 | tr -d \"\\\"'\\r\"; }
		istrue() { case \"\$(printf %s \"\${1:-}\" | tr A-Z a-z)\" in 1|true|yes|on) return 0;; *) return 1;; esac; }
		istrue \"\$(val RIFT_TCP_ENABLED)\" && files=\"\$files -f docker-compose.tcp.yml\"
		istrue \"\$(val RIFT_TLS_TUNNEL_ENABLED)\" && files=\"\$files -f docker-compose.tls.yml\"
		echo \"  compose: docker compose \$files up -d --build\"
	" || true

	# .env key-set diff, names only (values never leave the boxes).
	remote_keys="$("$SSH" "grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' '$REMOTE_DIR/deploy/.env' 2>/dev/null | sed 's/=\$//' | sort -u" || true)"
	local_env="${RIFT_ENV_FILE:-$RIFT_REPO_ROOT/.env}"
	if [ -f "$local_env" ]; then
		local_keys="$(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' "$local_env" | sed 's/=$//' | sort -u)"
		only_local="$(comm -23 <(printf '%s\n' "$local_keys") <(printf '%s\n' "$remote_keys"))"
		only_remote="$(comm -13 <(printf '%s\n' "$local_keys") <(printf '%s\n' "$remote_keys"))"
		[ -n "$only_local" ] && log_warn "  .env keys set locally but NOT on the VPS:$(printf ' %s' $only_local)"
		[ -n "$only_remote" ] && log_info "  .env keys on the VPS but not in your local .env:$(printf ' %s' $only_remote)"
		[ -z "$only_local$only_remote" ] && log_info "  .env key sets match"
	else
		log_info "  (no local .env to diff at $local_env)"
	fi
	log_info "plan only; nothing was changed"
	exit 0
fi

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
tar_cmd=(tar -C "$RIFT_REPO_ROOT"
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
#
# Which compose files to stack is decided ON THE VPS: the flags that select the
# raw-tunnel overlays live in the untracked $REMOTE_DIR/deploy/.env, which never
# leaves the box. .env is compose's env-file format, not shell — an unquoted
# value containing spaces would break `.` — so the booleans are read out with
# sed rather than sourced. Each feature adds ONLY its own ports, gated on its own
# flag, mirroring how tools/harden.sh opens them; publishing the other feature's
# ports would bind dead host ports the firewall never opens. The overlays must
# come last: docker-compose.prod clears riftd's ports with `ports: !reset []`,
# and a later !reset would wipe the tunnel ports they add.
remote_prelude=$(
	cat <<'SNIPPET'
set -e
rift_env_val() {
	[ -f .env ] || return 0
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" .env | tail -n 1 | tr -d "\"'\r"
}
rift_is_true() {
	case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
	1 | true | yes | on) return 0 ;;
	*) return 1 ;;
	esac
}
compose_files="-f docker-compose.yml -f docker-compose.prod.yml"
if rift_is_true "$(rift_env_val RIFT_TCP_ENABLED)"; then
	compose_files="$compose_files -f docker-compose.tcp.yml"
fi
if rift_is_true "$(rift_env_val RIFT_TLS_TUNNEL_ENABLED)"; then
	compose_files="$compose_files -f docker-compose.tls.yml"
fi
SNIPPET
)

# Before rebuilding, tag the currently-running riftd image as :rollback so
# tools/rollback.sh can restore it if this deploy turns out bad. Best-effort: a
# first-ever deploy has no image yet, which is fine.
compose_up="cd '$REMOTE_DIR/deploy' || exit 1
$remote_prelude
docker image inspect rift-riftd >/dev/null 2>&1 && docker tag rift-riftd rift-riftd:rollback || true
docker compose \$compose_files up -d --build"
if [ "$dry_run" = true ]; then
	log_info "[dry-run] would run on VPS: tag rift-riftd:rollback, then docker compose -f docker-compose.yml -f docker-compose.prod.yml [-f docker-compose.tcp.yml] [-f docker-compose.tls.yml] up -d --build"
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
caddy_restart="cd '$REMOTE_DIR/deploy' || exit 1
$remote_prelude
docker compose \$compose_files restart caddy"
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
