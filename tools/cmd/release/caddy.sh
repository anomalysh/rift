#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"
# shellcheck source=tools/lib/preflight.sh
. "$SCRIPT_DIR/../../lib/preflight.sh"

DEFAULT_PLUGINS="github.com/caddy-dns/rfc2136 github.com/caddy-dns/acmedns"
DEFAULT_IMAGE="rift-caddy:local"

usage() {
	cat >&2 <<EOF
Usage: rift-ops release caddy [--validate] [--push] [--image NAME] [--dry-run]

Build a Caddy image with the DNS-01 solver plugins listed in
RIFT_CADDY_DNS_PLUGINS, read from your untracked .env (or the environment).

Only RIFT_TLS_MODE=dns01 needs this image. The http01, self and internal modes
run on stock Caddy, which is why the default RIFT_CADDY_IMAGE is caddy:2-alpine.

Compiling Caddy wants more memory than a small VPS has, so the build runs here
and --push loads the finished image onto the remote host over SSH.

Options:
  --validate   After building, parse every deploy/caddy/dns/*.caddy snippet
               against the built binary. Catches a provider whose plugin is
               missing, or whose snippet syntax is wrong, before you deploy.
  --push       docker save | ssh docker load, onto RIFT_VPS_HOST.
  --image NAME Tag to build (default: \$RIFT_CADDY_IMAGE, else $DEFAULT_IMAGE).
  --dry-run    Print what would happen.

Environment (all optional; .env is sourced when present):
  RIFT_CADDY_DNS_PLUGINS  space-separated Go module paths
                          (default: $DEFAULT_PLUGINS)
  RIFT_CADDY_VERSION      Caddy major version or tag (default: 2)
  RIFT_CADDY_IMAGE        image tag to produce (default: $DEFAULT_IMAGE)
  RIFT_VPS_HOST/USER/PORT required only for --push
EOF
}

validate=false
push=false
dry_run=false
image=""

while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--validate) validate=true ;;
	--push) push=true ;;
	--dry-run) dry_run=true ;;
	--image)
		shift
		[ "$#" -gt 0 ] || die "--image needs a value"
		image="$1"
		;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

# Load the operator's untracked .env (honors RIFT_ENV_FILE) if present.
load_env

require_docker

# Compiling Caddy with plugins is a Go build: it links a ~50 MB binary and
# wants roughly 2 GiB while doing it. Out of memory, the linker is OOM-killed
# and Docker leaves a corrupt layer in the build cache; out of disk, the layer
# is truncated. Both surface as unrelated errors thousands of lines later.
readonly CADDY_BUILD_MIN_MEM_MB=2048
readonly CADDY_BUILD_MIN_DISK_MB=4096
readonly CADDY_BUILD_MIN_CPUS=2

if [ "$dry_run" != true ]; then
	preflight_report "$(preflight_docker_root)"
	require_memory "$CADDY_BUILD_MIN_MEM_MB" "compiling Caddy"
	require_docker_disk "$CADDY_BUILD_MIN_DISK_MB" "the Caddy image build"
	require_cpus "$CADDY_BUILD_MIN_CPUS" "compiling Caddy"
fi

plugins="${RIFT_CADDY_DNS_PLUGINS:-$DEFAULT_PLUGINS}"
caddy_version="${RIFT_CADDY_VERSION:-2}"
image="${image:-${RIFT_CADDY_IMAGE:-$DEFAULT_IMAGE}}"

log_info "image:   $image"
log_info "caddy:   $caddy_version"
log_info "plugins: $plugins"

build_cmd=(docker build
	--file "$RIFT_REPO_ROOT/deploy/Dockerfile.caddy"
	--build-arg "CADDY_VERSION=$caddy_version"
	--build-arg "RIFT_CADDY_DNS_PLUGINS=$plugins"
	--tag "$image"
	"$RIFT_REPO_ROOT")

if [ "$dry_run" = true ]; then
	log_info "[dry-run] ${build_cmd[*]}"
else
	"${build_cmd[@]}"
	log_info "built $image"
	log_info "compiled DNS providers:"
	docker run --rm "$image" caddy list-modules | grep '^dns.providers\.' | sed 's/^/  /' >&2
fi

# Parse each provider snippet against the real binary. A snippet whose plugin is
# absent, or whose keys the plugin does not recognise, fails here rather than at
# the first certificate renewal in production.
if [ "$validate" = true ]; then
	if [ "$dry_run" = true ]; then
		log_info "[dry-run] would validate deploy/caddy/dns/*.caddy"
	else
		failed=""
		# Some providers validate credential *shape* when the module is
		# provisioned (Cloudflare rejects a token that is not 40 characters),
		# so the placeholders below have to look plausible or validation fails
		# for the wrong reason.
		dummy_token="0123456789abcdef0123456789abcdef01234567"
		for snippet in "$RIFT_REPO_ROOT"/deploy/caddy/dns/*.caddy; do
			provider="$(basename "$snippet" .caddy)"
			if docker run --rm \
				-e RIFT_ACME_EMAIL=validate@example.test \
				-e RIFT_BASE_DOMAIN=rift.example.test \
				-e RIFT_GATEWAY_HOSTNAME=gateway.rift.example.test \
				-e RIFT_TLS_MODE=dns01 \
				-e RIFT_ACME_DNS_PROVIDER="$provider" \
				-e RIFT_DNS_API_TOKEN="$dummy_token" \
				-e RIFT_DNS_TSIG_KEY_NAME=rift. \
				-e RIFT_DNS_TSIG_KEY_ALG=hmac-sha256 \
				-e RIFT_DNS_TSIG_KEY=dmFsaWRhdGU= \
				-e RIFT_DNS_SERVER=127.0.0.1:53 \
				-e RIFT_ACMEDNS_USERNAME=u -e RIFT_ACMEDNS_PASSWORD=p \
				-e RIFT_ACMEDNS_SUBDOMAIN=s -e RIFT_ACMEDNS_SERVER_URL=http://127.0.0.1 \
				-e RIFT_PDNS_SERVER_URL=http://127.0.0.1:8081 \
				-e RIFT_PDNS_API_TOKEN=t -e RIFT_PDNS_SERVER_ID=localhost \
				-e RIFT_LINODE_API_VERSION=v4 \
				-v "$RIFT_REPO_ROOT/deploy/caddy:/etc/caddy:ro" \
				"$image" caddy validate --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
				log_info "  ok       $provider"
			else
				log_warn "  FAILED   $provider"
				failed="$failed $provider"
			fi
		done
		if [ -n "$failed" ]; then
			die "these provider snippets did not parse:$failed (missing plugin, or wrong keys)"
		fi
		log_info "every provider snippet parses"
	fi
fi

if [ "$push" = true ]; then
	require_env RIFT_VPS_HOST
	ssh_wrapper="$RIFT_TOOLS_DIR/cmd/remote/ssh.sh"
	if [ "$dry_run" = true ]; then
		log_info "[dry-run] docker save $image | $ssh_wrapper 'docker load'"
	else
		log_info "shipping $image to $RIFT_VPS_HOST (this transfers the whole image)"
		docker save "$image" | "$ssh_wrapper" "docker load"
		log_info "loaded $image on $RIFT_VPS_HOST"
		log_warn "set RIFT_CADDY_IMAGE=$image in the remote .env, then redeploy"
	fi
fi

log_info "done"
