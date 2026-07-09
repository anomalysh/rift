#!/usr/bin/env bash
# tools/publish-images.sh — build (and optionally push) the three rift container
# images without CI, for someone who has a laptop and a registry but no Actions.
#
# This mirrors what .github/workflows/publish.yml does: same images, same OCI
# labels, same Caddy plugin set. It deliberately does NOT run `docker login` for
# you — authenticate yourself first (e.g. `docker login ghcr.io`) — and it
# refuses to push unless you pass --push explicitly, so a stray run can never
# publish an image.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

DEFAULT_REGISTRY="ghcr.io/anomalysh"
DEFAULT_TAG="local"
# The default source URL and license baked into the images when not overridden.
IMAGE_SOURCE_URL="https://github.com/anomalysh/rift"
# Proprietary / all rights reserved (package.json is UNLICENSED). NOASSERTION is
# the SPDX value for "no open-source license asserted."
IMAGE_LICENSES="NOASSERTION"

# Compile exactly the DNS-01 providers that have a snippet under
# deploy/caddy/dns/ (matches the publish workflow). Keep in sync with that dir.
CADDY_DNS_PLUGINS="github.com/caddy-dns/acmedns github.com/caddy-dns/cloudflare github.com/caddy-dns/digitalocean github.com/caddy-dns/linode github.com/caddy-dns/powerdns github.com/caddy-dns/rfc2136"

# image | dockerfile | description   (context is always the repo root).
IMAGES=(
	"riftd|deploy/Dockerfile|rift self-hosted tunnel server"
	"rift-caddy|deploy/Dockerfile.caddy|Caddy with DNS-01 solver plugins for rift"
	"rift-cli|deploy/Dockerfile.cli|rift CLI tunnel agent"
)

usage() {
	cat <<EOF
Usage: tools/publish-images.sh [options]

Build the rift container images (riftd, rift-caddy, rift-cli) locally and,
only with --push, publish them to a registry. Mirrors the CI publish workflow.

This never runs \`docker login\` for you: authenticate first, then pass --push.
Without --push the images are only built (and loaded, for a single platform).

Options:
  --registry <ref>   Registry/namespace prefix (default: $DEFAULT_REGISTRY).
  --tag <tag>        Image tag (default: $DEFAULT_TAG).
  --platform <list>  buildx --platform value, e.g. linux/amd64,linux/arm64
                     (default: builder's native platform). Multi-platform
                     without --push cannot be loaded into the local docker; the
                     build then only validates.
  --push             Push to the registry. REQUIRED to publish; refuses
                     otherwise. Assumes you have already logged in.
  --dry-run          Print the docker commands without running them.
  -h, --help         Show this help and exit.

Environment:
  Requires docker (with buildx) on PATH.
EOF
}

registry="$DEFAULT_REGISTRY"
tag="$DEFAULT_TAG"
platform=""
push=false
dry_run=false

while [ "$#" -gt 0 ]; do
	case "$1" in
	--registry)
		shift
		[ "$#" -gt 0 ] || die "--registry needs a value"
		registry="$1"
		;;
	--registry=*) registry="${1#*=}" ;;
	--tag)
		shift
		[ "$#" -gt 0 ] || die "--tag needs a value"
		tag="$1"
		;;
	--tag=*) tag="${1#*=}" ;;
	--platform)
		shift
		[ "$#" -gt 0 ] || die "--platform needs a value"
		platform="$1"
		;;
	--platform=*) platform="${1#*=}" ;;
	--push) push=true ;;
	--dry-run) dry_run=true ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_cmd docker
docker buildx version >/dev/null 2>&1 || die "docker buildx is required (install the buildx plugin)"

# Trim a trailing slash so "$registry/$image" is always well formed.
registry="${registry%/}"

created="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
revision="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo "")"

log_info "registry: $registry"
log_info "tag:      $tag"
log_info "platform: ${platform:-<native>}"
log_info "push:     $push"

# run CMD...  — execute, or just print under --dry-run.
run() {
	if [ "$dry_run" = true ]; then
		log_info "[dry-run] $*"
	else
		"$@"
	fi
}

# Warn once if a multi-platform build cannot be loaded locally (no --push).
multi_platform_note_shown=false

build_image() {
	local image="$1" dockerfile="$2" description="$3"
	local ref="$registry/$image:$tag"

	local cmd=(docker buildx build
		--file "$REPO_ROOT/$dockerfile"
		--tag "$ref"
		--build-arg "IMAGE_SOURCE=$IMAGE_SOURCE_URL"
		--build-arg "IMAGE_REVISION=$revision"
		--build-arg "IMAGE_VERSION=$tag"
		--build-arg "IMAGE_CREATED=$created"
		--build-arg "IMAGE_LICENSES=$IMAGE_LICENSES"
		--build-arg "IMAGE_TITLE=$image"
		--build-arg "IMAGE_DESCRIPTION=$description")

	[ -n "$platform" ] && cmd+=(--platform "$platform")

	# rift-caddy needs its DNS plugin list; the others take no extra build args.
	if [ "$image" = "rift-caddy" ]; then
		cmd+=(--build-arg "RIFT_CADDY_DNS_PLUGINS=$CADDY_DNS_PLUGINS")
	fi

	if [ "$push" = true ]; then
		cmd+=(--push)
	elif [[ "$platform" == *,* ]]; then
		# buildx cannot --load a multi-platform result into the local docker;
		# build for validation only.
		if [ "$multi_platform_note_shown" = false ]; then
			log_warn "multi-platform build without --push cannot be loaded locally; validating only"
			multi_platform_note_shown=true
		fi
	else
		cmd+=(--load)
	fi

	cmd+=("$REPO_ROOT")

	log_info "building $ref"
	run "${cmd[@]}"
}

if [ "$push" != true ]; then
	log_warn "not pushing (pass --push to publish). Images stay local."
fi

for entry in "${IMAGES[@]}"; do
	IFS='|' read -r image dockerfile description <<<"$entry"
	build_image "$image" "$dockerfile" "$description"
done

log_info "done"
