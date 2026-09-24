#!/usr/bin/env bash
# tools/release.sh — compatibility entry point for .github/workflows/release.yml,
# which still invokes this path. The implementation moved to
# tools/cmd/release/cli.sh (`rift-ops release cli`) in the tools/ restructure;
# without this shim every v* tag failed to build and upload any binaries.
#
# In GitHub Actions it adds --strict, so a release never ships silently missing
# a platform whose cross-compile failed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "${GITHUB_ACTIONS:-}" = true ]; then
	set -- --strict "$@"
fi
exec bash "$SCRIPT_DIR/cmd/release/cli.sh" "$@"
