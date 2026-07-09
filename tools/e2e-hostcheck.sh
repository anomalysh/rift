#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

COMPOSE_FILE="$REPO_ROOT/deploy/docker-compose.hostcheck.yml"
PROJECT="rift-hostcheck"

# harden.sh, as seen from inside the container (tools/ is bind-mounted here).
HARDEN="/opt/rift/tools/harden.sh"

usage() {
	cat >&2 <<EOF
Usage: tools/e2e-hostcheck.sh [--keep] [--verbose]

Prove tools/harden.sh actually hardens a host. Builds a throwaway Debian trixie
container (mirroring the production VPS), starts sshd inside it, mints a
throwaway SSH keypair, and asserts every hardening area took effect -- with real
observations (sshd -T, a live login, nft list, valid JSON) rather than by
grepping the script's own output.

Everything happens INSIDE the container: nothing here touches this machine or the
real VPS, and the container has no published ports, no host network and no docker
socket, so it can neither reach the host nor phone out.

Options:
  --keep      Leave the last container running afterwards, for poking at.
  --verbose   Print harden.sh output on failure.
  -h, --help  Show this help and exit.
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

require_cmd docker jq

TMP="$(mktemp -d)"
pass=0
fail=0

cleanup() {
	local status=$?
	if [ "$keep" != true ]; then
		compose down -v --remove-orphans >/dev/null 2>&1 || true
	else
		log_warn "container left running (--keep); tear down with:"
		log_warn "  docker compose -f $COMPOSE_FILE -p $PROJECT down -v"
	fi
	rm -rf "$TMP"
	exit "$status"
}
trap cleanup EXIT INT TERM

compose() { docker compose -f "$COMPOSE_FILE" -p "$PROJECT" "$@"; }
dexec() { compose exec -T hostcheck "$@"; }
dscript() { compose exec -T hostcheck bash -s; }

# BuildKit builds inside its own container, which fails on hosts with a
# misconfigured runtime; the legacy builder produces the same image, so fall
# back to it rather than making the harness unusable. Mirrors tools/e2e.sh.
build_image() {
	if compose build >"$TMP/build.log" 2>&1; then return 0; fi
	log_warn "buildkit build failed; retrying with the legacy builder"
	if DOCKER_BUILDKIT=0 compose build >>"$TMP/build.log" 2>&1; then return 0; fi
	tail -20 "$TMP/build.log" >&2
	die "could not build the hostcheck image"
}

wait_container() {
	for _ in $(seq 1 30); do
		if dexec true >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	die "hostcheck container did not come up"
}

fresh_container() {
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	compose up -d >/dev/null
	wait_container
}

# run_harden <label> [args...] -> sets HRC to the exit code, output in $TMP/<label>.log
run_harden() {
	local label="$1"
	shift
	HRC=0
	dexec bash "$HARDEN" "$@" >"$TMP/$label.log" 2>&1 || HRC=$?
	if [ "$verbose" = true ]; then
		printf '    --- harden %s (rc=%d) ---\n' "$label" "$HRC" >&2
		sed 's/^/    | /' "$TMP/$label.log" >&2
	fi
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
	if printf '%s' "$haystack" | grep -qF -- "$needle"; then
		printf '    ok    %s\n' "$name"
		pass=$((pass + 1))
	else
		printf '    FAIL  %s: %q does not contain %q\n' "$name" "$haystack" "$needle"
		fail=$((fail + 1))
	fi
}

# ============================================================================
phase_main() {
	printf '\n=== phase: hardening takes effect ===\n'
	fresh_container

	# Prepare the container: host keys, a throwaway keypair installed for root,
	# an unrelated daemon.json key to prove the merge preserves it, and a running
	# sshd. The keypair lives in /tmp, never in the repo, and no key is secret.
	dscript <<'SH'
set -e
mkdir -p /run/sshd /root/.ssh /etc/docker
chmod 700 /root/.ssh
ssh-keygen -A >/dev/null
rm -f /tmp/rk_id /tmp/rk_id.pub
ssh-keygen -t ed25519 -N '' -f /tmp/rk_id -q
install -m 600 /tmp/rk_id.pub /root/.ssh/authorized_keys
printf '{"userland-proxy": false}\n' >/etc/docker/daemon.json
/usr/sbin/sshd
SH

	# A --check that cannot fail proves nothing: nothing is hardened yet, so it
	# must fail.
	run_harden precheck --check
	check "harden --check fails before hardening" "$([ "$HRC" -ne 0 ] && echo nonzero || echo zero)" "nonzero"

	run_harden apply
	check "harden apply exits zero" "$HRC" "0"

	printf '  ssh\n'
	local eff pw prl kbd
	eff="$(dexec sshd -T 2>/dev/null || true)"
	pw="$(printf '%s\n' "$eff" | awk '/^passwordauthentication /{print $2}')"
	prl="$(printf '%s\n' "$eff" | awk '/^permitrootlogin /{print $2}')"
	kbd="$(printf '%s\n' "$eff" | awk '/^kbdinteractiveauthentication /{print $2}')"
	check "sshd -T passwordauthentication no" "$pw" "no"
	# sshd -T prints prohibit-password's canonical alias without-password; both
	# mean root may log in by key but never by password.
	check "sshd -T permitrootlogin is key-only" \
		"$([ "$prl" = prohibit-password ] || [ "$prl" = without-password ] && echo keyonly || echo "$prl")" "keyonly"
	check "sshd -T kbdinteractiveauthentication no" "$kbd" "no"

	local trc=0
	dexec sshd -t >/dev/null 2>&1 || trc=$?
	check "sshd -t accepts the hardened config" "$trc" "0"

	# The assertion that matters most: a live sshd inside the container refuses a
	# password login and accepts a key login.
	local offered
	offered="$(dexec ssh -v -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o BatchMode=yes -o ConnectTimeout=5 -p 22 root@127.0.0.1 true 2>&1 || true)"
	check_contains "server offers publickey auth" "$offered" "Authentications that can continue: publickey"
	check "server no longer offers password auth" \
		"$(printf '%s' "$offered" | grep -qi 'continue:.*password' && echo offers || echo no)" "no"

	local pwlogin=refused
	if dexec ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o PreferredAuthentications=password -o PubkeyAuthentication=no \
		-o NumberOfPasswordPrompts=1 -o ConnectTimeout=5 -p 22 root@127.0.0.1 true \
		</dev/null >/dev/null 2>&1; then
		pwlogin=allowed
	fi
	check "password login is refused" "$pwlogin" "refused"

	local keyout
	keyout="$(dexec ssh -i /tmp/rk_id -o IdentitiesOnly=yes -o StrictHostKeyChecking=no \
		-o UserKnownHostsFile=/dev/null -o PreferredAuthentications=publickey \
		-o ConnectTimeout=5 -p 22 root@127.0.0.1 echo KEYLOGIN_OK 2>/dev/null || true)"
	check "key login succeeds" "$keyout" "KEYLOGIN_OK"

	printf '  nftables\n'
	local ncrc=0
	dexec nft -c -f /etc/nftables.conf >/dev/null 2>&1 || ncrc=$?
	check "nft -c validates /etc/nftables.conf" "$ncrc" "0"
	local ruleset
	ruleset="$(dexec nft list ruleset 2>/dev/null || true)"
	check_contains "ruleset accepts tcp 22/80/443" "$ruleset" "tcp dport { 22, 80, 443 } accept"
	check_contains "input chain default-drops inbound" "$ruleset" "policy drop"
	check_contains "icmp is accepted (no PMTU black-hole)" "$ruleset" "meta l4proto icmp accept"

	printf '  docker\n'
	local dj jrc=0
	dj="$(dexec cat /etc/docker/daemon.json 2>/dev/null || true)"
	printf '%s' "$dj" | jq -e . >/dev/null 2>&1 || jrc=$?
	check "daemon.json is valid JSON" "$jrc" "0"
	check "log-opts max-size merged" "$(printf '%s' "$dj" | jq -r '."log-opts"."max-size"')" "10m"
	check "log-opts max-file merged" "$(printf '%s' "$dj" | jq -r '."log-opts"."max-file"')" "3"
	check "log-driver set" "$(printf '%s' "$dj" | jq -r '."log-driver"')" "json-file"
	check "pre-existing unrelated key survives the merge" \
		"$(printf '%s' "$dj" | jq -r '."userland-proxy"')" "false"

	printf '  fail2ban\n'
	local f2brc=0
	dexec fail2ban-client -t >/dev/null 2>&1 || f2brc=$?
	check "fail2ban-client -t passes" "$f2brc" "0"
	local dump
	dump="$(dexec fail2ban-client -d 2>/dev/null || true)"
	check_contains "sshd jail is enabled" "$dump" "'add', 'sshd'"

	printf '  unattended-upgrades\n'
	local uarc=0
	dexec apt-config dump >/dev/null 2>&1 || uarc=$?
	check "apt-config parses after hardening" "$uarc" "0"
	check "periodic unattended-upgrade enabled" \
		"$(dexec bash -c 'test -e /etc/apt/apt.conf.d/20auto-upgrades && echo yes || echo no')" "yes"
	local uconf
	uconf="$(dexec cat /etc/apt/apt.conf.d/52rift-unattended-upgrades 2>/dev/null || true)"
	check_contains "security-only origins configured" "$uconf" "security,label=Debian-Security"

	printf '  idempotency\n'
	run_harden apply2
	check "second apply exits zero" "$HRC" "0"
	check_contains "second apply reports no changes" "$(cat "$TMP/apply2.log")" "changes=0"
	run_harden recheck --check
	check "harden --check passes after hardening" "$HRC" "0"
}

phase_lockout() {
	printf '\n=== phase: lockout guard (the most important test) ===\n'
	fresh_container

	# Marker present (from the image), but root has NO authorized_keys. Disabling
	# password auth here would be a permanent lockout, so harden.sh must refuse.
	dscript <<'SH'
set -e
mkdir -p /run/sshd /root/.ssh
chmod 700 /root/.ssh
ssh-keygen -A >/dev/null
rm -f /root/.ssh/authorized_keys
/usr/sbin/sshd
SH

	run_harden lockout
	check "harden refuses without a usable key" "$([ "$HRC" -ne 0 ] && echo nonzero || echo zero)" "nonzero"
	check "no ssh drop-in was written" \
		"$(dexec bash -c 'test -e /etc/ssh/sshd_config.d/99-rift.conf && echo yes || echo no')" "no"
	check "password auth is still enabled" \
		"$(dexec sshd -T 2>/dev/null | awk '/^passwordauthentication /{print $2}')" "yes"
}

phase_workstation() {
	printf '\n=== phase: workstation guard ===\n'
	fresh_container

	# Remove the provisioning marker: harden.sh must abort before touching
	# anything, so it can never harden a developer machine by accident.
	dexec rm -f /etc/rift-hostcheck-target

	run_harden workstation
	check "harden refuses without the provisioning marker" \
		"$([ "$HRC" -ne 0 ] && echo nonzero || echo zero)" "nonzero"
	check "no ssh drop-in was written" \
		"$(dexec bash -c 'test -e /etc/ssh/sshd_config.d/99-rift.conf && echo yes || echo no')" "no"
	check "no nftables config was written" \
		"$(dexec bash -c 'test -e /etc/docker/daemon.json && echo yes || echo no')" "no"
}

log_info "building the hostcheck image (debian:trixie mirror of the VPS)"
build_image

phase_main
phase_lockout
phase_workstation

printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || die "hostcheck e2e failed"
log_info "hostcheck e2e passed"
