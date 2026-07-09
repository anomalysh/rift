#!/usr/bin/env bash
# tools/release.sh — cross-compile rift release artifacts for every supported
# platform, checksum them, and package tarballs/zip for distribution.
#
# Bun compiles to fixed targets, so a release is N cross-compiles rather than
# one build. Output lands in dist/release/<version>/ containing both the raw
# binaries (fetched directly by install.sh) and the packaged archives.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

CLI_DIR="$REPO_DIR/cli"
PKG_DIR="$REPO_DIR/packaging"
MAN_PAGE="$PKG_DIR/man/rift.1"
COMPLETIONS_DIR="$PKG_DIR/completions"
ENTRY="$CLI_DIR/src/index.ts"

# Bun cross-compile targets and the artifact base name each produces. Verified
# against `bun build --compile` on Bun 1.3.12; an unsupported target is skipped
# (with a warning) rather than failing the whole release.
TARGETS=(
	"bun-linux-x64:rift-linux-x64"
	"bun-linux-arm64:rift-linux-arm64"
	"bun-linux-x64-musl:rift-linux-x64-musl"
	"bun-linux-arm64-musl:rift-linux-arm64-musl"
	"bun-darwin-x64:rift-darwin-x64"
	"bun-darwin-arm64:rift-darwin-arm64"
	"bun-windows-x64:rift-windows-x64.exe"
)

usage() {
	cat <<'EOF'
Usage: tools/release.sh [options]

Cross-compile rift release artifacts, checksum them, and package archives into
dist/release/<version>/.

Options:
  --version <v>   Override the release version (default: cli/package.json).
  --clean         Remove the version output directory before building.
  -h, --help      Show this help and exit.

Environment:
  Requires bun, sha256sum, tar, and zip on PATH.
EOF
}

pkg_version() {
	# cli/package.json is the single source of truth for the version.
	(cd "$CLI_DIR" && bun --print "require('./package.json').version")
}

main() {
	local version="" clean=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version requires a value"
			version="$2"
			shift 2
			;;
		--version=*)
			version="${1#*=}"
			shift
			;;
		--clean)
			clean=1
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

	require_cmd bun sha256sum tar zip
	[ -f "$ENTRY" ] || die "entrypoint not found: $ENTRY"
	[ -f "$MAN_PAGE" ] || die "man page not found: $MAN_PAGE"
	[ -d "$COMPLETIONS_DIR" ] || die "completions dir not found: $COMPLETIONS_DIR"

	[ -n "$version" ] || version="$(pkg_version)"
	[ -n "$version" ] || die "could not determine version"

	local out="$REPO_DIR/dist/release/$version"
	if [ "$clean" -eq 1 ] && [ -d "$out" ]; then
		log_info "cleaning $out"
		rm -rf "$out"
	fi
	mkdir -p "$out"

	log_info "building rift $version -> $out"

	local built=() skipped=()
	local entry target artifact
	for entry in "${TARGETS[@]}"; do
		target="${entry%%:*}"
		artifact="${entry#*:}"
		log_info "compiling $artifact ($target)"
		# Match the repo build flags (minify + sourcemap) for parity with
		# `bun run build`. A failed/unsupported target is skipped, not fatal.
		if (cd "$CLI_DIR" && bun build --compile --minify --sourcemap \
			--target="$target" "$ENTRY" --outfile "$out/$artifact") >/dev/null 2>"$out/.build.log"; then
			chmod +x "$out/$artifact" 2>/dev/null || true
			built+=("$artifact")
		else
			log_warn "target $target unsupported or failed; skipping $artifact"
			sed 's/^/    bun: /' "$out/.build.log" >&2 || true
			skipped+=("$target")
			rm -f "$out/$artifact"
		fi
	done
	rm -f "$out/.build.log"
	# --sourcemap embeds the map in each compiled binary but also drops a
	# redundant sidecar (named after the entry, e.g. index.js.map) into the
	# output dir; it is not a published artifact, so remove it.
	rm -f "$out"/*.map

	[ "${#built[@]}" -gt 0 ] || die "no targets built successfully"

	package_artifacts "$out" "${built[@]}"
	write_checksums "$out"
	verify_checksums "$out"
	smoke_test "$out"

	print_summary "$out" "$version"
	if [ "${#skipped[@]}" -gt 0 ]; then
		log_warn "skipped targets: ${skipped[*]}"
	fi
	log_info "release artifacts ready in $out"
}

# package_artifacts OUT ARTIFACT...  — build a .tar.gz (unix) or .zip (windows)
# per binary, each containing the binary, the man page, and the completions.
package_artifacts() {
	local out="$1"
	shift
	local artifact base stage binname
	for artifact in "$@"; do
		if [ "${artifact%.exe}" != "$artifact" ]; then
			base="${artifact%.exe}"
			binname="rift.exe"
		else
			base="$artifact"
			binname="rift"
		fi
		stage="$out/.stage/$base"
		rm -rf "$stage"
		mkdir -p "$stage/completions"
		cp "$out/$artifact" "$stage/$binname"
		chmod +x "$stage/$binname" 2>/dev/null || true
		cp "$MAN_PAGE" "$stage/rift.1"
		cp "$COMPLETIONS_DIR"/rift.bash "$COMPLETIONS_DIR"/rift.zsh \
			"$COMPLETIONS_DIR"/rift.fish "$stage/completions/"

		if [ "$binname" = "rift.exe" ]; then
			(cd "$out/.stage" && zip -q -r -X "$out/$base.zip" "$base")
			log_info "packaged $base.zip"
		else
			tar -C "$out/.stage" -czf "$out/$base.tar.gz" "$base"
			log_info "packaged $base.tar.gz"
		fi
	done
	rm -rf "$out/.stage"
}

# write_checksums OUT  — SHA256SUMS over every published file (raw binaries and
# archives). install.sh verifies the raw-binary entries before installing.
write_checksums() {
	local out="$1"
	(
		cd "$out"
		# Deterministic ordering; exclude any pre-existing SHA256SUMS.
		find . -maxdepth 1 -type f ! -name 'SHA256SUMS' -printf '%P\n' |
			sort |
			xargs sha256sum >SHA256SUMS
	)
	log_info "wrote SHA256SUMS ($(wc -l <"$out/SHA256SUMS") entries)"
}

verify_checksums() {
	local out="$1"
	(cd "$out" && sha256sum -c SHA256SUMS >/dev/null) ||
		die "checksum verification failed"
	log_info "sha256sum -c SHA256SUMS: OK"
}

# smoke_test OUT  — if the host-native artifact was built, run it to prove the
# binary is not merely present but actually executes and reports its version.
smoke_test() {
	local out="$1" host
	host="$(host_artifact)"
	if [ -n "$host" ] && [ -x "$out/$host" ]; then
		local v
		v="$("$out/$host" --version)"
		log_info "smoke test: $host --version -> $v"
	else
		log_info "smoke test skipped (no host-native artifact for this platform)"
	fi
}

host_artifact() {
	local os arch
	case "$(uname -s)" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*) return 0 ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch="x64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) return 0 ;;
	esac
	# glibc host only; a musl host would need the -musl artifact, which we do
	# not assume here (this is a best-effort smoke test, not a gate).
	if [ "$os" = "linux" ] && ldd --version 2>&1 | grep -qi musl; then
		printf 'rift-%s-%s-musl' "$os" "$arch"
	else
		printf 'rift-%s-%s' "$os" "$arch"
	fi
}

print_summary() {
	local out="$1" version="$2"
	printf '\n  rift %s — dist/release/%s\n\n' "$version" "$version" >&2
	printf '  %-28s %10s  %s\n' "ARTIFACT" "SIZE" "SHA256" >&2
	printf '  %-28s %10s  %s\n' "--------" "----" "------" >&2
	local name size sum
	while IFS= read -r name; do
		size="$(stat -c '%s' "$out/$name")"
		sum="$(sha256sum "$out/$name" | cut -c1-16)"
		printf '  %-28s %10s  %s…\n' "$name" "$(human_size "$size")" "$sum" >&2
	done < <(find "$out" -maxdepth 1 -type f ! -name 'SHA256SUMS' -printf '%P\n' | sort)
	printf '\n' >&2
}

human_size() {
	local b="$1"
	if [ "$b" -ge 1048576 ]; then
		printf '%d.%dM' $((b / 1048576)) $(((b % 1048576) * 10 / 1048576))
	elif [ "$b" -ge 1024 ]; then
		printf '%dK' $((b / 1024))
	else
		printf '%dB' "$b"
	fi
}

main "$@"
