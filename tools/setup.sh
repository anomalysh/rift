#!/usr/bin/env bash
set -euo pipefail

# rift setup wizard. Asks an operator a handful of questions and writes a
# ready-to-boot, untracked .env. It never contacts the VPS, never reads an
# existing .env, and generates every secret locally with the system CSPRNG --
# a secret that is generated and shown once cannot be leaked by being typed,
# echoed to a shell history, or transmitted anywhere.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# --- DNS-01 providers: exactly the snippets that exist under deploy/caddy/dns/.
# Offering a provider with no snippet would generate a .env that Caddy cannot
# use, so this list is derived from that directory's reality.
DNS_PROVIDERS="rfc2136 acmedns powerdns cloudflare digitalocean linode"

usage() {
	cat >&2 <<EOF
Usage: tools/setup.sh [options]

Interactive wizard that writes a ready-to-boot, untracked .env for rift.
Secrets are generated locally with the system CSPRNG; nothing is transmitted
and no existing .env is read.

Options:
  --output PATH        Where to write (default: .env). Must be gitignored.
  --force              Overwrite PATH if it already exists (off by default).
  --non-interactive    Take every default and generate secrets; no prompts.
  --defaults           Alias for --non-interactive.
  -h, --help           Show this help.

Non-interactive answers may be pre-seeded from the environment so the wizard is
scriptable. Recognised SETUP_* variables (each falls back to a default):
  SETUP_ENV                 development | production
  SETUP_BASE_DOMAIN         bare FQDN, e.g. rift.example.com
  SETUP_GATEWAY_HOSTNAME    default: gateway.<base>
  SETUP_TLS_MODE            internal | http01 | dns01 | self
  SETUP_ACME_EMAIL          required for http01/dns01
  SETUP_ACME_DNS_PROVIDER   required for dns01: $DNS_PROVIDERS
  SETUP_ACME_DNS_RESOLVERS  optional; DNS resolvers for the DNS-01 challenge
  SETUP_TLS_CERT_DIR/FILE   SETUP_TLS_KEY_FILE   required for self
  SETUP_POSTGRES            bundled | external
  SETUP_POSTGRES_DSN        required when SETUP_POSTGRES=external
  SETUP_REDIS               yes | no (default no)
  SETUP_NODE_ADVERTISE_URL  required when SETUP_REDIS=yes
  SETUP_SUBDOMAIN_GENERATOR words | random (default words)
  provider creds: SETUP_DNS_SERVER SETUP_DNS_TSIG_KEY_NAME SETUP_DNS_TSIG_KEY_ALG
                  SETUP_DNS_TSIG_KEY SETUP_ACMEDNS_SERVER_URL SETUP_ACMEDNS_USERNAME
                  SETUP_ACMEDNS_PASSWORD SETUP_ACMEDNS_SUBDOMAIN SETUP_PDNS_SERVER_URL
                  SETUP_PDNS_SERVER_ID SETUP_PDNS_API_TOKEN SETUP_DNS_API_TOKEN
                  SETUP_LINODE_API_VERSION
EOF
}

OUT=".env"
FORCE=false
INTERACTIVE=true
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--output)
		shift
		[ "$#" -gt 0 ] || die "--output needs a value"
		OUT="$1"
		;;
	--output=*) OUT="${1#*=}" ;;
	--force) FORCE=true ;;
	--non-interactive | --defaults) INTERACTIVE=false ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_cmd git

TMP_ENV=""
cleanup() {
	# A partially written temp file must never survive as a stray secret.
	[ -n "$TMP_ENV" ] && [ -f "$TMP_ENV" ] && rm -f "$TMP_ENV"
	return 0
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# Small helpers
# ---------------------------------------------------------------------------
lc() { printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]'; }
say() { printf '%s\n' "$*" >&2; }

# gen_secret N -- N URL/shell-safe characters from the system CSPRNG. The
# subshell disables pipefail so `head` closing the pipe does not trip `set -e`
# via tr's SIGPIPE. Alphanumeric output is safe inside a DSN without escaping.
gen_secret() {
	local n="${1:-48}" s
	s="$(
		set +o pipefail
		LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c "$n"
	)"
	[ "${#s}" -eq "$n" ] || die "could not gather $n random characters from /dev/urandom"
	printf '%s' "$s"
}

# mask SECRET -- first 4 and last 2 characters, the rest starred. Used only for
# on-screen confirmation; the real value goes to the file and nowhere else.
mask() {
	local s="${1:-}" n=${#1}
	if [ "$n" -le 6 ]; then
		printf '******'
		return
	fi
	printf '%s%s%s' "${s:0:4}" "$(printf '%*s' "$((n - 6))" '' | tr ' ' '*')" "${s: -2}"
}

# ask ENVVAR PROMPT DEFAULT [VALIDATOR] -- result in REPLY_VALUE.
# Interactive: prompt (showing DEFAULT), validate, re-ask on failure.
# Non-interactive: value of $ENVVAR, else DEFAULT; validate once, die on failure.
REPLY_VALUE=""
ask() {
	local envname="$1" prompt="$2" default="$3" validator="${4:-}" val
	while true; do
		if [ "$INTERACTIVE" = false ]; then
			val="${!envname:-$default}"
		else
			if [ -n "$default" ]; then
				printf '%s [%s]: ' "$prompt" "$default" >&2
			else
				printf '%s: ' "$prompt" >&2
			fi
			if ! IFS= read -r val; then
				die "unexpected end of input; re-run with --non-interactive/--defaults"
			fi
			[ -z "$val" ] && val="$default"
		fi
		if [ -n "$validator" ] && ! "$validator" "$val"; then
			[ "$INTERACTIVE" = false ] && die "invalid value for $envname: '$val'"
			log_warn "invalid value: '$val' -- try again"
			continue
		fi
		REPLY_VALUE="$val"
		return 0
	done
}

# ask_secret ENVVAR PROMPT -- like ask, but never echoes input and has no
# default. For credentials the operator owns (not generated by us). Result in
# REPLY_VALUE; empty is allowed (they can fill it in the file later).
ask_secret() {
	local envname="$1" prompt="$2" val
	if [ "$INTERACTIVE" = false ]; then
		REPLY_VALUE="${!envname:-}"
		return 0
	fi
	printf '%s (hidden, blank to fill in later): ' "$prompt" >&2
	if ! IFS= read -rs val; then
		printf '\n' >&2
		die "unexpected end of input"
	fi
	printf '\n' >&2
	REPLY_VALUE="$val"
}

# --- validators (mirror server/internal/config so a written file always boots)
v_env() { case "$(lc "$1")" in development | dev | d | production | prod | p) return 0 ;; *) return 1 ;; esac; }
v_fqdn() {
	local d="$1"
	case "$d" in
	"" | *" "* | */* | *:* | .* | *.) return 1 ;; # no scheme/path/port, no leading/trailing dot
	esac
	case "$d" in *.*) return 0 ;; *) return 1 ;; esac # must contain a dot
}
v_tls_mode() { case "$(lc "$1")" in internal | http01 | dns01 | self) return 0 ;; *) return 1 ;; esac; }
v_provider() {
	local p; p="$(lc "$1")"
	case " $DNS_PROVIDERS " in *" $p "*) return 0 ;; *) return 1 ;; esac
}
v_email() { case "$1" in ?*@?*.?*) return 0 ;; *) return 1 ;; esac; }
v_pg() { case "$(lc "$1")" in bundled | external) return 0 ;; *) return 1 ;; esac; }
v_dsn() { case "$1" in postgres://*|postgresql://*) return 0 ;; *) return 1 ;; esac; }
v_yesno() { case "$(lc "$1")" in y | yes | n | no | true | false | on | off | 1 | 0) return 0 ;; *) return 1 ;; esac; }
# Absolute URL with a scheme and a host, matching config's url.Parse check.
v_url() { case "$1" in [a-z]*://?*) return 0 ;; *) return 1 ;; esac; }
v_subgen() { case "$(lc "$1")" in words | random) return 0 ;; *) return 1 ;; esac; }

# ---------------------------------------------------------------------------
# Pre-flight: refuse to clobber, and refuse to write anywhere git would track.
# ---------------------------------------------------------------------------
if [ -e "$OUT" ] && [ "$FORCE" != true ]; then
	die "refusing to overwrite existing $OUT (pass --force to replace it). This wizard never reads an existing .env; back up and remove it yourself if you mean to regenerate."
fi

# A generated .env is nothing but secrets. If git would track the target, the
# next `git add` commits those secrets. --force does NOT override this: it is a
# safety floor, not a preference.
ensure_ignored() {
	local out="$1" rc=0
	if git check-ignore -q -- "$out" 2>/dev/null; then
		rc=0
	else
		rc=$?
	fi
	case "$rc" in
	0) return 0 ;; # ignored -> safe
	1) die "refusing to write $out: it is NOT gitignored and would be committed. Secrets must never be tracked; choose a path matching .env or .env.* (see .gitignore)." ;;
	*)
		# git could not decide (path outside a work tree). Fall back to the
		# filename, refusing anything but the known-ignored .env patterns.
		local base
		base="$(basename -- "$out")"
		case "$base" in
		.env.example) die "refusing to write $out: that name is tracked by git." ;;
		.env | .env.*) log_warn "could not confirm via git that $out is ignored; the name matches the ignored .env pattern, continuing" ;;
		*) die "refusing to write $out: cannot confirm it is gitignored and the name does not match the ignored .env pattern." ;;
		esac
		;;
	esac
}
ensure_ignored "$OUT"

# ---------------------------------------------------------------------------
# Interview
# ---------------------------------------------------------------------------
say "rift setup wizard"
say "Writing $OUT. Secrets are generated locally and shown once, masked."
say ""

# 1. Environment ------------------------------------------------------------
ask SETUP_ENV "Environment (development or production) -- production enforces a >=32 char admin token and a real TLS mode" "development" v_env
case "$(lc "$REPLY_VALUE")" in
production | prod | p) ENV_VAL="production" ;;
*) ENV_VAL="development" ;;
esac
IS_PROD=false
[ "$ENV_VAL" = "production" ] && IS_PROD=true

# 2. Base domain + gateway hostname ----------------------------------------
ask SETUP_BASE_DOMAIN "Base domain (bare FQDN, wildcard tunnels live under it)" "rift.example.com" v_fqdn
BASE="$REPLY_VALUE"
ask SETUP_GATEWAY_HOSTNAME "Gateway hostname (the wss:// endpoint agents dial)" "gateway.$BASE" v_fqdn
GW="$REPLY_VALUE"
if [ "${GW%%.*}" != "gateway" ]; then
	# Caddy matches this exact host before the wildcard; if its first label is
	# not reserved, an agent could claim it as a tunnel subdomain.
	log_warn "gateway hostname's first label is '${GW%%.*}', not 'gateway'. Add that label to RIFT_SUBDOMAIN_BLOCKLIST so no tunnel can claim it."
fi

# 3. TLS mode ---------------------------------------------------------------
say ""
say "TLS mode -- how the Caddy in front of riftd gets certificates:"
say "  dns01     one wildcard via DNS-01: EVERY subdomain is covered, so an unused"
say "            name answers with a readable 404 instead of a TLS error. Needs DNS creds."
say "  http01    a cert per hostname on first contact: no DNS creds, but a name that"
say "            never served a tunnel cannot complete a TLS handshake at all."
say "  self      serve a certificate and key you supply. No ACME, no renewal."
say "  internal  Caddy's own CA. Nothing publicly trusted. Local dev and e2e."
default_mode="internal"
[ "$IS_PROD" = true ] && default_mode="http01"
ask SETUP_TLS_MODE "TLS mode" "$default_mode" v_tls_mode
TLS_MODE="$(lc "$REPLY_VALUE")"

ACME_EMAIL=""
PROVIDER=""
RESOLVERS=""
CERT_DIR=""
CERT_FILE=""
KEY_FILE=""
# provider credential holders
DNS_SERVER="" ; TSIG_KEY_NAME="" ; TSIG_KEY_ALG="" ; TSIG_KEY=""
ACMEDNS_SERVER_URL="" ; ACMEDNS_USERNAME="" ; ACMEDNS_PASSWORD="" ; ACMEDNS_SUBDOMAIN=""
PDNS_SERVER_URL="" ; PDNS_SERVER_ID="" ; PDNS_API_TOKEN=""
DNS_API_TOKEN="" ; LINODE_API_VERSION=""

case "$TLS_MODE" in
http01)
	ask SETUP_ACME_EMAIL "ACME account email (Let's Encrypt expiry notices)" "" v_email
	ACME_EMAIL="$REPLY_VALUE"
	;;
dns01)
	ask SETUP_ACME_EMAIL "ACME account email (Let's Encrypt expiry notices)" "" v_email
	ACME_EMAIL="$REPLY_VALUE"
	say "DNS-01 provider (only providers with a snippet in deploy/caddy/dns/ are offered):"
	say "  $DNS_PROVIDERS"
	ask SETUP_ACME_DNS_PROVIDER "DNS provider" "" v_provider
	PROVIDER="$(lc "$REPLY_VALUE")"
	case "$PROVIDER" in
	rfc2136)
		ask SETUP_DNS_SERVER "  rfc2136 DNS server (host:port)" "" ; DNS_SERVER="$REPLY_VALUE"
		ask SETUP_DNS_TSIG_KEY_NAME "  TSIG key name (e.g. rift-acme.)" "" ; TSIG_KEY_NAME="$REPLY_VALUE"
		ask SETUP_DNS_TSIG_KEY_ALG "  TSIG algorithm" "hmac-sha256" ; TSIG_KEY_ALG="$REPLY_VALUE"
		ask_secret SETUP_DNS_TSIG_KEY "  TSIG key (base64 secret)" ; TSIG_KEY="$REPLY_VALUE"
		;;
	acmedns)
		ask SETUP_ACMEDNS_SERVER_URL "  acme-dns server URL" "" ; ACMEDNS_SERVER_URL="$REPLY_VALUE"
		ask SETUP_ACMEDNS_USERNAME "  acme-dns username" "" ; ACMEDNS_USERNAME="$REPLY_VALUE"
		ask_secret SETUP_ACMEDNS_PASSWORD "  acme-dns password" ; ACMEDNS_PASSWORD="$REPLY_VALUE"
		ask SETUP_ACMEDNS_SUBDOMAIN "  acme-dns subdomain" "" ; ACMEDNS_SUBDOMAIN="$REPLY_VALUE"
		;;
	powerdns)
		ask SETUP_PDNS_SERVER_URL "  PowerDNS API URL" "" ; PDNS_SERVER_URL="$REPLY_VALUE"
		ask SETUP_PDNS_SERVER_ID "  PowerDNS server id" "localhost" ; PDNS_SERVER_ID="$REPLY_VALUE"
		ask_secret SETUP_PDNS_API_TOKEN "  PowerDNS API token" ; PDNS_API_TOKEN="$REPLY_VALUE"
		;;
	cloudflare | digitalocean)
		ask_secret SETUP_DNS_API_TOKEN "  $PROVIDER API token" ; DNS_API_TOKEN="$REPLY_VALUE"
		;;
	linode)
		ask_secret SETUP_DNS_API_TOKEN "  Linode API token" ; DNS_API_TOKEN="$REPLY_VALUE"
		ask SETUP_LINODE_API_VERSION "  Linode API version" "v4" ; LINODE_API_VERSION="$REPLY_VALUE"
		;;
	esac
	# Split-horizon DNS makes Caddy resolve the challenge against an internal
	# view that lacks the public TXT record; naming external resolvers works
	# around it. Optional -- empty means Caddy's default resolvers.
	ask SETUP_ACME_DNS_RESOLVERS "  DNS resolvers for the challenge (space/comma list; blank = Caddy default, override only for split-horizon DNS)" "" ; RESOLVERS="$REPLY_VALUE"
	;;
self)
	ask SETUP_TLS_CERT_DIR "  Host dir bind-mounted at /certs in the Caddy container" "./certs" ; CERT_DIR="$REPLY_VALUE"
	ask SETUP_TLS_CERT_FILE "  Certificate path inside the container" "/certs/fullchain.pem" ; CERT_FILE="$REPLY_VALUE"
	ask SETUP_TLS_KEY_FILE "  Private key path inside the container" "/certs/key.pem" ; KEY_FILE="$REPLY_VALUE"
	;;
esac

# 4. Admin token (generated, never typed) -----------------------------------
ADMIN_TOKEN="$(gen_secret 48)"

# 5. Postgres ---------------------------------------------------------------
say ""
ask SETUP_POSTGRES "Postgres: 'bundled' (compose Postgres, generated password) or 'external' (your DSN)" "bundled" v_pg
PG_MODE="$(lc "$REPLY_VALUE")"
PG_PASSWORD=""
if [ "$PG_MODE" = "bundled" ]; then
	PG_PASSWORD="$(gen_secret 32)"
	# Host 'postgres' is the compose service name; alphanumeric password needs
	# no URL-escaping. sslmode=disable is correct on the internal compose net.
	PG_DSN="postgres://rift:${PG_PASSWORD}@postgres:5432/rift?sslmode=disable"
else
	log_warn "The DSN you enter contains a password. It is a secret; it goes only into $OUT."
	ask SETUP_POSTGRES_DSN "External Postgres DSN (postgres://user:pass@host:5432/db?sslmode=require)" "" v_dsn
	PG_DSN="$REPLY_VALUE"
fi

# 6. Redis / multi-node -----------------------------------------------------
say ""
ask SETUP_REDIS "Enable Redis for multi-node routing? (yes/no)" "no" v_yesno
REDIS_ON=false
PEER_SECRET=""
ADVERTISE_URL=""
if is_true "$REPLY_VALUE"; then
	REDIS_ON=true
	PEER_SECRET="$(gen_secret 48)"
	ask SETUP_NODE_ADVERTISE_URL "  This node's advertise URL (how peers reach it, e.g. http://10.0.0.4:8080)" "" v_url
	ADVERTISE_URL="$REPLY_VALUE"
fi

# 7. Subdomain generator ----------------------------------------------------
say ""
ask SETUP_SUBDOMAIN_GENERATOR "Generated-subdomain style: 'words' (adjective-noun-number) or 'random'" "words" v_subgen
SUBGEN="$(lc "$REPLY_VALUE")"

# ---------------------------------------------------------------------------
# Write the file atomically: build a 0600 temp file in the target directory,
# then mv it into place. A reader never sees a half-written file, and the
# secrets never touch a mode that another user could read, even for an instant.
# ---------------------------------------------------------------------------
OUT_DIR="$(cd "$(dirname -- "$OUT")" && pwd)"
OUT_ABS="$OUT_DIR/$(basename -- "$OUT")"
TMP_ENV="$(mktemp "$OUT_DIR/.env.wizard.XXXXXX")"
chmod 600 "$TMP_ENV"

w() { printf '%s\n' "$*" >>"$TMP_ENV"; }
wq() { printf '%s="%s"\n' "$1" "$2" >>"$TMP_ENV"; } # quoted: value has URL/DSN specials

secrets_list="admin token"
[ "$PG_MODE" = "bundled" ] && secrets_list="$secrets_list, database password"
[ "$REDIS_ON" = true ] && secrets_list="$secrets_list, peer secret"
w "# rift environment -- GENERATED by tools/setup.sh on $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
w "#"
w "# THIS FILE CONTAINS LIVE SECRETS ($secrets_list)."
w "# It is gitignored. Keep it private and NEVER commit it. To regenerate, delete"
w "# it and re-run tools/setup.sh (secrets are one-time and are not recoverable)."
w ""
w "# --- Core -------------------------------------------------------------------"
w "RIFT_ENV=$ENV_VAL"
w "RIFT_BASE_DOMAIN=$BASE"
w "RIFT_GATEWAY_HOSTNAME=$GW"
w ""
w "# --- Admin API (SECRET) -----------------------------------------------------"
w "RIFT_ADMIN_TOKEN=$ADMIN_TOKEN"
w ""
w "# --- TLS --------------------------------------------------------------------"
w "RIFT_TLS_MODE=$TLS_MODE"
case "$TLS_MODE" in
http01)
	wq RIFT_ACME_EMAIL "$ACME_EMAIL"
	;;
dns01)
	wq RIFT_ACME_EMAIL "$ACME_EMAIL"
	w "RIFT_ACME_DNS_PROVIDER=$PROVIDER"
	case "$PROVIDER" in
	rfc2136)
		wq RIFT_DNS_SERVER "$DNS_SERVER"
		wq RIFT_DNS_TSIG_KEY_NAME "$TSIG_KEY_NAME"
		wq RIFT_DNS_TSIG_KEY_ALG "$TSIG_KEY_ALG"
		wq RIFT_DNS_TSIG_KEY "$TSIG_KEY"
		;;
	acmedns)
		wq RIFT_ACMEDNS_SERVER_URL "$ACMEDNS_SERVER_URL"
		wq RIFT_ACMEDNS_USERNAME "$ACMEDNS_USERNAME"
		wq RIFT_ACMEDNS_PASSWORD "$ACMEDNS_PASSWORD"
		wq RIFT_ACMEDNS_SUBDOMAIN "$ACMEDNS_SUBDOMAIN"
		;;
	powerdns)
		wq RIFT_PDNS_SERVER_URL "$PDNS_SERVER_URL"
		wq RIFT_PDNS_SERVER_ID "$PDNS_SERVER_ID"
		wq RIFT_PDNS_API_TOKEN "$PDNS_API_TOKEN"
		;;
	cloudflare | digitalocean)
		wq RIFT_DNS_API_TOKEN "$DNS_API_TOKEN"
		;;
	linode)
		wq RIFT_DNS_API_TOKEN "$DNS_API_TOKEN"
		wq RIFT_LINODE_API_VERSION "$LINODE_API_VERSION"
		;;
	esac
	# RIFT_ACME_DNS_RESOLVERS is optional; only emit an override. Quoted
	# because it is a space/comma list and an unquoted space would split it.
	[ -n "$RESOLVERS" ] && wq RIFT_ACME_DNS_RESOLVERS "$RESOLVERS"
	;;
self)
	wq RIFT_TLS_CERT_DIR "$CERT_DIR"
	wq RIFT_TLS_CERT_FILE "$CERT_FILE"
	wq RIFT_TLS_KEY_FILE "$KEY_FILE"
	;;
esac
w ""
w "# --- Postgres (DSN password is a SECRET) ------------------------------------"
wq RIFT_POSTGRES_DSN "$PG_DSN"
if [ "$PG_MODE" = "bundled" ]; then
	w "# Consumed by the compose Postgres container (deploy/docker-compose.yml)."
	w "POSTGRES_USER=rift"
	w "POSTGRES_PASSWORD=$PG_PASSWORD"
	w "POSTGRES_DB=rift"
fi
if [ "$REDIS_ON" = true ]; then
	w ""
	w "# --- Redis / multi-node (PEER_SECRET is a SECRET) ---------------------------"
	w "RIFT_REDIS_ENABLED=true"
	w "RIFT_REDIS_ADDR=redis:6379"
	w "RIFT_PEER_SECRET=$PEER_SECRET"
	wq RIFT_NODE_ADVERTISE_URL "$ADVERTISE_URL"
fi
w ""
w "# --- Subdomain generation ---------------------------------------------------"
w "RIFT_SUBDOMAIN_GENERATOR=$SUBGEN"

mv -f "$TMP_ENV" "$OUT_ABS"
TMP_ENV="" # written; nothing for the trap to clean up

# ---------------------------------------------------------------------------
# Confirmation + next steps (all masked; the file is the only copy of a secret)
# ---------------------------------------------------------------------------
say ""
log_info "Wrote $OUT_ABS (mode 600)."
log_info "Admin token: $(mask "$ADMIN_TOKEN")  <- the only copy is in $OUT; it is never shown again."
[ "$PG_MODE" = "bundled" ] && log_info "Postgres password: $(mask "$PG_PASSWORD")"
[ "$REDIS_ON" = true ] && log_info "Peer secret: $(mask "$PEER_SECRET")"
say ""
say "Next steps:"
if [ "$IS_PROD" = true ]; then
	if [ "$TLS_MODE" = "dns01" ]; then
		say "  1. Build a Caddy image with the '$PROVIDER' DNS plugin:  make build-caddy"
		say "       (runs tools/build-caddy.sh; set RIFT_CADDY_IMAGE to the tag it prints)"
		say "  2. Fill in the '$PROVIDER' credentials in $OUT if you left any blank."
		say "  3. Deploy to the VPS:                                   make deploy"
		say "       (runs tools/remote-deploy.sh)"
	else
		say "  1. Deploy to the VPS:  make deploy   (stock Caddy handles $TLS_MODE)"
		say "       (runs tools/remote-deploy.sh)"
	fi
	say "  - Mint an admin token for a user later with:  make mint-token NAME=you"
else
	if [ "$REDIS_ON" = true ]; then
		say "  1. Start the local stack:  make up   (Redis backend: docker compose --profile redis up)"
	else
		say "  1. Start the local stack:  make up"
	fi
	say "  2. Mint an admin token:    make mint-token NAME=you"
	say "  3. Reach a tunnel by sending the Host header, e.g.:"
	say "       curl -H 'Host: sub.$BASE' http://127.0.0.1:8080/"
fi
say ""
say "  - $OUT holds live secrets and is gitignored. Keep it private; never commit it."
say "  - The secrets above were shown once, masked. $OUT is the only copy."
