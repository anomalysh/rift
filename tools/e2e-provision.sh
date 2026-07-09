#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# End-to-end test for tools/provision.sh and the Linode provider. Hermetic: it
# drives provision.sh against a MOCK Linode API and a throwaway sshd, both in
# Docker. No real cloud, no token, no cost. Every assertion is made against the
# mock's observed HTTP traffic (its request log) and real process exit codes --
# never by scraping the script's stdout.

COMPOSE_FILE="$REPO_ROOT/deploy/docker-compose.provision.yml"
PROJECT="rift-provision-e2e"

MOCK_PORT="${RIFT_PROVISION_MOCK_PORT:-18091}"
SSHD_PORT="${RIFT_PROVISION_SSHD_PORT:-18092}"
export RIFT_PROVISION_MOCK_PORT="$MOCK_PORT"
export RIFT_PROVISION_SSHD_PORT="$SSHD_PORT"

# A placeholder token: this is a MOCK, so any non-empty value is accepted. Never
# a real credential.
TOKEN="e2e-linode-token-not-a-secret-000000000000"

usage() {
	cat >&2 <<EOF
Usage: tools/e2e-provision.sh [--keep] [--verbose]

Drive tools/provision.sh end to end against a mock Linode API in Docker. Nothing
reaches a real cloud; provisioning is exercised with a placeholder token.

Options:
  --keep      Leave the containers running afterwards, for poking at.
  --verbose   Stream provision.sh output on failure.
EOF
}

keep=false
verbose=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--keep) keep=true ;;
	--verbose) verbose=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_cmd docker curl python3 ssh-keygen

TMPDIR_E2E="$(mktemp -d)"
EMPTY_ENV="$TMPDIR_E2E/empty.env"
: >"$EMPTY_ENV"

pass=0
fail=0

cleanup() {
	local status=$?
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

# Same buildkit fallback the main e2e uses: some hosts have a misconfigured
# container runtime that only the legacy builder tolerates.
build_images() {
	if compose build >"$TMPDIR_E2E/build.log" 2>&1; then
		return 0
	fi
	log_warn "buildkit build failed; retrying with the legacy builder"
	if DOCKER_BUILDKIT=0 compose build >>"$TMPDIR_E2E/build.log" 2>&1; then
		return 0
	fi
	tail -20 "$TMPDIR_E2E/build.log" >&2
	die "could not build the provisioning e2e images"
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

check_ge() {
	local name="$1" got="$2" min="$3"
	if [ "${got:-0}" -ge "$min" ] 2>/dev/null; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s: got [%s] want >= [%s]\n' "$name" "$got" "$min"
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

# --- mock control plane -----------------------------------------------------
mock_url() { printf 'http://127.0.0.1:%s' "$MOCK_PORT"; }
mock_reset() { curl -fsS -X POST "$(mock_url)/_mock/reset" >/dev/null; }
mock_config() {
	curl -fsS -X POST "$(mock_url)/_mock/config" \
		-H 'Content-Type: application/json' -d "$1" >/dev/null
}
mock_requests() { curl -fsS "$(mock_url)/_mock/requests"; }

wait_for_mock() {
	local i
	for i in $(seq 1 30); do
		if curl -fsS "$(mock_url)/_mock/requests" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	die "mock API never answered on $(mock_url)"
}

count_all_requests() { mock_requests | grep -c . || true; }

# count_requests METHOD PATH_REGEX -> number of matching logged requests.
count_requests() {
	mock_requests | python3 -c '
import sys, json, re
method, pat = sys.argv[1], re.compile(sys.argv[2])
n = 0
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    r = json.loads(line)
    if r.get("method") == method and pat.search(r.get("path", "")):
        n += 1
print(n)
' "$1" "$2"
}

count_status() {
	mock_requests | python3 -c '
import sys, json
want = int(sys.argv[1])
n = 0
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    if json.loads(line).get("status") == want:
        n += 1
print(n)
' "$1"
}

first_instance_id() {
	mock_requests | python3 -c '
import sys, json, re
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    m = re.match(r"^/v4/linode/instances/([0-9]+)$", json.loads(line).get("path", ""))
    if m:
        print(m.group(1))
        break
'
}

requests_contains() { mock_requests | grep -qF "$1"; }

json_field() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$1" "$2"; }
is_valid_json() { python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$1" >/dev/null 2>&1 && echo yes || echo no; }

# --- bring the stack up -----------------------------------------------------
log_info "building images"
build_images
log_info "starting mock + sshd"
compose down -v --remove-orphans >/dev/null 2>&1 || true
compose up -d >/dev/null

wait_for_tcp "$MOCK_PORT" "mock api"
wait_for_tcp "$SSHD_PORT" "sshd"
wait_for_mock

# Deploy key the provisioner installs. The comment is a marker the create-body
# assertion greps for.
KEY_MARKER="rift-e2e-marker"
ssh-keygen -t ed25519 -N '' -C "$KEY_MARKER" -f "$TMPDIR_E2E/id_e2e" >/dev/null
KEY_PUB="$TMPDIR_E2E/id_e2e.pub"

run() { # run RC_VAR LOGFILE -- args...  ; captures exit code without aborting
	local __rc_var="$1" __log="$2"
	shift 2
	local __rc=0
	"$@" >"$__log" 2>&1 || __rc=$?
	if [ "$verbose" = true ] && [ "$__rc" -ne 0 ]; then
		log_warn "--- output of: $* ---"
		cat "$__log" >&2
	fi
	printf -v "$__rc_var" '%s' "$__rc"
}

# ============================================================================
printf '\n=== 1. --dry-run makes zero HTTP requests and no state file ===\n'
mock_reset
DRY_STATE="$TMPDIR_E2E/dry-state.json"
rm -f "$DRY_STATE"
run rc "$TMPDIR_E2E/t1.log" env RIFT_LINODE_TOKEN="$TOKEN" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --api-base "$(mock_url)" --ssh-port "$SSHD_PORT" \
	--key "$KEY_PUB" --dry-run --name dry1 --state-file "$DRY_STATE"
check "dry-run exits 0" "$rc" "0"
check "dry-run makes zero HTTP requests" "$(count_all_requests)" "0"
check "dry-run writes no state file" "$([ -e "$DRY_STATE" ] && echo yes || echo no)" "no"

# ============================================================================
printf '\n=== 2. create without a token fails and the mock logs a 401 ===\n'
mock_reset
run rc "$TMPDIR_E2E/t2.log" env RIFT_LINODE_TOKEN="" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --api-base "$(mock_url)" --ssh-port "$SSHD_PORT" \
	--key "$KEY_PUB" --name notoken --status-timeout 5 --poll-interval 1 \
	--state-file "$TMPDIR_E2E/nt.json"
check "create without token exits non-zero" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check_ge "mock logged a 401" "$(count_status 401)" "1"

# ============================================================================
printf '\n=== 3. full create: one POST, key in body, polls, then SSH ===\n'
mock_reset
LIFE_STATE="$TMPDIR_E2E/life-state.json"
rm -f "$LIFE_STATE"
run rc "$TMPDIR_E2E/t3.log" env RIFT_LINODE_TOKEN="$TOKEN" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --provider linode --api-base "$(mock_url)" \
	--ssh-port "$SSHD_PORT" --key "$KEY_PUB" --name life1 \
	--status-timeout 30 --poll-interval 1 --state-file "$LIFE_STATE"
check "full create succeeds (SSH answered)" "$rc" "0"
check "exactly one POST to create the instance" "$(count_requests POST '^/v4/linode/instances$')" "1"
check "create body carries the deploy public key" \
	"$(requests_contains "$KEY_MARKER" && echo yes || echo no)" "yes"
check_ge "status endpoint was polled more than once" \
	"$(count_requests GET '^/v4/linode/instances/[0-9]+$')" "2"

# ============================================================================
printf '\n=== 4. state file has the right id and ipv4 and is valid JSON ===\n'
EXPECTED_ID="$(first_instance_id)"
check "state file exists" "$([ -f "$LIFE_STATE" ] && echo yes || echo no)" "yes"
check "state file is valid JSON" "$(is_valid_json "$LIFE_STATE")" "yes"
check "state instance_id matches the API id" "$(json_field "$LIFE_STATE" instance_id)" "$EXPECTED_ID"
check "state ipv4 is the instance ipv4" "$(json_field "$LIFE_STATE" ipv4)" "127.0.0.1"
check "state records the provider" "$(json_field "$LIFE_STATE" provider)" "linode"

# ============================================================================
printf '\n=== 5. --list shows the created instance ===\n'
LIST_OUT="$(env RIFT_LINODE_TOKEN="$TOKEN" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --provider linode --api-base "$(mock_url)" --list 2>/dev/null || true)"
check_ge "--list shows the created instance id" \
	"$(printf '%s' "$LIST_OUT" | grep -Ec "^${EXPECTED_ID}([^0-9]|$)" || true)" "1"

# ============================================================================
printf '\n=== 6. --destroy is idempotent (0 first, 0 again on 404) ===\n'
run rc1 "$TMPDIR_E2E/d1.log" env RIFT_LINODE_TOKEN="$TOKEN" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --provider linode --api-base "$(mock_url)" --destroy "$EXPECTED_ID"
check "first --destroy succeeds" "$rc1" "0"
run rc2 "$TMPDIR_E2E/d2.log" env RIFT_LINODE_TOKEN="$TOKEN" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --provider linode --api-base "$(mock_url)" --destroy "$EXPECTED_ID"
check "second --destroy is idempotent (still 0 on 404)" "$rc2" "0"
check_ge "two DELETEs were issued" "$(count_requests DELETE '^/v4/linode/instances/[0-9]+$')" "2"

# ============================================================================
printf '\n=== 7. a status timeout leaves the instance alive (no DELETE) ===\n'
mock_reset
mock_config '{"provisioning_polls": 100000}'
TO_STATE="$TMPDIR_E2E/timeout-state.json"
rm -f "$TO_STATE"
run rc "$TMPDIR_E2E/t7.log" env RIFT_LINODE_TOKEN="$TOKEN" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --provider linode --api-base "$(mock_url)" \
	--ssh-port "$SSHD_PORT" --key "$KEY_PUB" --name t7 \
	--status-timeout 3 --poll-interval 1 --state-file "$TO_STATE"
check "status timeout exits non-zero" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check "status timeout issued no DELETE" "$(count_requests DELETE '^/v4/linode/instances/')" "0"
check "status timeout wrote no state file" "$([ -e "$TO_STATE" ] && echo yes || echo no)" "no"

# ============================================================================
printf '\n=== 8. an unknown provider fails, naming the providers that exist ===\n'
run rc "$TMPDIR_E2E/t8.log" env RIFT_LINODE_TOKEN="$TOKEN" RIFT_ENV_FILE="$EMPTY_ENV" \
	"$SCRIPT_DIR/provision.sh" --provider nope --api-base "$(mock_url)" \
	--key "$KEY_PUB" --name x
check "unknown provider exits non-zero" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check_contains "error names the unknown provider" "$(cat "$TMPDIR_E2E/t8.log")" "unknown provider: nope"
check_contains "error lists the available providers" "$(cat "$TMPDIR_E2E/t8.log")" "linode"

# ============================================================================
printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || die "provisioning e2e failed"
log_info "provisioning e2e passed"
