#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# verify-deploy.sh -- assert a live rift deployment actually works.
#
# This is the gate the two TLS incidents needed: a deploy that "succeeded" but
# left the gateway or the apex without a certificate still handed visitors a
# protocol error. Here every check terminates TLS for real, validating the
# chain against the public trust store (no -k), so a missing certificate fails
# the deploy instead of a user.

usage() {
	cat >&2 <<EOF
Usage: tools/verify-deploy.sh [--host IP] [--base DOMAIN] [--gateway HOST]

Assert that a deployed rift stack serves correctly: TLS is valid on the apex
and the gateway hostname, the apex answers as a non-tunnel, HTTP redirects to
HTTPS, and the admin API is not publicly reachable.

Reads RIFT_BASE_DOMAIN / RIFT_GATEWAY_HOSTNAME / RIFT_VPS_HOST from .env when
not given. Exits non-zero on any failure -- it is meant to gate a pipeline.

Options:
  --host IP        The server address to connect to (default: RIFT_VPS_HOST).
  --base DOMAIN    Base domain (default: RIFT_BASE_DOMAIN).
  --gateway HOST   Gateway hostname (default: RIFT_GATEWAY_HOSTNAME).
  --insecure       Accept an untrusted chain (internal/self TLS modes).
EOF
}

host="" base="" gateway="" insecure=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--host)
		shift
		host="${1:-}"
		;;
	--base)
		shift
		base="${1:-}"
		;;
	--gateway)
		shift
		gateway="${1:-}"
		;;
	--insecure) insecure=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

if [ -f "$REPO_ROOT/.env" ]; then
	set -a
	# operator-supplied, not in the repo
	# shellcheck disable=SC1091
	. "$REPO_ROOT/.env"
	set +a
fi
base="${base:-${RIFT_BASE_DOMAIN:-}}"
gateway="${gateway:-${RIFT_GATEWAY_HOSTNAME:-gateway.$base}}"
host="${host:-${RIFT_VPS_HOST:-}}"
# internal/self certificates are not publicly trusted.
case "${RIFT_TLS_MODE:-}" in internal | self) insecure=true ;; esac

[ -n "$base" ] || die "no base domain (pass --base or set RIFT_BASE_DOMAIN)"
[ -n "$host" ] || die "no host (pass --host or set RIFT_VPS_HOST)"
require_cmd curl openssl

pass=0
fail=0
ok() {
	printf '  ok    %s\n' "$1" >&2
	pass=$((pass + 1))
}
bad() {
	printf '  FAIL  %s\n' "$1" >&2
	fail=$((fail + 1))
}

kflag=()
[ "$insecure" = true ] && kflag=(-k)

# code_for HOST PATH -- HTTP status for a hostname pinned to $host, or a marker.
code_for() {
	curl -s -o /dev/null -w '%{http_code}' --max-time 25 "${kflag[@]}" \
		--resolve "${1}:443:${host}" "https://${1}${2}" 2>/dev/null || echo "TLS-FAIL"
}

cert_ok() {
	echo | timeout 25 openssl s_client -connect "${host}:443" -servername "$1" 2>/dev/null |
		openssl x509 -noout -subject >/dev/null 2>&1
}

log_info "verifying $base on $host (insecure=$insecure)"

# 1. Ports are open.
for p in 80 443; do
	if timeout 6 bash -c "</dev/tcp/${host}/${p}" 2>/dev/null; then
		ok "port $p is open"
	else
		bad "port $p is closed"
	fi
done

# 2. The gateway hostname serves a certificate and answers health. Agents dial
#    this; the first TLS incident left it with no certificate at all.
if cert_ok "$gateway"; then ok "gateway $gateway presents a certificate"; else bad "gateway $gateway has no certificate"; fi
if [ "$(code_for "$gateway" /healthz)" = "200" ]; then ok "gateway health is 200"; else bad "gateway health is not 200"; fi

# 3. The apex has its own certificate (a wildcard does not cover it) and
#    answers as a non-tunnel. The second TLS incident was a missing apex cert.
if cert_ok "$base"; then ok "apex $base presents a certificate"; else bad "apex $base has no certificate"; fi
apex_code="$(code_for "$base" /)"
if [ "$apex_code" = "404" ]; then ok "apex answers 404 (not a tunnel)"; else bad "apex answered $apex_code, expected 404"; fi

# 4. HTTP redirects to HTTPS.
redir="$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 --resolve "${base}:80:${host}" "http://${base}/" 2>/dev/null || echo err)"
if [ "$redir" = "308" ] || [ "$redir" = "301" ] || [ "$redir" = "302" ]; then ok "http redirects to https ($redir)"; else bad "http did not redirect ($redir)"; fi

# 5. The admin API must NOT be reachable from outside. A refused or timed-out
#    connection is the desired result; any HTTP status means the port is exposed.
if admin_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "http://${host}:8082/v1/tokens" 2>/dev/null)"; then
	bad "admin API answered $admin_code on the public interface -- it must never be exposed"
else
	ok "admin API is not publicly reachable"
fi

echo >&2
if [ "$fail" -eq 0 ]; then
	log_info "verify passed ($pass checks)"
else
	die "verify FAILED: $fail of $((pass + fail)) checks failed"
fi
