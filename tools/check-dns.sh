#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
	cat >&2 <<EOF
Usage: tools/check-dns.sh [--host IP] [--base DOMAIN] [--gateway HOST]

Check the DNS and reachability facts rift depends on, and REPORT problems --
this never fails the build. It exists so "certificates mysteriously won't
issue" becomes one readable line before you rely on it, especially the
split-horizon and wildcard traps that are invisible until Caddy tries to issue.

Values are read from your untracked .env when not given:
  RIFT_BASE_DOMAIN, RIFT_GATEWAY_HOSTNAME, and (as the expected A target) the
  VPS host if RIFT_VPS_HOST is set.

Options:
  --host IP        Expected A/AAAA target (default: RIFT_VPS_HOST).
  --base DOMAIN    Base domain (default: RIFT_BASE_DOMAIN).
  --gateway HOST   Gateway hostname (default: RIFT_GATEWAY_HOSTNAME).
  --tls-mode MODE  If dns01, also checks the authoritative-vs-public views.

Exit status is 0 unless the arguments themselves are unusable; findings are
warnings, never failures.
EOF
}

host=""
base=""
gateway=""
tls_mode=""
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
	--tls-mode)
		shift
		tls_mode="${1:-}"
		;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

if [ -f "$REPO_ROOT/.env" ]; then
	set -a
	# shellcheck disable=SC1091 -- operator-supplied, not in the repo
	. "$REPO_ROOT/.env"
	set +a
fi

base="${base:-${RIFT_BASE_DOMAIN:-}}"
gateway="${gateway:-${RIFT_GATEWAY_HOSTNAME:-}}"
host="${host:-${RIFT_VPS_HOST:-}}"
tls_mode="${tls_mode:-${RIFT_TLS_MODE:-}}"

[ -n "$base" ] || die "no base domain (pass --base or set RIFT_BASE_DOMAIN)"
require_cmd dig

warnings=0
note_ok() { printf '  ok    %s\n' "$1" >&2; }
note_warn() {
	printf '  WARN  %s\n' "$1" >&2
	warnings=$((warnings + 1))
}

# A public resolver, so what we see is what a public ACME server would see --
# not the operator's possibly-split-horizon local view.
PUBLIC_RESOLVER="1.1.1.1"

resolve() {
	# resolve NAME TYPE [RESOLVER]
	dig +short "${3:+@$3}" "$2" "$1" 2>/dev/null | grep -vE '\.$' | grep . || true
}

log_info "checking DNS for *.$base (expected target: ${host:-<unset>})"

# 1. The wildcard must resolve, or no subdomain works at all.
wildcard_probe="rift-dns-check-probe.$base"
a="$(resolve "$wildcard_probe" A "$PUBLIC_RESOLVER")"
aaaa="$(resolve "$wildcard_probe" AAAA "$PUBLIC_RESOLVER")"
if [ -n "$a" ] || [ -n "$aaaa" ]; then
	note_ok "wildcard *.$base resolves (A: ${a:-none}  AAAA: ${aaaa:-none})"
	if [ -n "$host" ] && [ -n "$a" ] && ! printf '%s\n' "$a" | grep -qF "$host"; then
		note_warn "wildcard A is ${a//$'\n'/ }, not $host -- traffic will not reach this server"
	fi
else
	note_warn "wildcard *.$base does not resolve from $PUBLIC_RESOLVER -- add: *.$base A $host"
fi

# 2. The apex needs its own record; a wildcard does not cover it.
apex_a="$(resolve "$base" A "$PUBLIC_RESOLVER")"
if [ -n "$apex_a" ]; then
	note_ok "apex $base resolves (A: ${apex_a//$'\n'/ })"
else
	note_warn "apex $base has no A record -- it is not covered by the wildcard; add: $base A $host"
fi

# 3. The gateway hostname must resolve; agents dial it.
if [ -n "$gateway" ]; then
	g_a="$(resolve "$gateway" A "$PUBLIC_RESOLVER")"
	if [ -n "$g_a" ]; then
		note_ok "gateway $gateway resolves (A: ${g_a//$'\n'/ })"
	else
		note_warn "gateway $gateway does not resolve -- agents cannot dial it"
	fi
fi

# 4. Split-horizon: the authoritative answer must match the public answer, or
#    Caddy's DNS-01 propagation check (which may use the local view) will never
#    agree with the CA (which uses the public view) and issuance will hang.
if [ "$tls_mode" = "dns01" ]; then
	auth_ns="$(resolve "$base" NS "$PUBLIC_RESOLVER" | head -1)"
	if [ -n "$auth_ns" ]; then
		auth_view="$(resolve "$wildcard_probe" A "$auth_ns")"
		pub_view="$a"
		if [ -n "$auth_view" ] && [ -n "$pub_view" ] && [ "$auth_view" != "$pub_view" ]; then
			note_warn "split-horizon: authoritative $auth_ns answers ${auth_view//$'\n'/ } but $PUBLIC_RESOLVER answers ${pub_view//$'\n'/ }; set RIFT_ACME_DNS_RESOLVERS so the propagation check sees the public view"
		else
			note_ok "authoritative and public views agree (no split-horizon detected)"
		fi
	fi
fi

echo >&2
if [ "$warnings" -eq 0 ]; then
	log_info "DNS looks good"
else
	log_warn "$warnings DNS finding(s) above -- these are warnings, not failures"
fi
# Never non-zero on findings: this is advisory, by design.
exit 0
