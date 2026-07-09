#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

# Ordered so the SSH lockout guard (the whole point of this script) runs first:
# if it aborts, nothing else has been touched.
ALL_AREAS=(ssh fail2ban nftables docker unattended)

# The workstation guard aborts unless this marker exists. deploy/Dockerfile.hostcheck
# creates it in the throwaway test image; a real VPS gets it during provisioning.
MARKER="/etc/rift-hostcheck-target"

# The user whose authorized_keys must hold a key before password auth is
# disabled. Access to the VPS is root-over-key, so root is the default.
SSH_KEY_USER="${RIFT_HARDEN_SSH_USER:-root}"

SSH_DROPIN="/etc/ssh/sshd_config.d/99-rift.conf"
F2B_JAIL="/etc/fail2ban/jail.d/rift.conf"
NFT_CONF="/etc/nftables.conf"
DOCKER_DAEMON_JSON="/etc/docker/daemon.json"
UU_AUTO="/etc/apt/apt.conf.d/20auto-upgrades"
UU_CONF="/etc/apt/apt.conf.d/52rift-unattended-upgrades"

MODE="apply" # apply | check | dryrun
FORCE=false
RESTART_DOCKER=false
only=()
skip=()
CHANGES=0

usage() {
	cat >&2 <<EOF
Usage: tools/harden.sh [--check|--dry-run] [--only AREA] [--skip AREA]
                       [--restart-docker] [--ssh-user NAME] [--force]

Idempotent host hardening for a rift VPS (Debian). Running it twice changes
nothing the second time. It refuses to run unless it is root and unless the
provisioning marker $MARKER exists (pass --force to override).

Areas (run in this order):
  ssh          key-only auth via a drop-in in sshd_config.d; refuses to disable
               password auth unless a key is present and sshd accepts the config
  fail2ban     enable the sshd jail with sane ban settings
  nftables     inet filter: accept 22/80/443 + loopback/established/icmp, drop
               the rest of inbound; coexists with Docker's own chains
  docker       json-file log rotation merged into daemon.json (no daemon restart)
  unattended   unattended-upgrades for the security pocket only

Modes:
  (default)      apply the changes
  --check        verify every hardened state, change nothing, exit non-zero if
                 anything is not hardened. Quiet and exit-code-driven: suitable
                 as a CI gate.
  --dry-run      report what would change, change nothing, always exit zero

Options:
  --only AREA        run only this area (repeatable, or comma-separated)
  --skip AREA        skip this area (repeatable, or comma-separated)
  --restart-docker   after the docker area, restart dockerd (opt-in only; it
                     kills the running rift stack, so it is never automatic)
  --ssh-user NAME    user whose authorized_keys is checked (default: $SSH_KEY_USER)
  --force            bypass the $MARKER workstation guard
  -h, --help         show this help and exit
EOF
}

in_list() {
	local needle="$1"
	shift
	local x
	for x in "$@"; do [ "$x" = "$needle" ] && return 0; done
	return 1
}

add_areas() {
	# add_areas <dest-array-name> <comma-or-space list>
	local dest="$1" list="$2" it
	local -a items
	IFS=', ' read -r -a items <<<"$list"
	for it in "${items[@]}"; do
		[ -n "$it" ] || continue
		in_list "$it" "${ALL_AREAS[@]}" || die "unknown area: $it (valid: ${ALL_AREAS[*]})"
		eval "$dest+=(\"\$it\")"
	done
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--check) MODE="check" ;;
	--dry-run) MODE="dryrun" ;;
	--force) FORCE=true ;;
	--restart-docker) RESTART_DOCKER=true ;;
	--only)
		shift
		[ "$#" -gt 0 ] || die "--only needs a value"
		add_areas only "$1"
		;;
	--skip)
		shift
		[ "$#" -gt 0 ] || die "--skip needs a value"
		add_areas skip "$1"
		;;
	--ssh-user)
		shift
		[ "$#" -gt 0 ] || die "--ssh-user needs a value"
		SSH_KEY_USER="$1"
		;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

# --- reporting --------------------------------------------------------------
# In --check, note_change is the only thing that fails the run, and it prints to
# stderr; stdout stays clean so a CI gate reads the exit code, not the chatter.
note_change() {
	CHANGES=$((CHANGES + 1))
	case "$MODE" in
	apply) log_info "changed:      $*" ;;
	dryrun) log_info "would change: $*" ;;
	check) log_warn "not hardened: $*" ;;
	esac
}
note_ok() { [ "$MODE" = apply ] && log_info "ok:           $*"; return 0; }

# write_file <path> <mode>  -- content on stdin, staged then renamed into place.
write_file() {
	local path="$1" mode="$2" dir tmp
	dir="$(dirname "$path")"
	[ -d "$dir" ] || mkdir -p "$dir"
	tmp="$path.rift.$$"
	cat >"$tmp"
	chmod "$mode" "$tmp"
	mv -f "$tmp" "$path"
}

# ensure_managed_file <label> <path> <mode> <desired>
#   returns 0 if already compliant, 1 if a change is needed (and, in apply mode,
#   performed). Never a fatal error, so callers must use `|| rc=1`.
ensure_managed_file() {
	local label="$1" path="$2" mode="$3" desired="$4" current="" curmode=""
	if [ -f "$path" ]; then
		current="$(cat "$path")"
		curmode="$(stat -c '%a' "$path" 2>/dev/null || echo '')"
	fi
	if [ "$current" = "$desired" ] && [ "$curmode" = "$mode" ]; then
		note_ok "$label ($path)"
		return 0
	fi
	note_change "$label ($path)"
	[ "$MODE" = apply ] && printf '%s\n' "$desired" | write_file "$path" "$mode"
	return 1
}

# ensure_pkg <probe-cmd> <package>  -- install if missing (apply only). Always
# returns 0 so `set -e` does not treat "needs installing" as fatal.
ensure_pkg() {
	if command -v "$1" >/dev/null 2>&1 || dpkg -s "$2" >/dev/null 2>&1; then
		note_ok "package $2 present"
		return 0
	fi
	note_change "install package $2"
	if [ "$MODE" = apply ]; then
		DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$2" >/dev/null 2>&1 ||
			die "failed to install $2 (is apt available and online?)"
	fi
	return 0
}

json_valid() {
	if command -v python3 >/dev/null 2>&1; then
		python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$1" >/dev/null 2>&1
	elif command -v jq >/dev/null 2>&1; then
		jq -e . "$1" >/dev/null 2>&1
	else
		return 0
	fi
}

# systemd_running -- true only when systemd is the init and up. The hostcheck
# container has no systemd, so every area falls back to a non-systemd path.
systemd_running() { [ -d /run/systemd/system ]; }

# === SSH ====================================================================
ssh_accepts_challengeresponse() {
	# ChallengeResponseAuthentication is a deprecated alias for
	# KbdInteractiveAuthentication that some sshd builds have dropped. Probe with
	# a throwaway config and only emit the line if this build still parses it,
	# rather than have `sshd -t` reject the whole drop-in later. Trust the probe
	# only if an empty config validates here first; otherwise (e.g. no host keys)
	# we cannot tell the keyword apart from an unrelated failure, so skip it.
	local probe rc=1
	probe="$(mktemp)"
	printf 'ChallengeResponseAuthentication no\n' >"$probe"
	if sshd -t -f /dev/null >/dev/null 2>&1; then
		sshd -t -f "$probe" >/dev/null 2>&1 && rc=0
	fi
	rm -f "$probe"
	return $rc
}

ssh_desired_dropin() {
	printf '%s\n' \
		"# Managed by rift tools/harden.sh -- do not edit by hand." \
		"# Key-only SSH: no password or keyboard-interactive login paths." \
		"PasswordAuthentication no" \
		"PermitRootLogin prohibit-password" \
		"KbdInteractiveAuthentication no"
	if ssh_accepts_challengeresponse; then
		printf '%s\n' "ChallengeResponseAuthentication no"
	fi
}

ssh_has_authorized_key() {
	local user="$1" home ak
	home="$(getent passwd "$user" 2>/dev/null | cut -d: -f6)"
	[ -n "$home" ] || return 1
	ak="$home/.ssh/authorized_keys"
	[ -f "$ak" ] || return 1
	# At least one non-blank, non-comment line.
	grep -Eq '^[[:space:]]*[^#[:space:]]' "$ak"
}

ssh_check_match() {
	local eff="$1" key="$2" pattern="$3" desc="$4"
	if printf '%s\n' "$eff" | grep -Eqi "^${key} (${pattern})$"; then
		note_ok "sshd ${key} is ${desc}"
	else
		note_change "sshd ${key} is not ${desc}"
	fi
}

reload_sshd() {
	# systemctl when systemd is running, otherwise SIGHUP the sshd master -- the
	# hostcheck container has no init. A missing sshd is not fatal here: the
	# drop-in takes effect the next time sshd starts.
	if systemd_running; then
		systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null ||
			die "could not reload sshd via systemctl"
		return 0
	fi
	local pid=""
	if [ -f /run/sshd.pid ]; then
		pid="$(cat /run/sshd.pid 2>/dev/null || true)"
	fi
	[ -n "$pid" ] || pid="$(pgrep -o -x sshd 2>/dev/null || true)"
	if [ -n "$pid" ]; then
		kill -HUP "$pid"
	else
		log_warn "sshd is not running; the hardened drop-in applies when it next starts"
	fi
}

area_ssh() {
	local desired
	desired="$(ssh_desired_dropin)"

	if [ "$MODE" = check ]; then
		local eff
		eff="$(sshd -T 2>/dev/null || true)"
		# sshd -T prints prohibit-password's canonical alias without-password.
		ssh_check_match "$eff" passwordauthentication 'no' 'no'
		ssh_check_match "$eff" permitrootlogin 'prohibit-password|without-password' 'prohibit-password'
		ssh_check_match "$eff" kbdinteractiveauthentication 'no' 'no'
		return 0
	fi

	local current=""
	[ -f "$SSH_DROPIN" ] && current="$(cat "$SSH_DROPIN")"
	if [ "$current" = "$desired" ]; then
		note_ok "ssh hardening drop-in ($SSH_DROPIN)"
		return 0
	fi

	note_change "ssh hardening drop-in ($SSH_DROPIN)"
	[ "$MODE" = apply ] || return 0

	# LOCKOUT PREVENTION -- the entire reason this script exists.
	# Disabling password auth with no usable key is a permanent, unrecoverable
	# lockout. Refuse unless (a) the target user already has a key AND (b) sshd
	# accepts the new config. The key check comes BEFORE we write anything, so a
	# refusal leaves the host exactly as it was. Do not weaken either check.
	ssh_has_authorized_key "$SSH_KEY_USER" ||
		die "refusing to disable password auth: no key in ${SSH_KEY_USER}'s ~/.ssh/authorized_keys -- this would lock you out permanently"

	printf '%s\n' "$desired" | write_file "$SSH_DROPIN" 644

	# Validate the whole effective config; on any rejection, revert and abort
	# rather than leave sshd unable to start.
	if ! sshd -t >/dev/null 2>&1; then
		rm -f "$SSH_DROPIN"
		die "sshd -t rejected the new config; reverted $SSH_DROPIN and changed nothing"
	fi
	reload_sshd
	note_ok "sshd reloaded with hardened config"
	return 0
}

# === fail2ban ===============================================================
f2b_desired() {
	printf '%s\n' \
		"# Managed by rift tools/harden.sh." \
		"[sshd]" \
		"enabled = true" \
		"maxretry = 5" \
		"findtime = 10m" \
		"bantime = 1h"
}

area_fail2ban() {
	ensure_pkg fail2ban-server fail2ban
	local rc=0
	ensure_managed_file "fail2ban sshd jail" "$F2B_JAIL" 644 "$(f2b_desired)" || rc=1

	# Config test (no running daemon required, so it works in the container).
	if command -v fail2ban-client >/dev/null 2>&1; then
		if fail2ban-client -t >/dev/null 2>&1; then
			[ "$MODE" = check ] && note_ok "fail2ban config valid" || true
		elif [ "$MODE" = apply ] && [ "$rc" = 1 ]; then
			die "fail2ban-client -t rejected the config after writing $F2B_JAIL"
		else
			note_change "fail2ban-client -t reports an invalid config"
		fi
	fi

	if [ "$MODE" = apply ] && [ "$rc" = 1 ] && systemd_running; then
		systemctl enable fail2ban >/dev/null 2>&1 || true
		systemctl reload-or-restart fail2ban >/dev/null 2>&1 || true
	fi
	return 0
}

# === nftables ===============================================================
nft_desired() {
	cat <<'EOF'
#!/usr/sbin/nft -f
# Managed by rift tools/harden.sh.
#
# This defines ONLY an `inet filter` table. Docker installs its own rules in the
# `ip` family (the nat table plus the DOCKER / DOCKER-USER / FORWARD filter
# chains); those live in a different address family and are untouched by this
# file, so published container ports and outbound container traffic keep
# working. We deliberately never `flush ruleset`, which would wipe Docker's
# chains and break every running container until dockerd reinstalled them. The
# create/delete/recreate below scopes the reset to our table alone, which is
# what makes reloading this file idempotent.
#
# No `forward` base chain is added on purpose: adding one would drop us into the
# forward path alongside Docker's own filtering, for no benefit here. ICMP and
# ICMPv6 are accepted, not dropped -- dropping them black-holes Path MTU
# Discovery (silently breaking large replies over some paths) and ordinary
# ping/diagnostics, with no security gain.

table inet filter { }
delete table inet filter

table inet filter {
	chain input {
		type filter hook input priority filter; policy drop;

		iif "lo" accept
		ct state established,related accept
		ct state invalid drop

		meta l4proto icmp accept
		meta l4proto ipv6-icmp accept

		tcp dport { 22, 80, 443 } accept
	}

	chain output {
		type filter hook output priority filter; policy accept;
	}
}
EOF
}

area_nftables() {
	local rc=0
	ensure_managed_file "nftables ruleset" "$NFT_CONF" 644 "$(nft_desired)" || rc=1

	if [ "$MODE" = check ]; then
		if nft list table inet filter >/dev/null 2>&1; then
			note_ok "nftables inet filter table loaded"
		else
			note_change "nftables inet filter table not loaded"
		fi
		return 0
	fi

	[ "$MODE" = apply ] || return 0

	# Validate before the ruleset ever reaches the kernel.
	nft -c -f "$NFT_CONF" >/dev/null 2>&1 || die "nft -c rejected $NFT_CONF; not loading it"

	# Reload only when the file changed or the table is absent, so a no-op run
	# stays a no-op.
	if [ "$rc" = 1 ] || ! nft list table inet filter >/dev/null 2>&1; then
		nft -f "$NFT_CONF" || die "failed to load $NFT_CONF"
		note_ok "nftables ruleset loaded"
	fi

	# Load /etc/nftables.conf at boot where systemd manages it.
	systemd_running && systemctl enable nftables >/dev/null 2>&1 || true
	return 0
}

# === Docker log rotation ====================================================
docker_merged() {
	# Print daemon.json with json-file log rotation merged in: existing keys are
	# preserved. Prefer python3 (always in the hostcheck image and on any real
	# Debian host); fall back to jq; refuse (exit 3) only if neither exists AND
	# there is an existing file we would otherwise clobber.
	local src="$DOCKER_DAEMON_JSON"
	if command -v python3 >/dev/null 2>&1; then
		python3 - "$src" <<'PY'
import json, os, sys
path = sys.argv[1]
data = {}
if os.path.exists(path):
    text = open(path).read().strip()
    if text:
        data = json.loads(text)          # invalid JSON raises -> caller aborts
if not isinstance(data, dict):
    raise SystemExit("existing daemon.json is not a JSON object")
data["log-driver"] = "json-file"
opts = data.get("log-opts")
if not isinstance(opts, dict):
    opts = {}
opts["max-size"] = "10m"
opts["max-file"] = "3"
data["log-opts"] = opts
print(json.dumps(data, indent=2, sort_keys=True))
PY
	elif command -v jq >/dev/null 2>&1; then
		local base='{}'
		[ -s "$src" ] && base="$(cat "$src")"
		printf '%s' "$base" | jq -S \
			'. + {"log-driver":"json-file"} | .["log-opts"] = ((.["log-opts"] // {}) + {"max-size":"10m","max-file":"3"})'
	elif [ -s "$src" ]; then
		return 3
	else
		printf '%s\n' \
			'{' \
			'  "log-driver": "json-file",' \
			'  "log-opts": {' \
			'    "max-file": "3",' \
			'    "max-size": "10m"' \
			'  }' \
			'}'
	fi
}

area_docker() {
	local merged
	if ! merged="$(docker_merged 2>/dev/null)"; then
		if [ -s "$DOCKER_DAEMON_JSON" ] && ! json_valid "$DOCKER_DAEMON_JSON"; then
			die "$DOCKER_DAEMON_JSON is not valid JSON; refusing to touch it"
		fi
		log_warn "no python3/jq to merge $DOCKER_DAEMON_JSON safely; skipping docker log rotation"
		return 0
	fi

	local rc=0
	ensure_managed_file "docker log rotation" "$DOCKER_DAEMON_JSON" 644 "$merged" || rc=1

	if [ "$MODE" = apply ]; then
		json_valid "$DOCKER_DAEMON_JSON" || die "wrote $DOCKER_DAEMON_JSON but it is not valid JSON"
	fi

	if [ "$rc" = 1 ] && [ "$MODE" = apply ]; then
		# A running dockerd only picks up daemon.json on restart. We do NOT
		# restart it from here: that would kill the live rift stack. Restarting
		# is an explicit --restart-docker opt-in.
		log_warn "docker log rotation written; run 'systemctl restart docker' for it to take effect"
		if [ "$RESTART_DOCKER" = true ]; then
			if systemd_running; then
				log_warn "--restart-docker set: restarting dockerd now (this drops the running stack)"
				systemctl restart docker || die "docker restart failed"
			else
				log_warn "--restart-docker set but systemd is not running; restart docker yourself"
			fi
		else
			log_warn "not restarting docker automatically; re-run with --restart-docker to opt in"
		fi
	fi
	return 0
}

# === unattended-upgrades ====================================================
uu_auto_desired() {
	printf '%s\n' \
		'// Managed by rift tools/harden.sh.' \
		'APT::Periodic::Update-Package-Lists "1";' \
		'APT::Periodic::Unattended-Upgrade "1";'
}

uu_conf_desired() {
	# ${distro_codename} is expanded by unattended-upgrades itself, not the
	# shell, so the heredoc is single-quoted to keep it literal.
	cat <<'EOF'
// Managed by rift tools/harden.sh.
// Security pocket only: apply security updates unattended, leave everything
// else to the operator.
Unattended-Upgrade::Origins-Pattern {
	"origin=Debian,codename=${distro_codename}-security,label=Debian-Security";
};
Unattended-Upgrade::Automatic-Reboot "false";
EOF
}

area_unattended() {
	ensure_pkg unattended-upgrade unattended-upgrades
	local rc=0
	ensure_managed_file "unattended-upgrades schedule" "$UU_AUTO" 644 "$(uu_auto_desired)" || rc=1
	ensure_managed_file "unattended-upgrades security policy" "$UU_CONF" 644 "$(uu_conf_desired)" || rc=1

	# apt-config parses every apt.conf.d file; a syntax error would break apt.
	if command -v apt-config >/dev/null 2>&1; then
		if apt-config dump >/dev/null 2>&1; then
			[ "$MODE" = check ] && note_ok "apt config parses" || true
		elif [ "$MODE" = apply ] && [ "$rc" = 1 ]; then
			die "apt-config rejected the config after writing the unattended-upgrades files"
		else
			note_change "apt-config reports an invalid config"
		fi
	fi
	return 0
}

# === main ===================================================================
# Both guards run before any area. They are cheap and their absence is
# catastrophic: this script permanently disables SSH password login, so running
# it as non-root would half-apply, and running it on the wrong machine (a
# developer laptop) is unrecoverable.
[ "$(id -u)" -eq 0 ] || die "must run as root (it changes sshd, nftables and the docker daemon)"

if [ ! -e "$MARKER" ] && [ "$FORCE" != true ]; then
	die "refusing to run: $MARKER not found. This host does not look like a rift provisioning target, and this script permanently disables SSH password login. Pass --force only if you are certain."
fi

selected=()
for a in "${ALL_AREAS[@]}"; do
	if [ "${#only[@]}" -gt 0 ] && ! in_list "$a" "${only[@]}"; then continue; fi
	if [ "${#skip[@]}" -gt 0 ] && in_list "$a" "${skip[@]}"; then continue; fi
	selected+=("$a")
done
[ "${#selected[@]}" -gt 0 ] || die "no areas selected"

[ "$MODE" = check ] || log_info "mode=$MODE areas='${selected[*]}'"

for a in "${selected[@]}"; do
	[ "$MODE" = check ] || printf '\n== %s ==\n' "$a" >&2
	"area_$a"
done

case "$MODE" in
check)
	if [ "$CHANGES" -eq 0 ]; then
		exit 0
	fi
	die "check failed: $CHANGES item(s) not hardened"
	;;
dryrun)
	printf '\n=== summary ===\n  changes=%d\n' "$CHANGES"
	log_info "dry-run complete: $CHANGES change(s) would be made"
	;;
apply)
	printf '\n=== summary ===\n  changes=%d\n' "$CHANGES"
	log_info "hardening complete: $CHANGES change(s) made"
	;;
esac
