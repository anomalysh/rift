#!/bin/sh
# tools/install.sh — POSIX installer for the rift CLI (curl | sh friendly).
#
# Detects OS/arch/libc, downloads the matching release binary from GitHub,
# VERIFIES its SHA256 against the published SHA256SUMS, and installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/anomalysh/rift/master/tools/install.sh | sh
#
# Overridable via environment:
#   RIFT_INSTALL_REPO      GitHub owner/repo            (default anomalysh/rift)
#   RIFT_INSTALL_BASE_URL  release download base URL    (default GitHub releases)
#   RIFT_INSTALL_VERSION   version to install           (default latest release)
#   RIFT_INSTALL_DIR       install directory            (default /usr/local/bin
#                                                        or ~/.local/bin)
#
# Written for POSIX sh: no arrays, no bashisms, no `local`.
set -eu

REPO="${RIFT_INSTALL_REPO:-anomalysh/rift}"
BASE_URL="${RIFT_INSTALL_BASE_URL:-https://github.com/${REPO}/releases/download}"
VERSION="${RIFT_INSTALL_VERSION:-}"
INSTALL_DIR="${RIFT_INSTALL_DIR:-}"
DRY_RUN=0

log() { printf 'rift-install: %s\n' "$*" >&2; }
warn() { printf 'rift-install: warning: %s\n' "$*" >&2; }
err() {
	printf 'rift-install: error: %s\n' "$*" >&2
	exit 1
}
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
	cat <<'EOF'
Usage: install.sh [options]

Download and install the rift CLI, verifying its checksum first.

Options:
  --version <v>   Install a specific version (default: latest release).
  --dir <path>    Install into <path> instead of the default bin directory.
  --dry-run       Print what would happen without downloading or installing.
  -h, --help      Show this help and exit.

Environment:
  RIFT_INSTALL_REPO, RIFT_INSTALL_BASE_URL, RIFT_INSTALL_VERSION, RIFT_INSTALL_DIR
EOF
}

http_to_file() {
	# http_to_file URL OUTFILE
	if have curl; then
		curl -fsSL "$1" -o "$2"
	elif have wget; then
		wget -q -O "$2" "$1"
	else
		err "need curl or wget to download files"
	fi
}

http_to_stdout() {
	# http_to_stdout URL
	if have curl; then
		curl -fsSL "$1"
	elif have wget; then
		wget -q -O - "$1"
	else
		err "need curl or wget to download files"
	fi
}

sha256_of() {
	# sha256_of FILE — print the lowercase hex digest, or fail if no tool.
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		return 1
	fi
}

# ---- argument parsing ------------------------------------------------------
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--version)
		[ $# -ge 2 ] || err "--version requires a value"
		VERSION="$2"
		shift 2
		;;
	--version=*)
		VERSION="${1#*=}"
		shift
		;;
	--dir)
		[ $# -ge 2 ] || err "--dir requires a value"
		INSTALL_DIR="$2"
		shift 2
		;;
	--dir=*)
		INSTALL_DIR="${1#*=}"
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	*)
		err "unknown argument: $1 (see --help)"
		;;
	esac
done

# ---- platform detection ----------------------------------------------------
os="$(uname -s)"
case "$os" in
Linux) os="linux" ;;
Darwin) os="darwin" ;;
*) err "unsupported OS: ${os} (rift ships linux and darwin builds; on Windows download rift-windows-x64.exe from the releases page)" ;;
esac

arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) arch="x64" ;;
aarch64 | arm64) arch="arm64" ;;
*) err "unsupported architecture: ${arch}" ;;
esac

libc="glibc"
if [ "$os" = "linux" ]; then
	# musl vs glibc selection. `ldd --version` is authoritative; only fall back
	# to the presence of the musl dynamic loader when ldd tells us nothing.
	# A glibc host can ALSO ship a musl loader (multiarch/compat), so choosing
	# musl purely from the loader's presence would hand a glibc machine a
	# musl-linked binary that crashes against the wrong C library.
	ldd_out="$(ldd --version 2>&1 || true)"
	if printf '%s' "$ldd_out" | grep -qi musl; then
		libc="musl"
	elif printf '%s' "$ldd_out" | grep -qiE 'gnu|glibc'; then
		libc="glibc"
	elif ls /lib/ld-musl-* >/dev/null 2>&1; then
		libc="musl"
	fi
fi

if [ "$os" = "linux" ] && [ "$libc" = "musl" ]; then
	artifact="rift-linux-${arch}-musl"
elif [ "$os" = "linux" ]; then
	artifact="rift-linux-${arch}"
else
	artifact="rift-darwin-${arch}"
fi

# ---- version resolution ----------------------------------------------------
if [ -z "$VERSION" ]; then
	api="https://api.github.com/repos/${REPO}/releases/latest"
	# Swallow curl's own 404 line; a missing "latest" release is an expected
	# state (the repo simply has no published release yet), not a transport
	# error worth showing the user.
	VERSION="$(http_to_stdout "$api" 2>/dev/null | grep '"tag_name"' | head -n1 | cut -d'"' -f4 || true)"
	if [ -z "$VERSION" ]; then
		err "no published release found for ${REPO}. Either the project has not cut a release yet, or the API is unreachable. Install a specific version with: RIFT_INSTALL_VERSION=0.1.0 (or pass --version)."
	fi
fi
VERSION="${VERSION#v}" # accept both 0.1.0 and v0.1.0
TAG="v${VERSION}"

bin_url="${BASE_URL}/${TAG}/${artifact}"
sums_url="${BASE_URL}/${TAG}/SHA256SUMS"

# ---- install directory -----------------------------------------------------
if [ -n "$INSTALL_DIR" ]; then
	dest_dir="$INSTALL_DIR"
elif [ -w /usr/local/bin ]; then
	dest_dir="/usr/local/bin"
else
	dest_dir="${HOME}/.local/bin"
fi
target="${dest_dir}/rift"

if [ "$DRY_RUN" -eq 1 ]; then
	log "dry run — no changes will be made"
	log "  repo:      ${REPO}"
	log "  platform:  ${os}/${arch}$([ "$os" = linux ] && printf ' (%s)' "$libc")"
	log "  version:   ${VERSION} (tag ${TAG})"
	log "  artifact:  ${artifact}"
	log "  binary:    ${bin_url}"
	log "  checksums: ${sums_url}"
	log "  install:   ${target}"
	exit 0
fi

# ---- download --------------------------------------------------------------
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t rift-install)"
trap 'rm -rf "$tmp"' EXIT INT TERM

log "downloading ${artifact} (${TAG})"
http_to_file "$bin_url" "${tmp}/${artifact}" || err "download failed: ${bin_url}"
http_to_file "$sums_url" "${tmp}/SHA256SUMS" || err "download failed: ${sums_url}"

# ---- checksum verification (before anything touches PATH) ------------------
# Refuse to install a binary whose SHA256 does not match the published
# SHA256SUMS. Piping an unverified binary into a bin directory is a
# supply-chain hole: a compromised mirror, a stale CDN, or a MITM could serve a
# trojaned rift. This check is the trust anchor of the installer, so on any
# mismatch (or a missing checksum entry) we abort rather than install.
expected="$(awk -v f="$artifact" '{ n=$2; sub(/^\*/, "", n); if (n==f) print $1 }' "${tmp}/SHA256SUMS")"
[ -n "$expected" ] || err "no checksum for ${artifact} in SHA256SUMS; refusing to install"
actual="$(sha256_of "${tmp}/${artifact}")" || err "need sha256sum or shasum to verify the download"
[ -n "$actual" ] || err "could not compute the download's sha256"
if [ "$expected" != "$actual" ]; then
	err "checksum mismatch for ${artifact}:
    expected ${expected}
    actual   ${actual}
  refusing to install a binary that does not match SHA256SUMS"
fi
log "checksum verified (${actual})"

chmod 0755 "${tmp}/${artifact}"

# ---- install ---------------------------------------------------------------
mkdir -p "$dest_dir" 2>/dev/null || true
if [ -d "$dest_dir" ] && [ -w "$dest_dir" ]; then
	# Stage next to the target then rename, so an in-use rift is replaced
	# atomically rather than truncated mid-copy.
	staged="${dest_dir}/.rift.install.$$"
	cp "${tmp}/${artifact}" "$staged"
	chmod 0755 "$staged"
	mv -f "$staged" "$target"
else
	# We never run sudo on the user's behalf. Persist the verified binary
	# outside the temp dir (so the trap does not delete it) and print the
	# exact privileged command for the user to run themselves.
	persist="${TMPDIR:-/tmp}/rift"
	cp "${tmp}/${artifact}" "$persist"
	chmod 0755 "$persist"
	warn "${dest_dir} is not writable by this user; not using sudo automatically."
	warn "the verified binary is at ${persist}. To finish, run:"
	printf '\n    sudo install -m 0755 %s %s\n\n' "$persist" "$target" >&2
	warn "or re-run with a writable directory, e.g. --dir \"\$HOME/.local/bin\""
	exit 1
fi

log "installed rift ${VERSION} to ${target}"

# Confirm the freshly installed binary actually runs on this host.
if installed_version="$("$target" --version 2>/dev/null)"; then
	log "verified: rift ${installed_version}"
fi

# Warn if the install dir is not on PATH, so `rift` resolves after install.
case ":${PATH}:" in
*:"${dest_dir}":*) : ;;
*) warn "${dest_dir} is not on your PATH; add it, e.g.: export PATH=\"${dest_dir}:\$PATH\"" ;;
esac

log "run 'rift --help' to get started"
