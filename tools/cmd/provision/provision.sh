#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"
PROVIDERS_DIR="$RIFT_TOOLS_DIR/providers"

# provision.sh -- create a cloud VPS for rift and get it ready for hand-off.
#
# Scope is deliberately narrow: create the instance, install the deploy key,
# wait until SSH answers, and STOP. Everything after is a separate, idempotent
# step:
#   * DNS is NOT touched -- the operator self-hosts theirs.
#   * Firewall + SSH lockdown belong to tools/harden.sh.
#   * Deploying the stack belongs to tools/remote-deploy.sh.
# The last thing this script prints is the exact next commands to run.
#
# All cloud specifics live in tools/providers/<name>.sh (see that README). This
# script never names a provider's API; it only calls the contract functions.

DEFAULT_PROVIDER="linode"
DEFAULT_REGION="ap-northeast"
# g6-standard-1 is 2 GB. A 1 GB instance leaves under ~600 MB free once Postgres,
# riftd and Caddy are all running, and the OOM killer eventually reaps riftd. The
# extra GB is the difference between "occasionally dies at 3am" and "stable".
DEFAULT_TYPE="g6-standard-1"
# Matches the production host's OS (Debian 13 "trixie").
DEFAULT_IMAGE="linode/debian13"

DEFAULT_STATUS_TIMEOUT=300
DEFAULT_SSH_TIMEOUT=180
DEFAULT_POLL_INTERVAL=5
DEFAULT_SSH_PORT=22

usage() {
	cat >&2 <<EOF
Usage: rift-ops provision create [options]
       rift-ops provision create --list
       rift-ops provision create --destroy <id>

Create a VPS and wait until SSH answers, then hand off to tools/harden.sh and
tools/remote-deploy.sh. Creates the instance and installs the deploy key only;
it does not touch DNS, the firewall, or deploy anything.

Options:
  --provider NAME     Cloud provider (default: \$RIFT_PROVIDER, else $DEFAULT_PROVIDER).
                      One file per provider under tools/providers/.
  --name NAME         Instance label       (default: rift-<timestamp>).
  --region REGION     Region               (default: $DEFAULT_REGION).
  --type TYPE         Instance type/plan   (default: $DEFAULT_TYPE, 2 GB).
  --image IMAGE       OS image             (default: $DEFAULT_IMAGE).
  --key PATH          Public key to install as the deploy key
                      (default: tools/.ssh/id_ed25519.pub).
  --api-base URL      Provider API base URL. Lets the e2e point at a mock.
  --state-file PATH   Where to record the created instance
                      (default: .rift/state.json).
  --status-timeout N  Seconds to wait for the instance to reach 'running'
                      (default: $DEFAULT_STATUS_TIMEOUT).
  --ssh-timeout N     Seconds to wait for SSH to answer (default: $DEFAULT_SSH_TIMEOUT).
  --poll-interval N   Seconds between status polls (default: $DEFAULT_POLL_INTERVAL).
  --ssh-port PORT     TCP port to probe for SSH (default: $DEFAULT_SSH_PORT). Real
                      provisioning uses 22; the e2e overrides it to reach a
                      containerised sshd.
  --dry-run           Print every API call that would be made and create nothing.
  --list              List this provider's instances and exit.
  --destroy <id>      Destroy an instance (idempotent) and exit.
  -h, --help          This help.

Environment (all optional; the untracked .env is read for defaults):
  RIFT_PROVIDER, RIFT_LINODE_TOKEN, RIFT_PROVIDER_API_BASE, RIFT_REGION,
  RIFT_TYPE, RIFT_IMAGE, RIFT_INSTANCE_NAME, RIFT_STATE_FILE.
  RIFT_ENV_FILE overrides which .env is read (default: <repo>/.env).

State file (schema is stable; the pipeline reads it):
  { "provider", "instance_id", "name", "ipv4", "ipv6", "created_at" }
EOF
}

# --- argument parsing -------------------------------------------------------
opt_provider=""
opt_name=""
opt_region=""
opt_type=""
opt_image=""
opt_key=""
opt_api_base=""
opt_state_file=""
opt_status_timeout=""
opt_ssh_timeout=""
opt_poll_interval=""
opt_ssh_port=""
dry_run=false
do_list=false
destroy_id=""

need_value() { [ "$1" -gt 1 ] || die "$2 needs a value"; }

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--provider) need_value "$#" "$1" && shift && opt_provider="$1" ;;
	--name) need_value "$#" "$1" && shift && opt_name="$1" ;;
	--region) need_value "$#" "$1" && shift && opt_region="$1" ;;
	--type) need_value "$#" "$1" && shift && opt_type="$1" ;;
	--image) need_value "$#" "$1" && shift && opt_image="$1" ;;
	--key) need_value "$#" "$1" && shift && opt_key="$1" ;;
	--api-base) need_value "$#" "$1" && shift && opt_api_base="$1" ;;
	--state-file) need_value "$#" "$1" && shift && opt_state_file="$1" ;;
	--status-timeout) need_value "$#" "$1" && shift && opt_status_timeout="$1" ;;
	--ssh-timeout) need_value "$#" "$1" && shift && opt_ssh_timeout="$1" ;;
	--poll-interval) need_value "$#" "$1" && shift && opt_poll_interval="$1" ;;
	--ssh-port) need_value "$#" "$1" && shift && opt_ssh_port="$1" ;;
	--dry-run) dry_run=true ;;
	--list) do_list=true ;;
	--destroy) need_value "$#" "$1" && shift && destroy_id="$1" ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

# --- defaults from .env, without clobbering the caller's environment --------
# Capture any RIFT_* the caller already exported BEFORE sourcing .env, then give
# those precedence: a blank or stale value in .env must never wipe a token (or
# api-base) the operator -- or the e2e -- put in the environment on purpose.
_env_provider="${RIFT_PROVIDER:-}"
_env_token="${RIFT_LINODE_TOKEN:-}"
_env_api_base="${RIFT_PROVIDER_API_BASE:-}"
_env_region="${RIFT_REGION:-}"
_env_type="${RIFT_TYPE:-}"
_env_image="${RIFT_IMAGE:-}"
_env_name="${RIFT_INSTANCE_NAME:-}"
_env_state="${RIFT_STATE_FILE:-}"

# The snapshots above let the caller's env win; load_env then fills in the rest
# from the file (honoring RIFT_ENV_FILE, as this script's --help documents).
load_env

# Precedence for every value: CLI flag > caller env > .env > built-in default.
provider="${opt_provider:-${_env_provider:-${RIFT_PROVIDER:-$DEFAULT_PROVIDER}}}"
region="${opt_region:-${_env_region:-${RIFT_REGION:-$DEFAULT_REGION}}}"
type="${opt_type:-${_env_type:-${RIFT_TYPE:-$DEFAULT_TYPE}}}"
image="${opt_image:-${_env_image:-${RIFT_IMAGE:-$DEFAULT_IMAGE}}}"
api_base="${opt_api_base:-${_env_api_base:-${RIFT_PROVIDER_API_BASE:-}}}"
state_file="${opt_state_file:-${_env_state:-${RIFT_STATE_FILE:-$RIFT_REPO_ROOT/.rift/state.json}}}"
name="${opt_name:-${_env_name:-${RIFT_INSTANCE_NAME:-rift-$(date +%Y%m%d-%H%M%S)}}}"
status_timeout="${opt_status_timeout:-$DEFAULT_STATUS_TIMEOUT}"
ssh_timeout="${opt_ssh_timeout:-$DEFAULT_SSH_TIMEOUT}"
poll_interval="${opt_poll_interval:-$DEFAULT_POLL_INTERVAL}"
ssh_port="${opt_ssh_port:-$DEFAULT_SSH_PORT}"

# Hand the resolved values the providers read to the environment.
export RIFT_LINODE_TOKEN="${_env_token:-${RIFT_LINODE_TOKEN:-}}"
[ -n "$api_base" ] && export RIFT_PROVIDER_API_BASE="$api_base"
[ "$dry_run" = true ] && export RIFT_PROVIDER_DRY_RUN=1

# --- provider loading -------------------------------------------------------
available_providers() {
	local f
	for f in "$PROVIDERS_DIR"/*.sh; do
		[ -e "$f" ] || continue
		basename "$f" .sh
	done
}

load_provider() {
	local nm="$1" file="$PROVIDERS_DIR/$1.sh" list
	if [ ! -f "$file" ]; then
		list="$(available_providers | paste -sd, - 2>/dev/null || true)"
		die "unknown provider: $nm (available: ${list:-none})"
	fi
	# path is validated above
	# shellcheck disable=SC1090
	. "$file"
}

# --- helpers ----------------------------------------------------------------
resolve_key() {
	local k="$opt_key"
	[ -n "$k" ] || k="$(rift_ssh_key_path).pub"
	if [ ! -f "$k" ]; then
		die "no public key at $k -- generate the deploy key first:
    ssh-keygen -t ed25519 -N '' -f $(rift_ssh_key_path)"
	fi
	printf '%s' "$k"
}

wait_status_running() {
	local id="$1" status deadline
	deadline=$(($(date +%s) + status_timeout))
	while :; do
		status="$(provider_status "$id")"
		log_info "instance $id status: ${status:-unknown} (waiting for 'running')"
		[ "$status" = "running" ] && return 0
		if [ "$(date +%s)" -ge "$deadline" ]; then
			# A timeout must destroy NOTHING. The operator is paying for this
			# instance and can inspect or reuse it; deleting a box that was
			# merely slow to boot is worse than leaving one they can see. Tell
			# them the id and how to remove it deliberately.
			die "timed out after ${status_timeout}s waiting for instance $id to reach 'running'.
    Left running on purpose. Inspect it, or destroy it deliberately with:
      $0 --provider $provider --destroy $id"
		fi
		sleep "$poll_interval"
	done
}

wait_for_ssh() {
	local host="$1" port="$2" id="$3" deadline
	deadline=$(($(date +%s) + ssh_timeout))
	log_info "waiting for SSH on $host:$port"
	while :; do
		if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then
			exec 3<&- 3>&-
			log_info "SSH is answering on $host:$port"
			return 0
		fi
		if [ "$(date +%s)" -ge "$deadline" ]; then
			die "timed out after ${ssh_timeout}s waiting for SSH on $host:$port.
    Instance $id is left running. Inspect it, or destroy it with:
      $0 --provider $provider --destroy $id"
		fi
		sleep 2
	done
}

# Valid-JSON state file. Values travel via the environment (consistent with the
# no-secrets-in-argv rule), and created_at is stamped in UTC.
_STATE_PY='
import os, json, datetime
print(json.dumps({
    "provider": os.environ.get("RIFT_STATE_PROVIDER", ""),
    "instance_id": os.environ.get("RIFT_STATE_ID", ""),
    "name": os.environ.get("RIFT_STATE_NAME", ""),
    "ipv4": os.environ.get("RIFT_STATE_IPV4", ""),
    "ipv6": os.environ.get("RIFT_STATE_IPV6", ""),
    "created_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
}, indent=2))
'

write_state() {
	local file="$1" prov="$2" id="$3" nm="$4" ipv4="$5" ipv6="$6"
	mkdir -p "$(dirname "$file")"
	RIFT_STATE_PROVIDER="$prov" RIFT_STATE_ID="$id" RIFT_STATE_NAME="$nm" \
		RIFT_STATE_IPV4="$ipv4" RIFT_STATE_IPV6="$ipv6" \
		python3 -c "$_STATE_PY" >"$file"
}

print_next_steps() {
	local ipv4="$1"
	cat >&2 <<EOF

Instance is reachable. Provisioning stops here by design.

Next steps (each is separate and idempotent):
  1. Harden the box (firewall + SSH lockdown):
       RIFT_VPS_HOST=$ipv4 tools/harden.sh
  2. Deploy the rift stack:
       RIFT_VPS_HOST=$ipv4 tools/remote-deploy.sh

State written to: $state_file
EOF
}

# --- dispatch ---------------------------------------------------------------
load_provider "$provider"

if [ "$do_list" = true ]; then
	[ "$dry_run" = true ] || provider_require_env
	log_info "instances for provider '$provider':"
	printf 'ID\tLABEL\tSTATUS\tIPV4\n' >&2
	provider_list
	exit 0
fi

if [ -n "$destroy_id" ]; then
	[ "$dry_run" = true ] || provider_require_env
	log_info "destroying instance $destroy_id via '$provider'"
	provider_destroy "$destroy_id"
	log_info "destroyed (or already gone): $destroy_id"
	exit 0
fi

# --- create -----------------------------------------------------------------
key_path="$(resolve_key)"
pubkey="$(cat "$key_path")"

[ "$dry_run" = true ] || provider_require_env

log_info "provider: $provider"
log_info "creating instance: name=$name region=$region type=$type image=$image"
log_info "deploy key: $key_path"

id="$(provider_create "$name" "$region" "$type" "$image" "$pubkey")"
[ -n "$id" ] || die "provider returned no instance id"
log_info "instance id: $id"

wait_status_running "$id"

ipv4="$(provider_ipv4 "$id")"
ipv6="$(provider_ipv6 "$id")"
log_info "ipv4: ${ipv4:-<none>}  ipv6: ${ipv6:-<none>}"

if [ "$dry_run" = true ]; then
	log_info "[dry-run] would wait for TCP $ssh_port on ${ipv4:-<ip>}"
	log_info "[dry-run] would write state to $state_file"
	log_info "[dry-run] complete; nothing was created"
	exit 0
fi

[ -n "$ipv4" ] || die "no public IPv4 returned; cannot reach the instance"
wait_for_ssh "$ipv4" "$ssh_port" "$id"

write_state "$state_file" "$provider" "$id" "$name" "$ipv4" "$ipv6"
log_info "wrote state: $state_file"

print_next_steps "$ipv4"
log_info "provisioning complete"
