#!/usr/bin/env bash
# rift-ops release docker — build the multi-arch CLI release inside a pinned Bun
# container and extract the artifacts to dist/release/<version>/.
#
# Reproducible: the toolchain is fixed by deploy/Dockerfile.release, so the
# result does not depend on the host's Bun (or its free disk). Uses buildx local
# output, so a working buildx builder is required (see `docker buildx ls`).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

DOCKERFILE="$RIFT_REPO_ROOT/deploy/Dockerfile.release"
OUT="$RIFT_REPO_ROOT/dist/release"

usage() {
	cat <<'EOF'
Usage: rift-ops release docker [options]

Cross-compile the rift CLI for every target inside a pinned Bun container and
extract the artifacts to dist/release/<version>/. The toolchain is fixed by
deploy/Dockerfile.release, so the build does not depend on the host's Bun.

Options:
  --bun <version>   Override the Bun image tag (default: the Dockerfile's).
  -h, --help        Show this help and exit.

Requires docker with a working buildx builder (docker buildx ls).
EOF
}

main() {
	local bun=""
	while [ $# -gt 0 ]; do
		case "$1" in
		--bun)
			[ $# -ge 2 ] || die "--bun requires a value"
			bun="$2"
			shift 2
			;;
		--bun=*)
			bun="${1#*=}"
			shift
			;;
		-h | --help)
			usage
			return 0
			;;
		*)
			die "unknown argument: $1 (see --help)"
			;;
		esac
	done

	require_cmd docker
	[ -f "$DOCKERFILE" ] || die "missing $DOCKERFILE"
	docker buildx version >/dev/null 2>&1 ||
		die "docker buildx is unavailable; install/enable it (docker buildx ls)"

	local args=(
		build
		--file "$DOCKERFILE"
		--target artifacts
		--output "type=local,dest=$OUT"
	)
	[ -n "$bun" ] && args+=(--build-arg "BUN_VERSION=$bun")
	args+=("$RIFT_REPO_ROOT")

	log_info "building rift release in a pinned Bun container (reproducible)"
	docker buildx "${args[@]}"

	log_info "release artifacts extracted to dist/release/"
	find "$OUT" -maxdepth 2 -type f -name 'rift-*' ! -name '*.tar.gz' ! -name '*.zip' |
		sort | sed 's#^#  built: #' >&2
}

main "$@"
