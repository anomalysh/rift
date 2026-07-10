#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
# shellcheck source=tools/lib/e2e-harness.sh
. "$SCRIPT_DIR/lib/e2e-harness.sh"

# e2e-install.sh -- prove the curl|sh installer against a real (local) release
# tree. install.sh is the highest-blast-radius script -- it drops a binary onto
# a user's PATH -- and its trust anchor is the SHA256SUMS check. This serves a
# fake release over http and asserts: a clean install verifies and runs, a
# TAMPERED checksum is refused before anything touches PATH, a MISSING checksum
# entry is refused, and --dry-run changes nothing. No network, no Docker.

usage() {
	cat >&2 <<'EOF'
Usage: tools/e2e-install.sh

Hermetically test tools/install.sh: serve a fake release tree over a local HTTP
server and assert clean install, checksum-tamper rejection, and --dry-run.
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
esac

require_cmd python3 sha256sum

PORT="${RIFT_E2E_INSTALL_PORT:-18099}"
WORK="$(rift_mktemp_dir)"
RELEASE="$WORK/release/v0.1.0"
mkdir -p "$RELEASE"

# The fake binaries answer --version so install.sh's post-install check passes.
# One file per platform target install.sh might resolve to on the test host, so
# the suite runs anywhere. They are identical; only their names differ.
artifacts="rift-linux-x64 rift-linux-x64-musl rift-linux-arm64 rift-linux-arm64-musl rift-darwin-x64 rift-darwin-arm64"
for a in $artifacts; do
	cat >"$RELEASE/$a" <<'BIN'
#!/bin/sh
case "$1" in --version) echo "0.1.0" ;; *) echo "rift (test build)" ;; esac
BIN
done

# The genuine SHA256SUMS: the real digests of the artifacts above.
write_good_sums() {
	(cd "$RELEASE" && sha256sum $artifacts >SHA256SUMS)
}
write_good_sums

# Serve the release tree read-only over loopback.
(cd "$WORK/release" && exec python3 -m http.server "$PORT" --bind 127.0.0.1) \
	>"$WORK/http.log" 2>&1 &
register_cleanup "kill $! 2>/dev/null || true"
wait_for_tcp "$PORT" "install fixture server"

base="http://127.0.0.1:$PORT"
run_install() {
	# Isolate every install: its own dir, no inherited RIFT_INSTALL_*.
	env -i PATH="$PATH" HOME="$WORK" \
		RIFT_INSTALL_BASE_URL="$base" RIFT_INSTALL_VERSION="0.1.0" RIFT_INSTALL_DIR="$1" \
		sh "$SCRIPT_DIR/install.sh" >"$2" 2>&1
}

printf '=== install.sh e2e ===\n'

# 1. Clean install: verifies, installs, and the binary runs.
bin1="$WORK/bin-clean"
rc=0
run_install "$bin1" "$WORK/clean.log" || rc=$?
check "clean install exits zero" "$rc" "0"
check "rift binary is installed" "$([ -x "$bin1/rift" ] && echo yes || echo no)" "yes"
check "installed binary runs (--version)" "$("$bin1/rift" --version 2>/dev/null || true)" "0.1.0"
check_contains "installer reports the checksum verified" "$(cat "$WORK/clean.log")" "checksum verified"

# 2. Tampered SHA256SUMS: the digest no longer matches, so install must refuse
# and leave the (fresh) target dir empty.
sed "s/^[0-9a-f]\{64\}/$(printf 'd%.0s' $(seq 1 64))/" "$RELEASE/SHA256SUMS" >"$RELEASE/SHA256SUMS.bad"
mv "$RELEASE/SHA256SUMS.bad" "$RELEASE/SHA256SUMS"
bin2="$WORK/bin-tampered"
rc=0
run_install "$bin2" "$WORK/tampered.log" || rc=$?
check "tampered checksum is rejected (non-zero)" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check "no binary installed after a tampered checksum" "$([ -e "$bin2/rift" ] && echo yes || echo no)" "no"
check_contains "installer explains the mismatch" "$(cat "$WORK/tampered.log")" "checksum mismatch"
write_good_sums # restore the genuine sums

# 3. Missing checksum entry: SHA256SUMS with no line for the artifact -> refuse.
grep -v 'rift-linux' "$RELEASE/SHA256SUMS" | grep -v 'rift-darwin' >"$RELEASE/SHA256SUMS.empty" || true
cp "$RELEASE/SHA256SUMS.empty" "$RELEASE/SHA256SUMS"
bin3="$WORK/bin-nosum"
rc=0
run_install "$bin3" "$WORK/nosum.log" || rc=$?
check "missing checksum entry is rejected" "$([ "$rc" -ne 0 ] && echo yes || echo no)" "yes"
check_contains "installer refuses without a checksum" "$(cat "$WORK/nosum.log")" "no checksum for"
write_good_sums

# 4. --dry-run changes nothing.
bin4="$WORK/bin-dry"
rc=0
env -i PATH="$PATH" HOME="$WORK" RIFT_INSTALL_BASE_URL="$base" RIFT_INSTALL_VERSION="0.1.0" \
	RIFT_INSTALL_DIR="$bin4" sh "$SCRIPT_DIR/install.sh" --dry-run >"$WORK/dry.log" 2>&1 || rc=$?
check "--dry-run exits zero" "$rc" "0"
check "--dry-run installs nothing" "$([ -e "$bin4/rift" ] && echo yes || echo no)" "no"

print_summary "install e2e"
