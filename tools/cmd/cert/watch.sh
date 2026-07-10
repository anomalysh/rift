#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

# cert-watch.sh -- warn before a served TLS certificate expires. Silent cert
# expiry is the likeliest future rift outage: renewal runs unattended, and the
# first sign of a stall is a visitor seeing an expired-cert error. This checks
# the apex and the gateway hostname over a real TLS handshake and reports the
# days remaining. Advisory like check-dns.sh (exit 0), so it is safe to run from
# cron; --strict makes an imminent expiry a non-zero exit for alerting.

usage() {
	cat >&2 <<'EOF'
Usage: rift-ops cert watch [--host IP] [--base DOMAIN] [--gateway HOST]
                           [--days N] [--strict]

Report how long the live TLS certificates have left on the apex and the gateway
hostname. Reads RIFT_VPS_HOST / RIFT_BASE_DOMAIN / RIFT_GATEWAY_HOSTNAME from
.env when not given.

Options:
  --host IP      Server address to connect to (default: RIFT_VPS_HOST).
  --base DOMAIN  Base domain (default: RIFT_BASE_DOMAIN).
  --gateway HOST Gateway hostname (default: RIFT_GATEWAY_HOSTNAME).
  --days N       Warn when fewer than N days remain (default: 30).
  --strict       Exit non-zero if any certificate is under the threshold.
EOF
}

host="" base="" gateway="" threshold=30 strict=false
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
	--days)
		shift
		threshold="${1:-30}"
		;;
	--strict) strict=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_cmd openssl date
load_env

host="${host:-${RIFT_VPS_HOST:-}}"
base="${base:-${RIFT_BASE_DOMAIN:-}}"
gateway="${gateway:-${RIFT_GATEWAY_HOSTNAME:-gateway.$base}}"
[ -n "$host" ] || die "no server address (pass --host or set RIFT_VPS_HOST in .env)"
[ -n "$base" ] || die "no base domain (pass --base or set RIFT_BASE_DOMAIN in .env)"
case "$threshold" in '' | *[!0-9]*) die "--days must be a whole number of days" ;; esac

now="$(date -u +%s)"
under=0

# cert_days SNI — days until the cert served for SNI at $host:443 expires, or
# "unreachable" if the handshake or parse fails.
cert_days() {
	local sni="$1" enddate exp
	enddate="$(printf '' |
		openssl s_client -servername "$sni" -connect "$host:443" 2>/dev/null |
		openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2)"
	[ -n "$enddate" ] || {
		printf 'unreachable'
		return 0
	}
	# GNU date parses OpenSSL's "MON DD HH:MM:SS YYYY GMT"; fall back to BSD date.
	exp="$(date -u -d "$enddate" +%s 2>/dev/null ||
		date -u -j -f '%b %e %T %Y %Z' "$enddate" +%s 2>/dev/null || true)"
	[ -n "$exp" ] || {
		printf 'unparseable'
		return 0
	}
	printf '%d' "$(((exp - now) / 86400))"
}

report() {
	local name="$1" days="$2"
	case "$days" in
	unreachable) log_warn "$name: no certificate served (TLS handshake failed)" ;;
	unparseable) log_warn "$name: could not parse the certificate's expiry" ;;
	*)
		if [ "$days" -lt 0 ]; then
			log_error "$name: certificate EXPIRED $((-days)) day(s) ago"
			under=$((under + 1))
		elif [ "$days" -lt "$threshold" ]; then
			log_warn "$name: certificate expires in $days day(s) (< $threshold)"
			under=$((under + 1))
		else
			log_info "$name: certificate valid for $days more day(s)"
		fi
		;;
	esac
}

log_info "checking certificates served by $host (warn under $threshold days)"
report "apex ($base)" "$(cert_days "$base")"
report "gateway ($gateway)" "$(cert_days "$gateway")"

if [ "$under" -gt 0 ]; then
	[ "$strict" = true ] && die "$under certificate(s) at or under the threshold"
	log_warn "$under certificate(s) need attention soon"
fi
exit 0
