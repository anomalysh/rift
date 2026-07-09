#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

COMPOSE_FILE="$REPO_ROOT/deploy/docker-compose.e2e.yml"
PROJECT="rift-e2e"

BASE_DOMAIN="rift.localtest"
HTTPS_PORT="18443"
HTTP_PORT="18080"
GATEWAY_PORT="18081"
ADMIN_PORT="18082"
UPSTREAM_PORT="13099"
ADMIN_TOKEN="e2e-admin-token-not-a-secret-0000000000000"

usage() {
	cat >&2 <<EOF
Usage: tools/e2e.sh [--mode MODE ...] [--keep] [--verbose]

Bring up the full rift stack in Docker -- Postgres, riftd, and a real Caddy
using the production Caddyfile -- and drive it end to end over HTTPS with the
compiled rift CLI. Hermetic: nothing reaches the internet.

The base domain is $BASE_DOMAIN. Requests use \`curl --resolve\`, so nothing is
written to /etc/hosts and nothing needs root.

Modes (repeatable; default: internal self):
  internal   Caddy's own CA signs a wildcard. Certificate coverage matches the
             dns01 production mode exactly, so this is the local stand-in for a
             wildcard deployment.
  self       An operator-supplied wildcard certificate, generated here.

  http01 is deliberately NOT covered: on-demand issuance needs a real ACME
  server. Its *authorisation* logic -- the ask endpoint that decides which
  hostnames may get a certificate -- is asserted directly in every mode, and
  again in the Go suite.

Options:
  --mode MODE   Run this mode (repeat for several).
  --keep        Leave the stack running afterwards, for poking at.
  --verbose     Stream container logs on failure and set riftd to debug.
EOF
}

modes=()
keep=false
verbose=false
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
	--keep) keep=true ;;
	--verbose) verbose=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done
[ "${#modes[@]}" -gt 0 ] || modes=(internal self)

require_cmd docker curl openssl python3

TMPDIR_E2E="$(mktemp -d)"
UPSTREAM_PID=""
CLI_PID=""

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

	case "$mode" in
	self)
		generate_self_cert
		export RIFT_E2E_CERT_DIR="$TMPDIR_E2E/certs"
		export RIFT_TLS_CERT_FILE="/certs/fullchain.pem"
		export RIFT_TLS_KEY_FILE="/certs/key.pem"
		;;
	internal)
		export RIFT_E2E_CERT_DIR="$REPO_ROOT/deploy/caddy"
		export RIFT_TLS_CERT_FILE="" RIFT_TLS_KEY_FILE=""
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
	esac

	log_info "starting upstream on 127.0.0.1:$UPSTREAM_PORT"
	python3 "$SCRIPT_DIR/e2e/upstream.py" "$UPSTREAM_PORT" >"$TMPDIR_E2E/upstream.log" 2>&1 &
	UPSTREAM_PID=$!
	wait_for_tcp "$UPSTREAM_PORT" "upstream"

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

	# A subdomain with no tunnel: a wildcard-covered mode answers 404. This is
	# the behaviour http01 cannot provide, and the reason dns01 exists.
	check "unused subdomain answers 404" "$(status_of "nobody.$BASE_DOMAIN")" "404"
	check_contains "unused subdomain explains itself" \
		"$(body_of "nobody.$BASE_DOMAIN")" "No tunnel is currently serving"

	check "gateway hostname is reachable" \
		"$(status_of "gateway.$BASE_DOMAIN" "/healthz")" "200"

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
	local host
	for host in "hello.$BASE_DOMAIN" "nobody.$BASE_DOMAIN" "$BASE_DOMAIN" "gateway.$BASE_DOMAIN"; do
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

for mode in "${modes[@]}"; do
	run_mode "$mode"
done

printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || die "e2e failed"
log_info "e2e passed"
