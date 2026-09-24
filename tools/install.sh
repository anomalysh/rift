#!/bin/sh
# tools/install.sh — POSIX installer for the rift CLI (curl | sh friendly).
#
# Detects OS/arch/libc, downloads the matching release binary from GitHub over
# HTTPS, VERIFIES its SHA256 against the published SHA256SUMS, and installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/anomalysh/rift/master/tools/install.sh | sh
#
# Overridable via environment:
#   RIFT_INSTALL_REPO      GitHub owner/repo            (default anomalysh/rift)
#   RIFT_INSTALL_BASE_URL  release download base URL    (default GitHub releases)
#   RIFT_INSTALL_VERSION   version to install           (default latest release)
#   RIFT_INSTALL_DIR       install directory            (default /usr/local/bin
#                                                        or ~/.local/bin)
#   RIFT_INSTALL_ALLOW_HTTP=1  permit a plain-http BASE_URL, and only on
#                          loopback (127.0.0.1 / localhost / [::1]); for the
#                          hermetic installer test, never for real installs
#
# Written for POSIX sh: no arrays, no bashisms, no `local`.
set -eu

REPO="${RIFT_INSTALL_REPO:-anomalysh/rift}"
BASE_URL="${RIFT_INSTALL_BASE_URL:-https://github.com/${REPO}/releases/download}"
VERSION="${RIFT_INSTALL_VERSION:-}"
INSTALL_DIR="${RIFT_INSTALL_DIR:-}"
ALLOW_HTTP="${RIFT_INSTALL_ALLOW_HTTP:-}"
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

# Transport policy: every download is HTTPS with TLS >= 1.2, including across
# redirects, so the binary and SHA256SUMS cannot be swapped by anyone on the
# network path. The single exception is the hermetic installer test, which
# serves a fake release over http on loopback (RIFT_INSTALL_ALLOW_HTTP=1).
insecure_http_ok=0

curl_get() {
	# curl_get URL [curl args...] — fetch honouring the transport policy.
	_url="$1"
	shift
	if [ "$insecure_http_ok" -eq 1 ]; then
		curl -fsSL "$@" "$_url"
	else
		curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 "$@" "$_url"
	fi
}

wget_get() {
	# wget_get URL OUTFILE — fetch honouring the transport policy. BusyBox wget
	# (Alpine) lacks --https-only/--secure-protocol; the URL itself is already
	# checked to be https, so they are added only where supported.
	if [ "$insecure_http_ok" -eq 1 ]; then
		wget -q -O "$2" "$1"
	elif wget --help 2>&1 | grep -q -- '--https-only'; then
		wget -q --https-only --secure-protocol=TLSv1_2 -O "$2" "$1"
	else
		wget -q -O "$2" "$1"
	fi
}

http_to_file() {
	# http_to_file URL OUTFILE
	if have curl; then
		curl_get "$1" -o "$2"
	elif have wget; then
		wget_get "$1" "$2"
	else
		err "need curl or wget to download files"
	fi
}

http_to_stdout() {
	# http_to_stdout URL
	if have curl; then
		curl_get "$1"
	elif have wget; then
		wget_get "$1" -
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

# ---- source validation -----------------------------------------------------
# REPO is spliced into URLs, so it must be a plain owner/name.
case "$REPO" in
*/*/* | /* | */ | *[!A-Za-z0-9._/-]*)
	err "invalid RIFT_INSTALL_REPO '${REPO}': expected owner/name"
	;;
*/*) : ;;
*) err "invalid RIFT_INSTALL_REPO '${REPO}': expected owner/name" ;;
esac

case "$BASE_URL" in
https://*) : ;;
http://127.0.0.1:* | http://127.0.0.1/* | http://localhost:* | http://localhost/* | http://\[::1\]:* | http://\[::1\]/*)
	if [ "$ALLOW_HTTP" = "1" ]; then
		insecure_http_ok=1
		warn "RIFT_INSTALL_ALLOW_HTTP=1: downloading over plain http from loopback (testing only)"
	else
		err "refusing non-https RIFT_INSTALL_BASE_URL '${BASE_URL}' (set RIFT_INSTALL_ALLOW_HTTP=1 only for a local test server)"
	fi
	;;
*) err "refusing non-https RIFT_INSTALL_BASE_URL '${BASE_URL}': downloads must use https://" ;;
esac

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
# SHA256SUMS. This is an INTEGRITY check, not an authenticity check: SHA256SUMS
# comes from the same release, over the same HTTPS connection, as the binary.
# It catches a truncated or corrupted download, a stale CDN or mirror object,
# and a binary swapped without its sums file being updated to match. It does
# NOT protect against whoever can publish to the release (a compromised
# GitHub account or release pipeline) -- they can replace both files together.
# Protection against a network attacker comes from the HTTPS-only transport
# above. On any mismatch (or a missing checksum entry) we abort.
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
	# exact privileged command for the user to run themselves. The directory
	# is a fresh private mktemp one: a fixed name such as /tmp/rift could be
	# pre-created (or symlinked) by another local user, who would then control
	# the file the user is about to install as root.
	persist_dir="$(mktemp -d 2>/dev/null || mktemp -d -t rift-verified)" ||
		err "cannot create a directory to hold the verified binary"
	persist="${persist_dir}/rift"
	cp "${tmp}/${artifact}" "$persist"
	chmod 0755 "$persist"
	warn "${dest_dir} is not writable by this user; not using sudo automatically."
	warn "the verified binary is at ${persist}. To finish, run:"
	# Plain `install -m` plus a separate mkdir: macOS/BSD install has no -D.
	printf '\n    sudo mkdir -p %s && sudo install -m 0755 %s %s\n\n' \
		"$dest_dir" "$persist" "$target" >&2
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
