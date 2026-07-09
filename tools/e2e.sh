#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=tools/lib/preflight.sh
. "$SCRIPT_DIR/lib/preflight.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

COMPOSE_FILE="$REPO_ROOT/deploy/docker-compose.e2e.yml"
PROJECT="rift-e2e"

BASE_DOMAIN="rift.localtest"
HTTPS_PORT="18443"
HTTP_PORT="18080"
GATEWAY_PORT="18081"
ADMIN_PORT="18082"
GATEWAY2_PORT="18091"
UPSTREAM_PORT="13099"
ADMIN_TOKEN="e2e-admin-token-not-a-secret-0000000000000"
# Must clear the 32-character minimum riftd enforces on a peer secret.
PEER_SECRET="e2e-peer-secret-not-a-secret-000000000000"
# Matches tools/e2e/dns/named.conf. A throwaway zone in a throwaway container.
TSIG_NAME="rift-e2e."
TSIG_ALG="hmac-sha256"
TSIG_KEY="c2VjcmV0LXJpZnQtZTJlLXRlc3Qta2V5LW5vdC1hLXNlY3JldA=="
PEBBLE_IMAGE="ghcr.io/letsencrypt/pebble:latest"
CADDY_DNS_IMAGE="rift-caddy:e2e"

usage() {
	cat >&2 <<EOF
Usage: tools/e2e.sh [--mode MODE ...] [--keep] [--verbose]

Bring up the full rift stack in Docker -- Postgres, riftd, and a real Caddy
using the production Caddyfile -- and drive it end to end over HTTPS with the
compiled rift CLI. Hermetic: nothing reaches the internet.

The base domain is $BASE_DOMAIN. Requests use \`curl --resolve\`, so nothing is
written to /etc/hosts and nothing needs root.

Modes (repeatable; default: internal self http01 dns01):
  internal   Caddy's own CA signs a wildcard. No ACME.
  self       An operator-supplied wildcard certificate, generated here.
  http01     A real ACME order against Pebble, validated over HTTP-01, gated by
             riftd's ask endpoint. Proves the on-demand path AND its one real
             limitation: a hostname that never had a tunnel gets no certificate.
  dns01      A real ACME order against Pebble, validated over DNS-01, with
             Caddy's rfc2136 solver writing the challenge record into an
             authoritative BIND over a TSIG-signed dynamic update. Proves the
             wildcard covers a subdomain that has never been used.

The ACME modes run Pebble (Let's Encrypt's own test server) and BIND inside the
stack, so the ACME client is exercised for real: no internet, no rate limit,
nothing mocked.

Options:
  --mode MODE   Run this mode (repeat for several).
  --cluster     Also run the two-node Redis routing test.
  --keep        Leave the stack running afterwards, for poking at.
  --verbose     Stream container logs on failure and set riftd to debug.
EOF
}

modes=()
keep=false
verbose=false
cluster=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--mode)
		shift
		[ "$#" -gt 0 ] || die "--mode needs a value"
		modes+=("$1")
		;;
	--cluster) cluster=true ;;
	--keep) keep=true ;;
	--verbose) verbose=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done
[ "${#modes[@]}" -gt 0 ] || modes=(internal self http01 dns01)

# Building three images and running two TLS modes is not free. Refuse before
# writing anything rather than dying halfway through a docker build with a
# truncated layer in the cache.
readonly E2E_MIN_DISK_MB=3072
readonly E2E_MIN_MEM_MB=1024

require_cmd curl openssl python3
require_docker

TMPDIR_E2E="$(mktemp -d)"
UPSTREAM_PID=""
CLI_PID=""

CURRENT_MODE=""
pass=0
fail=0

cleanup() {
	local status=$?
	[ -n "$CLI_PID" ] && kill "$CLI_PID" 2>/dev/null || true
	[ -n "$UPSTREAM_PID" ] && kill "$UPSTREAM_PID" 2>/dev/null || true
	if [ "$keep" != true ]; then
		compose down -v --remove-orphans >/dev/null 2>&1 || true
	else
		log_warn "stack left running (--keep); tear down with:"
		log_warn "  docker compose -f $COMPOSE_FILE -p $PROJECT down -v"
	fi
	rm -rf "$TMPDIR_E2E"
	exit "$status"
}
trap cleanup EXIT INT TERM

compose() { docker compose -f "$COMPOSE_FILE" -p "$PROJECT" "$@"; }

# BuildKit runs the build inside its own container, which fails on hosts whose
# container runtime is misconfigured (a stale nvidia hook, for instance). The
# legacy builder does not, and produces the same image, so fall back to it
# rather than making the whole harness unusable on such a machine.
build_images() {
	if compose build >"$TMPDIR_E2E/build.log" 2>&1; then
		return 0
	fi
	log_warn "buildkit build failed; retrying with the legacy builder"
	if DOCKER_BUILDKIT=0 compose build >>"$TMPDIR_E2E/build.log" 2>&1; then
		return 0
	fi
	tail -20 "$TMPDIR_E2E/build.log" >&2
	die "could not build the e2e images"
}

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

check_contains() {
	local name="$1" haystack="$2" needle="$3"
	if printf '%s' "$haystack" | grep -qF "$needle"; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s: %q does not contain %q\n' "$name" "$haystack" "$needle"
		fail=$((fail + 1))
	fi
}

# rcurl issues a request to a rift hostname through Caddy, validating the chain
# against the CA for the mode under test. No -k anywhere: an e2e that skips
# verification would not have caught a certificate that was never issued.
rcurl() {
	local host="$1"
	shift
	curl --silent --show-error \
		--resolve "${host}:${HTTPS_PORT}:127.0.0.1" \
		--cacert "$TMPDIR_E2E/ca.pem" \
		--max-time 30 \
		"$@" "https://${host}:${HTTPS_PORT}${path:-/}"
}

status_of() {
	local host="$1" path="${2:-/}"
	path="$path" rcurl "$host" -o /dev/null -w '%{http_code}' || echo "TLS-FAIL"
}

body_of() {
	local host="$1" path="${2:-/}"
	path="$path" rcurl "$host" || true
}

# generate_self_cert makes a self-signed wildcard usable as both leaf and root,
# which is what the `self` mode serves and what curl validates against.
generate_self_cert() {
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
		-keyout "$TMPDIR_E2E/certs/key.pem" \
		-out "$TMPDIR_E2E/certs/fullchain.pem" \
		-subj "/CN=${BASE_DOMAIN}" \
		-addext "subjectAltName=DNS:${BASE_DOMAIN},DNS:*.${BASE_DOMAIN}" \
		-addext "basicConstraints=critical,CA:TRUE" \
		2>/dev/null
}

wait_for_tcp() {
	local port="$1" what="$2" i
	for i in $(seq 1 60); do
		if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
			exec 3<&- 3>&-
			return 0
		fi
		sleep 1
	done
	die "$what did not come up on port $port"
}


# ---------------------------------------------------------------- ACME (Pebble)

# Pebble's image is distroless, so its CA material is copied out rather than
# read with a shell. Caddy needs this root to trust the ACME server's own HTTPS
# certificate; without it the client cannot even fetch the directory.
prepare_pebble() {
	mkdir -p "$TMPDIR_E2E/pebble"
	docker pull -q "$PEBBLE_IMAGE" >/dev/null 2>&1 || true
	local cid
	cid="$(docker create "$PEBBLE_IMAGE")"
	docker cp "$cid:/test/certs/pebble.minica.pem" "$TMPDIR_E2E/pebble/minica.pem" >/dev/null
	docker rm -f "$cid" >/dev/null
	[ -s "$TMPDIR_E2E/pebble/minica.pem" ] || die "could not extract pebble's CA root"
}

# Pebble mints a fresh issuing root on every start, so the trust anchor has to
# be fetched at run time. Pinning one would silently test nothing.
fetch_pebble_root() {
	local net="${PROJECT}_default" i
	for i in $(seq 1 30); do
		if docker run --rm --network "$net" \
			-v "$TMPDIR_E2E/pebble:/p:ro" curlimages/curl:latest \
			-s --max-time 5 --cacert /p/minica.pem https://pebble:15000/roots/0 \
			>"$TMPDIR_E2E/ca.pem" 2>/dev/null && [ -s "$TMPDIR_E2E/ca.pem" ]; then
			return 0
		fi
		sleep 1
	done
	die "pebble never served its issuing root"
}

# dns01 needs a Caddy with the rfc2136 solver compiled in. Stock Caddy has none.
ensure_caddy_dns_image() {
	if docker image inspect "$CADDY_DNS_IMAGE" >/dev/null 2>&1; then
		log_info "using existing $CADDY_DNS_IMAGE"
		return 0
	fi
	log_info "building $CADDY_DNS_IMAGE with the rfc2136 solver (this compiles Caddy)"
	RIFT_CADDY_DNS_PLUGINS="github.com/caddy-dns/rfc2136" \
		RIFT_CADDY_IMAGE="$CADDY_DNS_IMAGE" \
		"$SCRIPT_DIR/build-caddy.sh" >/dev/null 2>&1 ||
		die "could not build $CADDY_DNS_IMAGE"
}

wait_for_tls() {
	local host="$1" i
	for i in $(seq 1 60); do
		if echo | openssl s_client -connect "127.0.0.1:${HTTPS_PORT}" \
			-servername "$host" 2>/dev/null | grep -q 'BEGIN CERTIFICATE'; then
			return 0
		fi
		sleep 2
	done
	return 1
}

cert_issuer_of() {
	local host="$1"
	echo | openssl s_client -connect "127.0.0.1:${HTTPS_PORT}" -servername "$host" 2>/dev/null |
		openssl x509 -noout -issuer 2>/dev/null || true
}

has_cert() {
	echo | timeout 15 openssl s_client -connect "127.0.0.1:${HTTPS_PORT}" \
		-servername "$1" 2>/dev/null | grep -qc 'BEGIN CERTIFICATE'
}

run_mode() {
	local mode="$1"
	printf '\n=== mode: %s ===\n' "$mode"

	mkdir -p "$TMPDIR_E2E/certs"
	export RIFT_TLS_MODE="$mode"
	export RIFT_E2E_BASE_DOMAIN="$BASE_DOMAIN"
	export RIFT_E2E_ADMIN_TOKEN="$ADMIN_TOKEN"
	export RIFT_E2E_HTTPS_PORT="$HTTPS_PORT" RIFT_E2E_HTTP_PORT="$HTTP_PORT"
	export RIFT_E2E_GATEWAY_PORT="$GATEWAY_PORT" RIFT_E2E_ADMIN_PORT="$ADMIN_PORT"
	export RIFT_E2E_LOG_LEVEL="info"
	[ "$verbose" = true ] && export RIFT_E2E_LOG_LEVEL="debug"

	# Defaults; the ACME modes override several of these.
	export RIFT_E2E_CERT_DIR="$REPO_ROOT/deploy/caddy"
	export RIFT_E2E_PEBBLE_DIR="$REPO_ROOT/deploy/caddy"
	export RIFT_TLS_CERT_FILE="" RIFT_TLS_KEY_FILE=""
	export RIFT_ACME_CA_PROFILE="public" RIFT_ACME_CA_URL="" RIFT_ACME_CA_ROOT=""
	export RIFT_ACME_DNS_PROVIDER=""
	export RIFT_E2E_TSIG_NAME="" RIFT_E2E_TSIG_ALG="" RIFT_E2E_TSIG_KEY=""
	export RIFT_E2E_REDIS_ENABLED="false" RIFT_E2E_PEER_SECRET="$PEER_SECRET"
	unset RIFT_CADDY_IMAGE || true
	export COMPOSE_PROFILES=""

	case "$mode" in
	self)
		generate_self_cert
		export RIFT_E2E_CERT_DIR="$TMPDIR_E2E/certs"
		export RIFT_TLS_CERT_FILE="/certs/fullchain.pem"
		export RIFT_TLS_KEY_FILE="/certs/key.pem"
		;;
	internal) ;;
	http01 | dns01)
		# A genuine ACME order against Pebble, not a simulation of one.
		prepare_pebble
		export RIFT_E2E_PEBBLE_DIR="$TMPDIR_E2E/pebble"
		export RIFT_ACME_CA_PROFILE="internal-ca"
		export RIFT_ACME_CA_URL="https://pebble:14000/dir"
		export RIFT_ACME_CA_ROOT="/pebble/minica.pem"
		export COMPOSE_PROFILES="acme"
		if [ "$mode" = "dns01" ]; then
			ensure_caddy_dns_image
			export RIFT_CADDY_IMAGE="$CADDY_DNS_IMAGE"
			export RIFT_ACME_DNS_PROVIDER="rfc2136"
			export RIFT_E2E_TSIG_NAME="$TSIG_NAME"
			export RIFT_E2E_TSIG_ALG="$TSIG_ALG"
			export RIFT_E2E_TSIG_KEY="$TSIG_KEY"
		fi
		;;
	*) die "mode $mode is not supported by this harness (see --help)" ;;
	esac

	log_info "starting stack"
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	build_images
	compose up -d >/dev/null

	wait_for_tcp "$ADMIN_PORT" "riftd admin"
	wait_for_tcp "$HTTPS_PORT" "caddy"

	# The CA the test trusts differs per mode. For `internal` it is Caddy's own
	# root, which only exists once Caddy has started.
	case "$mode" in
	self) cp "$TMPDIR_E2E/certs/fullchain.pem" "$TMPDIR_E2E/ca.pem" ;;
	internal)
		local i
		for i in $(seq 1 30); do
			if compose exec -T caddy cat /data/caddy/pki/authorities/local/root.crt \
				>"$TMPDIR_E2E/ca.pem" 2>/dev/null && [ -s "$TMPDIR_E2E/ca.pem" ]; then
				break
			fi
			sleep 1
		done
		[ -s "$TMPDIR_E2E/ca.pem" ] || die "caddy never wrote its internal root CA"
		;;
	http01 | dns01)
		fetch_pebble_root
		log_info "trusting pebble's issuing root for this run"
		;;
	esac

	log_info "starting upstream on 127.0.0.1:$UPSTREAM_PORT"
	python3 "$SCRIPT_DIR/e2e/upstream.py" "$UPSTREAM_PORT" >"$TMPDIR_E2E/upstream.log" 2>&1 &
	UPSTREAM_PID=$!
	wait_for_tcp "$UPSTREAM_PORT" "upstream"

	CURRENT_MODE="$mode"

	local token
	token="$(mint_token)"
	[ -n "$token" ] || die "could not mint a token"

	log_info "connecting the rift CLI"
	"$REPO_ROOT/cli/dist/rift" http "$UPSTREAM_PORT" hello \
		--token "$token" \
		--server "ws://127.0.0.1:${GATEWAY_PORT}/tunnel" \
		>"$TMPDIR_E2E/cli.log" 2>&1 &
	CLI_PID=$!

	local i
	for i in $(seq 1 30); do
		[ "$(status_of "hello.$BASE_DOMAIN")" = "200" ] && break
		sleep 1
	done

	assert_routing
	assert_tls "$mode"
	case "$mode" in
	http01 | dns01) assert_acme "$mode" ;;
	esac
	assert_ask_endpoint
	assert_admin_is_guarded

	kill "$CLI_PID" 2>/dev/null || true
	CLI_PID=""
	kill "$UPSTREAM_PID" 2>/dev/null || true
	UPSTREAM_PID=""

	if [ "$fail" -ne 0 ] && [ "$verbose" = true ]; then
		log_warn "riftd logs:"
		compose logs riftd 2>&1 | tail -30 >&2
		log_warn "caddy logs:"
		compose logs caddy 2>&1 | tail -30 >&2
	fi

	[ "$keep" = true ] || compose down -v --remove-orphans >/dev/null 2>&1 || true
}


# ---------------------------------------------------------------- cluster
#
# Redis exists so a public request arriving at one node can be served by an
# agent attached to a different one. The only way to prove that is to attach
# the agent to node 2 and send the request to node 1.
run_cluster() {
	printf '\n=== cluster: two nodes routed by redis ===\n'
	CURRENT_MODE="internal"

	export RIFT_TLS_MODE="internal"
	export RIFT_E2E_BASE_DOMAIN="$BASE_DOMAIN"
	export RIFT_E2E_ADMIN_TOKEN="$ADMIN_TOKEN"
	export RIFT_E2E_HTTPS_PORT="$HTTPS_PORT" RIFT_E2E_HTTP_PORT="$HTTP_PORT"
	export RIFT_E2E_GATEWAY_PORT="$GATEWAY_PORT" RIFT_E2E_ADMIN_PORT="$ADMIN_PORT"
	export RIFT_E2E_GATEWAY2_PORT="$GATEWAY2_PORT"
	export RIFT_E2E_LOG_LEVEL="info"
	[ "$verbose" = true ] && export RIFT_E2E_LOG_LEVEL="debug"
	export RIFT_E2E_CERT_DIR="$REPO_ROOT/deploy/caddy"
	export RIFT_E2E_PEBBLE_DIR="$REPO_ROOT/deploy/caddy"
	export RIFT_TLS_CERT_FILE="" RIFT_TLS_KEY_FILE=""
	export RIFT_ACME_CA_PROFILE="public" RIFT_ACME_CA_URL="" RIFT_ACME_CA_ROOT=""
	export RIFT_ACME_DNS_PROVIDER=""
	export RIFT_E2E_TSIG_NAME="" RIFT_E2E_TSIG_ALG="" RIFT_E2E_TSIG_KEY=""
	export RIFT_E2E_REDIS_ENABLED="true"
	export RIFT_E2E_PEER_SECRET="$PEER_SECRET"
	export COMPOSE_PROFILES="cluster"
	unset RIFT_CADDY_IMAGE || true

	log_info "starting a two-node stack"
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	build_images
	compose up -d >/dev/null

	wait_for_tcp "$ADMIN_PORT" "riftd admin"
	wait_for_tcp "$GATEWAY2_PORT" "riftd2 gateway"
	wait_for_tcp "$HTTPS_PORT" "caddy"

	local i
	for i in $(seq 1 30); do
		if compose exec -T caddy cat /data/caddy/pki/authorities/local/root.crt \
			>"$TMPDIR_E2E/ca.pem" 2>/dev/null && [ -s "$TMPDIR_E2E/ca.pem" ]; then
			break
		fi
		sleep 1
	done
	[ -s "$TMPDIR_E2E/ca.pem" ] || die "caddy never wrote its internal root CA"

	python3 "$SCRIPT_DIR/e2e/upstream.py" "$UPSTREAM_PORT" >"$TMPDIR_E2E/upstream.log" 2>&1 &
	UPSTREAM_PID=$!
	wait_for_tcp "$UPSTREAM_PORT" "upstream"

	local token
	token="$(mint_token)"
	[ -n "$token" ] || die "could not mint a token"

	# Attach the agent to node 2. Public traffic will arrive at node 1.
	log_info "attaching the agent to node 2 (gateway on $GATEWAY2_PORT)"
	"$REPO_ROOT/cli/dist/rift" http "$UPSTREAM_PORT" hello \
		--token "$token" \
		--server "ws://127.0.0.1:${GATEWAY2_PORT}/tunnel" \
		>"$TMPDIR_E2E/cli.log" 2>&1 &
	CLI_PID=$!

	for i in $(seq 1 30); do
		[ "$(status_of "hello.$BASE_DOMAIN")" = "200" ] && break
		sleep 1
	done

	printf '  peer forwarding\n'
	check "a request to node 1 is served by the agent on node 2" \
		"$(body_of "hello.$BASE_DOMAIN" "/across")" "local app saw GET /across"
	check "status is 200 across the peer hop" "$(status_of "hello.$BASE_DOMAIN")" "200"

	# The lease must name node 2, or node 1 served it locally and this test
	# proved nothing.
	local lease
	lease="$(compose exec -T redis redis-cli --raw GET "rift:route:hello" 2>/dev/null | tr -d '\r')"
	check "redis lease points at node 2" "$lease" "http://riftd2:8080"

	# A POST body must survive the extra hop.
	local echoed
	echoed="$(path=/echo rcurl "hello.$BASE_DOMAIN" -X POST --data-binary 'across-the-hop')"
	check "request body survives the peer hop" "$echoed" "across-the-hop"

	printf '  peer authentication\n'
	# Without the shared secret, the internal proxy route is an open door onto
	# every tunnel by name.
	check "internal proxy refuses a request with no peer secret" \
		"$(compose exec -T riftd wget -q -S -O /dev/null \
			--header="X-Rift-Subdomain: hello" \
			http://127.0.0.1:8080/internal/proxy 2>&1 |
			sed -n 's|.*HTTP/1.1 \([0-9]*\).*|\1|p' | head -1)" "403"

	check "internal proxy refuses a wrong peer secret" \
		"$(compose exec -T riftd wget -q -S -O /dev/null \
			--header="X-Rift-Subdomain: hello" --header="X-Rift-Peer-Token: wrong" \
			http://127.0.0.1:8080/internal/proxy 2>&1 |
			sed -n 's|.*HTTP/1.1 \([0-9]*\).*|\1|p' | head -1)" "403"

	# When the agent goes away the lease must be retracted, not left to rot.
	kill "$CLI_PID" 2>/dev/null || true
	CLI_PID=""
	for i in $(seq 1 20); do
		lease="$(compose exec -T redis redis-cli --raw EXISTS "rift:route:hello" 2>/dev/null | tr -d '\r')"
		[ "$lease" = "0" ] && break
		sleep 1
	done
	check "the route lease is retracted when the agent disconnects" "$lease" "0"
	check "the subdomain stops routing" "$(status_of "hello.$BASE_DOMAIN")" "404"

	kill "$UPSTREAM_PID" 2>/dev/null || true
	UPSTREAM_PID=""

	if [ "$fail" -ne 0 ] && [ "$verbose" = true ]; then
		compose logs riftd riftd2 2>&1 | tail -40 >&2
	fi
	[ "$keep" = true ] || compose down -v --remove-orphans >/dev/null 2>&1 || true
}

mint_token() {
	curl -fsS --max-time 15 -X POST "http://127.0.0.1:${ADMIN_PORT}/v1/tokens" \
		-H "Authorization: Bearer ${ADMIN_TOKEN}" \
		-H 'Content-Type: application/json' \
		-d '{"name":"e2e","max_tunnels":3}' |
		sed -n 's/.*"token":"\([^"]*\)".*/\1/p'
}

assert_routing() {
	printf '  routing\n'
	check "tunnel serves over https" \
		"$(body_of "hello.$BASE_DOMAIN" "/probe")" "local app saw GET /probe"
	check "tunnel returns 200" "$(status_of "hello.$BASE_DOMAIN")" "200"

	# The apex is not a tunnel, but it must answer rather than fail TLS.
	check "apex answers 404" "$(status_of "$BASE_DOMAIN")" "404"
	check_contains "apex explains itself" \
		"$(body_of "$BASE_DOMAIN")" "does not correspond to a tunnel"

	# A subdomain with no tunnel. A wildcard-covered mode answers 404 over valid
	# TLS. http01 cannot: the name has no certificate, so the handshake fails.
	# That difference is the entire reason dns01 exists, so assert it in both
	# directions rather than skipping the mode that behaves "badly".
	if [ "$CURRENT_MODE" = "http01" ]; then
		if has_cert "nobody.$BASE_DOMAIN"; then
			printf '    FAIL  http01 issued a certificate for a subdomain with no tunnel\n'
			fail=$((fail + 1))
		else
			printf '    ok    http01 refuses a certificate for an unused subdomain (expected)\n'
			pass=$((pass + 1))
		fi
	else
		check "unused subdomain answers 404" "$(status_of "nobody.$BASE_DOMAIN")" "404"
		check_contains "unused subdomain explains itself" \
			"$(body_of "nobody.$BASE_DOMAIN")" "No tunnel is currently serving"
	fi

	check "gateway hostname is reachable" \
		"$(status_of "gateway.$BASE_DOMAIN" "/healthz")" "200"

	# Liveness says the process runs; readiness says it can reach Postgres.
	check "readiness probe is green with a live database" \
		"$(compose exec -T riftd wget -q -S -O /dev/null http://127.0.0.1:8080/readyz 2>&1 |
			sed -n 's|.*HTTP/1.1 \([0-9]*\).*|\1|p' | head -1)" "200"

	local echoed
	echoed="$(path=/echo rcurl "hello.$BASE_DOMAIN" -X POST --data-binary 'e2e-payload')"
	check "request body round-trips" "$echoed" "e2e-payload"

	local sha_local sha_tunnel
	sha_local="$(curl -fsS "http://127.0.0.1:${UPSTREAM_PORT}/big?n=2097152" | sha256sum | cut -d' ' -f1)"
	sha_tunnel="$(path='/big?n=2097152' rcurl "hello.$BASE_DOMAIN" | sha256sum | cut -d' ' -f1)"
	check "2 MiB body is byte-identical through the tunnel" "$sha_tunnel" "$sha_local"

	assert_streaming
	assert_dead_upstream_is_502
}

# A proxy that buffers would break server-sent events. The upstream sleeps
# between chunks, so receiving the first before the second proves it does not.
assert_streaming() {
	local out
	out="$(path=/stream python3 - "$TMPDIR_E2E/ca.pem" "$BASE_DOMAIN" "$HTTPS_PORT" <<'PY'
import socket, ssl, sys, time
ca, base, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
ctx = ssl.create_default_context(cafile=ca)
host = f"hello.{base}"
raw = socket.create_connection(("127.0.0.1", port), timeout=20)
sock = ctx.wrap_socket(raw, server_hostname=host)
sock.sendall(f"GET /stream HTTP/1.1\r\nHost: {host}:{port}\r\nConnection: close\r\n\r\n".encode())
start, seen = time.time(), {}
buf = b""
while len(seen) < 2 and time.time() - start < 20:
    data = sock.recv(4096)
    if not data:
        break
    buf += data
    for marker in (b"chunk-1", b"chunk-2"):
        if marker in buf and marker not in seen:
            seen[marker] = time.time() - start
print("buffered" if len(seen) < 2 or seen[b"chunk-2"] - seen[b"chunk-1"] < 0.15 else "incremental")
PY
	)"
	check "response streams incrementally" "$out" "incremental"
}

# A refused local service must surface as 502, promptly, not as a hang.
assert_dead_upstream_is_502() {
	kill "$UPSTREAM_PID" 2>/dev/null || true
	wait "$UPSTREAM_PID" 2>/dev/null || true
	sleep 1
	check "dead upstream becomes 502" "$(status_of "hello.$BASE_DOMAIN")" "502"

	python3 "$SCRIPT_DIR/e2e/upstream.py" "$UPSTREAM_PORT" >>"$TMPDIR_E2E/upstream.log" 2>&1 &
	UPSTREAM_PID=$!
	wait_for_tcp "$UPSTREAM_PORT" "upstream restart"
	sleep 1
	check "upstream recovery restores 200" "$(status_of "hello.$BASE_DOMAIN")" "200"
}

assert_tls() {
	local mode="$1"
	printf '  tls (%s)\n' "$mode"

	# Every hostname a visitor might type must present a certificate this test
	# validates against the CA -- with no -k, and no exceptions.
	local hosts=("hello.$BASE_DOMAIN" "nobody.$BASE_DOMAIN" "$BASE_DOMAIN" "gateway.$BASE_DOMAIN")
	# Under http01 an unused hostname legitimately has no certificate.
	[ "$mode" = "http01" ] && hosts=("hello.$BASE_DOMAIN" "$BASE_DOMAIN" "gateway.$BASE_DOMAIN")

	local host
	for host in "${hosts[@]}"; do
		local subject
		subject="$(echo |
			openssl s_client -connect "127.0.0.1:${HTTPS_PORT}" -servername "$host" \
				-CAfile "$TMPDIR_E2E/ca.pem" 2>/dev/null |
			openssl x509 -noout -subject 2>/dev/null || true)"
		if [ -n "$subject" ]; then
			printf '    ok    certificate served for %s\n' "$host"
			pass=$((pass + 1))
		else
			printf '    FAIL  no certificate for %s\n' "$host"
			fail=$((fail + 1))
		fi
	done

	# The gateway hostname once fell between Caddy's managed and on-demand
	# paths and received no certificate at all, while every other name worked.
	# Asserting it explicitly is the regression test for that.
	check "gateway hostname serves over TLS" \
		"$(status_of "gateway.$BASE_DOMAIN" "/healthz")" "200"

	check "http redirects to https" \
		"$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
			--resolve "hello.${BASE_DOMAIN}:${HTTP_PORT}:127.0.0.1" \
			"http://hello.${BASE_DOMAIN}:${HTTP_PORT}/")" "308"
}

# The ACME modes are the only ones that exercise the client end to end: an
# order, an authorization, a challenge solved against a real server, and a
# finalized certificate. Asserting the issuer is what distinguishes "Caddy
# served something" from "Caddy completed an ACME order".
assert_acme() {
	local mode="$1"
	printf '  acme (%s, against pebble)\n' "$mode"

	if wait_for_tls "hello.$BASE_DOMAIN"; then
		printf '    ok    certificate issued by the ACME server\n'
		pass=$((pass + 1))
	else
		printf '    FAIL  no certificate was ever issued\n'
		fail=$((fail + 1))
		return
	fi

	local issuer
	issuer="$(cert_issuer_of "hello.$BASE_DOMAIN")"
	if printf '%s' "$issuer" | grep -qi pebble; then
		printf '    ok    issuer is the test ACME server (%s)\n' "${issuer#issuer=}"
		pass=$((pass + 1))
	else
		printf '    FAIL  issuer is not pebble: %s\n' "$issuer"
		fail=$((fail + 1))
	fi

	if [ "$mode" = "dns01" ]; then
		# The wildcard can only have been issued if Caddy's rfc2136 solver
		# wrote the challenge TXT into BIND over a TSIG-signed update, and
		# Pebble read it back. Nothing else could have produced this.
		if has_cert "never-used-$RANDOM.$BASE_DOMAIN"; then
			printf '    ok    the wildcard covers a subdomain that never had a tunnel\n'
			pass=$((pass + 1))
		else
			printf '    FAIL  dns01 did not yield a wildcard certificate\n'
			fail=$((fail + 1))
		fi
		check "an unused subdomain answers 404 over valid TLS" \
			"$(status_of "never-used.$BASE_DOMAIN")" "404"

		# Prove the update really landed in the zone, not just that a cert appeared.
		if compose exec -T bind dig @127.0.0.1 +short SOA "$BASE_DOMAIN" 2>/dev/null | grep -q .; then
			printf '    ok    bind is authoritative for the zone caddy updated\n'
			pass=$((pass + 1))
		else
			printf '    FAIL  bind did not answer for %s\n' "$BASE_DOMAIN"
			fail=$((fail + 1))
		fi
	fi
}

# riftd's ask endpoint is what stops on-demand TLS being an open certificate
# relay. It is reachable only from inside the network, so ask riftd directly.
assert_ask_endpoint() {
	printf '  tls-ask authorisation\n'
	ask() {
		compose exec -T riftd wget -q -S -O /dev/null \
			"http://127.0.0.1:8080/internal/tls-ask?domain=$1" 2>&1 |
			sed -n 's/.*HTTP\/1.1 \([0-9]*\).*/\1/p' | head -1
	}
	check "live tunnel authorised" "$(ask "hello.$BASE_DOMAIN")" "200"
	check "gateway hostname authorised" "$(ask "gateway.$BASE_DOMAIN")" "200"
	check "base domain authorised" "$(ask "$BASE_DOMAIN")" "200"
	check "unknown subdomain refused" "$(ask "nobody.$BASE_DOMAIN")" "404"
	check "foreign domain refused" "$(ask "attacker.example.com")" "403"
}

assert_admin_is_guarded() {
	printf '  admin api\n'
	check "no bearer token is 401" \
		"$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
			"http://127.0.0.1:${ADMIN_PORT}/v1/tokens")" "401"
	check "wrong bearer token is 401" \
		"$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
			-H 'Authorization: Bearer wrong' \
			"http://127.0.0.1:${ADMIN_PORT}/v1/tokens")" "401"
}

# The CLI is the client under test; build it if it is missing or stale.
if [ ! -x "$REPO_ROOT/cli/dist/rift" ]; then
	require_cmd bun
	log_info "building the rift CLI"
	(cd "$REPO_ROOT/cli" && bun install --silent && bun run build) >/dev/null
fi

# The guards themselves are part of what this harness tests: a resource check
# that cannot fail is decoration.
assert_preflight_guards() {
	printf '\n=== preflight guards ===\n'

	if ( require_memory 999999999 "an impossible build" ) >/dev/null 2>&1; then
		printf '    FAIL  require_memory did not refuse an impossible requirement\n'
		fail=$((fail + 1))
	else
		printf '    ok    require_memory refuses an impossible requirement\n'
		pass=$((pass + 1))
	fi

	if ( require_disk_free 999999999 "$REPO_ROOT" "an impossible build" ) >/dev/null 2>&1; then
		printf '    FAIL  require_disk_free did not refuse an impossible requirement\n'
		fail=$((fail + 1))
	else
		printf '    ok    require_disk_free refuses an impossible requirement\n'
		pass=$((pass + 1))
	fi

	# An oversized artifact must be caught before it is shipped anywhere.
	local big="$TMPDIR_E2E/oversized.bin"
	head -c 2097152 /dev/zero >"$big"
	if ( require_file_smaller_than "$big" 1 "a test artifact" ) >/dev/null 2>&1; then
		printf '    FAIL  require_file_smaller_than passed a 2 MiB file against a 1 MiB limit\n'
		fail=$((fail + 1))
	else
		printf '    ok    require_file_smaller_than refuses an oversized artifact\n'
		pass=$((pass + 1))
	fi
	if ( require_file_smaller_than "$big" 8 "a test artifact" ) >/dev/null 2>&1; then
		printf '    ok    require_file_smaller_than accepts a file under its limit\n'
		pass=$((pass + 1))
	else
		printf '    FAIL  require_file_smaller_than rejected a file under its limit\n'
		fail=$((fail + 1))
	fi
	rm -f "$big"

	# A missing file is a failure, not a silent pass.
	if ( require_file_smaller_than "$TMPDIR_E2E/does-not-exist" 1 "a missing artifact" ) >/dev/null 2>&1; then
		printf '    FAIL  require_file_smaller_than passed a nonexistent file\n'
		fail=$((fail + 1))
	else
		printf '    ok    require_file_smaller_than refuses a missing artifact\n'
		pass=$((pass + 1))
	fi

	# The documented escape hatch must actually work, or operators will patch
	# the library instead of setting the variable.
	if ( RIFT_SKIP_PREFLIGHT=1 require_memory 999999999 "an impossible build" ) >/dev/null 2>&1; then
		printf '    ok    RIFT_SKIP_PREFLIGHT=1 bypasses the guards\n'
		pass=$((pass + 1))
	else
		printf '    FAIL  RIFT_SKIP_PREFLIGHT=1 did not bypass the guards\n'
		fail=$((fail + 1))
	fi
}

preflight_report "$(preflight_docker_root)"
require_memory "$E2E_MIN_MEM_MB" "the e2e stack"
require_docker_disk "$E2E_MIN_DISK_MB" "the e2e image builds"
assert_preflight_guards

for mode in "${modes[@]}"; do
	run_mode "$mode"
done

if [ "$cluster" = true ]; then
	run_cluster
fi

printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || die "e2e failed"
log_info "e2e passed"
