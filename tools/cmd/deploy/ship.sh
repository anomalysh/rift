#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

# ship.sh -- the deployment pipeline: provision -> harden -> deploy -> verify.
#
# Each stage is its own idempotent script, so a failure is resumable rather
# than a restart: `--from harden` skips provisioning an instance that already
# exists. The stages record their progress in the same state file provision.sh
# writes, so a plain re-run picks up where it stopped.
#
# verify is a real gate, not a victory lap. It asserts TLS actually serves --
# the failure mode that shipped twice before anyone noticed, because a deploy
# that "succeeded" still handed visitors a protocol error. If verify fails, the
# pipeline fails.

STAGES=(provision harden deploy verify)
STATE_FILE_DEFAULT="$RIFT_REPO_ROOT/.rift/state.json"

usage() {
	cat >&2 <<EOF
Usage: rift-ops deploy ship [--from STAGE] [--only STAGE] [--dry-run] [--yes]

Run the deployment pipeline end to end:
  provision  create the VPS and wait for SSH        (tools/provision.sh)
  harden     ssh lockdown, firewall, log rotation    (tools/harden.sh --force)
  deploy     build + start the stack, reload Caddy    (tools/remote-deploy.sh)
  verify     assert TLS serves and the stack is up    (this script)

Stages run in order. Re-running skips stages already recorded complete in the
state file; --from re-runs from a stage onward, --only runs exactly one.

Options:
  --from STAGE   Start at STAGE (one of: ${STAGES[*]}).
  --only STAGE   Run just STAGE.
  --host IP      Skip provision; harden/deploy/verify this existing host.
  --state-file F Default: $STATE_FILE_DEFAULT
  --dry-run      Print what each stage would do; change nothing.
  --yes          Do not prompt before the destructive-ish stages.

Environment: the provider/VPS/TLS variables from your untracked .env, plus
RIFT_VPS_* for ssh. Provisioning needs a provider token; see tools/provision.sh.
EOF
}

from_stage=""
only_stage=""
host=""
state_file="$STATE_FILE_DEFAULT"
dry_run=false
assume_yes=false

is_stage() {
	local s
	for s in "${STAGES[@]}"; do [ "$s" = "$1" ] && return 0; done
	return 1
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--from)
		shift
		from_stage="${1:-}"
		is_stage "$from_stage" || die "--from must be one of: ${STAGES[*]}"
		;;
	--only)
		shift
		only_stage="${1:-}"
		is_stage "$only_stage" || die "--only must be one of: ${STAGES[*]}"
		;;
	--host)
		shift
		host="${1:-}"
		;;
	--state-file)
		shift
		state_file="${1:-}"
		;;
	--dry-run) dry_run=true ;;
	--yes) assume_yes=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

load_env

# state_get KEY -- read a value from the JSON state file, or empty.
state_get() {
	[ -f "$state_file" ] || return 0
	python3 -c "import json,sys
try:
    d=json.load(open('$state_file'))
except Exception:
    sys.exit(0)
print(d.get('$1',''))" 2>/dev/null || true
}

# state_mark_stage STAGE -- record a stage as complete.
state_mark_stage() {
	[ "$dry_run" = true ] && return 0
	mkdir -p "$(dirname "$state_file")"
	python3 -c "import json,os
p='$state_file'
try:
    d=json.load(open(p))
except Exception:
    d={}
d.setdefault('stages',{})['$1']=True
json.dump(d,open(p,'w'),indent=2)"
}

stage_done() {
	[ "$(state_get 'stages' | grep -c "'$1': True" 2>/dev/null || true)" != "0" ] 2>/dev/null || true
	python3 -c "import json,sys
try:
    d=json.load(open('$state_file'))
except Exception:
    sys.exit(1)
sys.exit(0 if d.get('stages',{}).get('$1') else 1)" 2>/dev/null
}

should_run() {
	local stage="$1"
	if [ -n "$only_stage" ]; then
		[ "$stage" = "$only_stage" ]
		return
	fi
	if [ -n "$from_stage" ]; then
		# Run this and every later stage.
		local seen_from=false s
		for s in "${STAGES[@]}"; do
			[ "$s" = "$from_stage" ] && seen_from=true
			[ "$s" = "$stage" ] && { [ "$seen_from" = true ] && return 0 || return 1; }
		done
	fi
	# Default: run unless already recorded complete.
	if stage_done "$stage"; then
		return 1
	fi
	return 0
}

confirm() {
	[ "$assume_yes" = true ] && return 0
	[ "$dry_run" = true ] && return 0
	printf '%s [y/N] ' "$1" >&2
	local ans
	read -r ans
	[ "$ans" = "y" ] || [ "$ans" = "Y" ]
}

run() {
	if [ "$dry_run" = true ]; then
		log_info "[dry-run] $*"
		return 0
	fi
	"$@"
}

# ---------------------------------------------------------------- stages

stage_provision() {
	if [ -n "$host" ]; then
		log_info "provision: skipped, using existing host $host"
		return 0
	fi
	log_info "stage: provision"
	run "$RIFT_TOOLS_DIR/cmd/provision/provision.sh" --state-file "$state_file"
	host="$(state_get ipv4)"
	[ -n "$host" ] || [ "$dry_run" = true ] || die "provision did not record an ipv4 in $state_file"
}

resolve_host() {
	[ -n "$host" ] && return 0
	host="$(state_get ipv4)"
	[ -n "$host" ] || [ -n "${RIFT_VPS_HOST:-}" ] || die "no host: provision first, or pass --host"
	host="${host:-$RIFT_VPS_HOST}"
}

stage_harden() {
	resolve_host
	log_info "stage: harden ($host)"
	confirm "harden $host? this disables password SSH login" || die "aborted"
	# harden.sh runs ON the box. Ship the tools over, then run it there.
	# --force because the real host has no hostcheck marker (that guard exists
	# to stop the script running on a developer laptop). A failed upload must
	# abort: running harden.sh on a stale or partial tools/ could lock SSH.
	run env RIFT_VPS_HOST="$host" "$RIFT_TOOLS_DIR/cmd/remote/scp.sh" -r "$RIFT_TOOLS_DIR" "/opt/rift/tools" ||
		die "failed to ship tools/ to $host; not running harden on a stale copy"
	run env RIFT_VPS_HOST="$host" "$RIFT_TOOLS_DIR/cmd/remote/ssh.sh" \
		"bash /opt/rift/tools/cmd/host/harden.sh --force"
}

stage_deploy() {
	resolve_host
	log_info "stage: deploy ($host)"
	run env RIFT_VPS_HOST="$host" "$RIFT_TOOLS_DIR/cmd/deploy/deploy.sh"
}

stage_verify() {
	resolve_host
	log_info "stage: verify ($host)"
	if [ "$dry_run" = true ]; then
		log_info "[dry-run] would assert TLS serves and the stack is healthy"
		return 0
	fi
	"$RIFT_TOOLS_DIR/cmd/deploy/verify.sh" --host "$host"
}

# ---------------------------------------------------------------- run

log_info "pipeline target state: $state_file"
ran_any=false
for stage in "${STAGES[@]}"; do
	if should_run "$stage"; then
		ran_any=true
		case "$stage" in
		provision) stage_provision ;;
		harden) stage_harden ;;
		deploy) stage_deploy ;;
		verify) stage_verify ;;
		esac
		state_mark_stage "$stage"
	else
		log_info "skipping $stage (already complete; --from $stage to re-run)"
	fi
done

[ "$ran_any" = true ] || log_warn "every stage was already complete; nothing to do"
log_info "ship complete"
