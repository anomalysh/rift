#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

usage() {
	cat >&2 <<'EOF'
Usage: tools/ssh-provision-key.sh

Bootstrap key-based SSH to the tunl VPS. Idempotent — safe to re-run.
  1. Generates tools/.ssh/id_ed25519 if it does not exist.
  2. Installs the public key on the VPS (password auth, this step only).
  3. Verifies key-only login works.
  4. Prints the next hardening steps (disable password auth, rotate password).

Environment:
  TUNL_VPS_HOST      (required) VPS hostname or IP
  TUNL_VPS_USER      SSH user            (default: root)
  TUNL_VPS_PORT      SSH port            (default: 22)
  TUNL_VPS_PASSWORD  (required) used ONLY to install the key
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
"") ;;
*) die "unexpected argument: $1 (see --help)" ;;
esac

require_cmd ssh ssh-keygen sshpass
require_env TUNL_VPS_HOST TUNL_VPS_PASSWORD

host="$TUNL_VPS_HOST"
user="${TUNL_VPS_USER:-root}"
port="${TUNL_VPS_PORT:-22}"
key="$(tunl_ssh_key_path)"
ssh_dir="$(dirname "$key")"

# 1. Generate the key if absent.
if [ -f "$key" ]; then
	log_info "deploy key already exists, not regenerating: $key"
else
	log_info "generating ed25519 deploy key: $key"
	mkdir -p "$ssh_dir"
	chmod 700 "$ssh_dir"
	ssh-keygen -t ed25519 -N '' -C "tunl-deploy" -f "$key"
	chmod 600 "$key"
fi

pub="$(cat "$key.pub")"

# 2. Install the public key using password auth (bootstrap only).
#    Export SSHPASS so sshpass reads it from the environment, never argv.
log_info "installing public key on $user@$host via password auth"
export SSHPASS="$TUNL_VPS_PASSWORD"

# The public key is embedded, single-quoted, in the remote command. A pubkey
# contains no single quotes, so this is safe. grep -qxF makes the append
# idempotent. $HOME is escaped to expand on the remote side.
remote_install="umask 077; mkdir -p \"\$HOME/.ssh\"; touch \"\$HOME/.ssh/authorized_keys\"; grep -qxF '$pub' \"\$HOME/.ssh/authorized_keys\" || printf '%s\\n' '$pub' >> \"\$HOME/.ssh/authorized_keys\""

sshpass -e ssh "${TUNL_SSH_OPTS[@]}" -p "$port" -o PubkeyAuthentication=no \
	"$user@$host" "$remote_install"

# 3. Verify key-only login. BatchMode=yes makes a failed key auth error out
#    instead of falling back to a password prompt.
log_info "verifying key-only login"
if ssh "${TUNL_SSH_OPTS[@]}" -p "$port" -i "$key" \
	-o IdentitiesOnly=yes -o PasswordAuthentication=no -o BatchMode=yes \
	"$user@$host" 'echo tunl-key-ok' | grep -q '^tunl-key-ok$'; then
	log_info "key-only login works"
else
	die "key-only login failed; the key was not accepted"
fi

# 4. Hardening reminder.
log_warn "==================================================================="
log_warn "Key auth works. HARDEN THE VPS NOW:"
log_warn "  1. On the VPS set 'PasswordAuthentication no' in"
log_warn "     /etc/ssh/sshd_config, then reload sshd."
log_warn "  2. ROTATE the root password: TUNL_VPS_PASSWORD was sent over the"
log_warn "     wire to bootstrap this key and should be considered spent."
log_warn "Until both are done, the box still accepts the old password."
log_warn "==================================================================="
