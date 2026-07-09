#!/usr/bin/env bash
# Linode (API v4) provider for tools/provision.sh. SOURCED, not executed: this
# file only defines functions. See tools/providers/README.md for the contract.
#
# Endpoints used:
#   POST   /v4/linode/instances        create
#   GET    /v4/linode/instances/{id}   status / addresses
#   DELETE /v4/linode/instances/{id}   destroy (404 == already gone == success)
#   GET    /v4/linode/instances        list

LINODE_DEFAULT_API_BASE="https://api.linode.com"

# Canned responses returned in --dry-run so provision.sh can walk the full flow
# without any network call. Shapes match the real API's relevant fields.
_LINODE_CANNED_CREATE='{"id":90001,"status":"provisioning"}'
_LINODE_CANNED_INSTANCE='{"status":"running","ipv4":["203.0.113.10"],"ipv6":"2001:db8::10/128"}'
_LINODE_CANNED_LIST='{"data":[]}'

# --- tiny JSON readers (stdlib python; JSON on stdin, selector in argv) -------
# The selector is script-internal, never operator input.
_LINODE_FIELD_PY='
import sys, json
field = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
if not isinstance(d, dict):
    d = {}
if field == "ipv4":
    a = d.get("ipv4") or []
    v = a[0] if a else ""
elif field == "ipv6":
    v = (d.get("ipv6") or "").split("/")[0]
else:
    v = d.get(field, "")
sys.stdout.write("" if v is None else str(v))
'

_LINODE_LIST_PY='
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    d = {}
for it in (d.get("data") or []):
    a = it.get("ipv4") or []
    ipv4 = a[0] if a else ""
    sys.stdout.write("\t".join([
        str(it.get("id", "")),
        str(it.get("label", "")),
        str(it.get("status", "")),
        str(ipv4),
    ]) + "\n")
'

# Builds only the JSON request body. Non-secret fields arrive as argv; the
# root_pass is read from the environment so it never lands in argv (and thus
# never in `ps`).
_LINODE_BODY_PY='
import os, sys, json
label, region, itype, image, pubkey = sys.argv[1:6]
sys.stdout.write(json.dumps({
    "label": label,
    "region": region,
    "type": itype,
    "image": image,
    "authorized_keys": [pubkey],
    "root_pass": os.environ["RIFT_LINODE_ROOT_PASS"],
    "booted": True,
}, separators=(",", ":")))
'

_linode_field() { python3 -c "$_LINODE_FIELD_PY" "$1"; }

_linode_base() { printf '%s' "${RIFT_PROVIDER_API_BASE:-$LINODE_DEFAULT_API_BASE}"; }

# curl config directive carrying just the bearer token, fed on stdin. Kept out
# of argv on purpose.
_linode_auth_config() {
	printf 'header = "Authorization: Bearer %s"\n' "${RIFT_LINODE_TOKEN:-}"
}

# Linode rejects a create without a root_pass even when only key auth is wanted.
# We generate a long random one, never print it, and never persist it: it lives
# only inside the request body (a shell variable piped to curl) and is discarded
# the moment the call returns. The deploy key is the sole credential that lasts.
_linode_gen_root_pass() { openssl rand -base64 33; }

# _linode_request METHOD PATH CANNED_JSON
# Reads a curl --config file from stdin (auth header, and for POST the body), so
# no secret ever appears in argv. Prints the HTTP status on the FIRST line and
# the response body on the following lines. The status travels in the output
# rather than a variable because callers invoke this through a pipeline (`cfg |
# _linode_request`), which bash runs in a subshell -- a variable set there would
# never reach the caller. In dry-run it prints the call and the canned body and
# touches no network.
_linode_request() {
	local method="$1" path="$2" canned="${3:-}"
	local url
	url="$(_linode_base)/v4$path"

	if is_true "${RIFT_PROVIDER_DRY_RUN:-}"; then
		# Drain any config piped in by the caller, so the producing printf never
		# writes to a closed pipe (a SIGPIPE that pipefail would turn fatal).
		cat >/dev/null 2>&1 || true
		log_info "[dry-run] would call: $method $url"
		printf '200\n%s' "$canned"
		return 0
	fi

	local tmp code
	tmp="$(mktemp)"
	code="$(curl --silent --show-error --config - \
		--request "$method" \
		--output "$tmp" \
		--write-out '%{http_code}' \
		"$url" || true)"
	printf '%s\n' "${code:-000}"
	cat "$tmp"
	rm -f "$tmp"
}

# Split "status\nbody" (a _linode_request result) with no subshell, so the
# status survives. Sets _R_STATUS and _R_BODY.
_linode_split() {
	local out="$1"
	_R_STATUS="${out%%$'\n'*}"
	if [ "$out" = "$_R_STATUS" ]; then
		_R_BODY=""
	else
		_R_BODY="${out#*$'\n'}"
	fi
}

# Turn a non-2xx status into a clear, fatal error. A 401 is called out because
# it is what a missing or wrong RIFT_LINODE_TOKEN produces.
_linode_check() {
	local ctx="$1" st="$2"
	case "$st" in
	2*) return 0 ;;
	401) die "linode: $ctx rejected with 401 -- check RIFT_LINODE_TOKEN" ;;
	*) die "linode: $ctx failed (HTTP $st)" ;;
	esac
}

provider_name() { printf 'linode\n'; }

provider_require_env() {
	require_cmd curl python3 openssl
	# The token is deliberately NOT hard-required here. The API answers a missing
	# or wrong token with 401, which is authoritative and lets the e2e prove the
	# header is really sent; we only nudge the operator.
	[ -n "${RIFT_LINODE_TOKEN:-}" ] ||
		log_warn "RIFT_LINODE_TOKEN is empty; the Linode API will answer 401"
}

# _linode_get ID CANNED -> prints the instance body; dies on a non-2xx status.
_linode_get() {
	local id="$1" canned="$2" out
	out="$(_linode_auth_config | _linode_request GET "/linode/instances/$id" "$canned")"
	_linode_split "$out"
	_linode_check "status" "$_R_STATUS"
	printf '%s' "$_R_BODY"
}

provider_create() {
	local name="$1" region="$2" itype="$3" image="$4" pubkey="$5"
	local out id

	if is_true "${RIFT_PROVIDER_DRY_RUN:-}"; then
		out="$(: | _linode_request POST /linode/instances "$_LINODE_CANNED_CREATE")"
		_linode_split "$out"
		printf '%s\n' "$(printf '%s' "$_R_BODY" | _linode_field id)"
		return 0
	fi

	local root_pass body esc
	root_pass="$(_linode_gen_root_pass)"
	# root_pass and token stay out of argv: root_pass reaches python via the
	# environment, and both reach curl via the stdin config below.
	body="$(RIFT_LINODE_ROOT_PASS="$root_pass" python3 -c "$_LINODE_BODY_PY" \
		"$name" "$region" "$itype" "$image" "$pubkey")"
	# Escape the JSON for a curl config quoted value ("\" then '"').
	esc="${body//\\/\\\\}"
	esc="${esc//\"/\\\"}"

	out="$(
		{
			_linode_auth_config
			printf 'header = "Content-Type: application/json"\n'
			printf 'data = "%s"\n' "$esc"
		} | _linode_request POST /linode/instances "$_LINODE_CANNED_CREATE"
	)"
	_linode_split "$out"
	_linode_check "create" "$_R_STATUS"

	id="$(printf '%s' "$_R_BODY" | _linode_field id)"
	[ -n "$id" ] || die "linode: create returned no instance id"
	printf '%s\n' "$id"
}

provider_status() {
	_linode_get "$1" "$_LINODE_CANNED_INSTANCE" | _linode_field status
}

provider_ipv4() {
	_linode_get "$1" "$_LINODE_CANNED_INSTANCE" | _linode_field ipv4
}

provider_ipv6() {
	_linode_get "$1" "$_LINODE_CANNED_INSTANCE" | _linode_field ipv6
}

provider_destroy() {
	local id="$1" out
	out="$(_linode_auth_config | _linode_request DELETE "/linode/instances/$id" "")"
	_linode_split "$out"
	# 404 means the instance is already gone. The goal of destroy is "this id no
	# longer exists", which a 404 already satisfies, so it is success -- that is
	# what makes --destroy safe to run twice.
	case "$_R_STATUS" in
	2* | 404) return 0 ;;
	401) die "linode: destroy rejected with 401 -- check RIFT_LINODE_TOKEN" ;;
	*) die "linode: destroy failed (HTTP $_R_STATUS)" ;;
	esac
}

provider_list() {
	local out
	out="$(_linode_auth_config | _linode_request GET "/linode/instances" "$_LINODE_CANNED_LIST")"
	_linode_split "$out"
	_linode_check "list" "$_R_STATUS"
	printf '%s' "$_R_BODY" | python3 -c "$_LINODE_LIST_PY"
}
