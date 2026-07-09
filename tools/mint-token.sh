#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"

usage() {
	cat >&2 <<'EOF'
Usage: tools/mint-token.sh NAME

Create an admin API token named NAME and print the plaintext token to stdout.

The admin API is NOT publicly exposed, so run this on the VPS itself or over an
SSH tunnel, e.g.:
    tools/ssh.sh -L 8082:127.0.0.1:8082   # in another terminal
    RIFT_ADMIN_URL=http://127.0.0.1:8082 tools/mint-token.sh my-laptop

Environment:
  RIFT_ADMIN_URL    Admin API base URL   (default: http://127.0.0.1:8082)
  RIFT_ADMIN_TOKEN  (required) bearer token authenticating the admin caller
EOF
}

case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
"") usage; die "NAME is required" ;;
-*) die "unexpected option: $1 (see --help)" ;;
esac

name="$1"

require_cmd curl
require_env RIFT_ADMIN_TOKEN

base="${RIFT_ADMIN_URL:-http://127.0.0.1:8082}"
url="${base%/}/v1/tokens"

# Build the JSON body safely (jq escapes NAME; the fallback assumes a simple name).
if command -v jq >/dev/null 2>&1; then
	body="$(jq -nc --arg n "$name" '{name: $n}')"
else
	body="{\"name\":\"$name\"}"
fi

log_info "creating token '$name' via $url"
resp="$(curl -fsS -X POST "$url" \
	-H "Authorization: Bearer $RIFT_ADMIN_TOKEN" \
	-H "Content-Type: application/json" \
	-d "$body")" || die "admin API request failed (is the admin listener reachable and RIFT_ADMIN_TOKEN correct?)"

# Print the plaintext token. The exact JSON field is resolved leniently since
# the server response shape is owned by another component.
if command -v jq >/dev/null 2>&1; then
	token="$(printf '%s' "$resp" | jq -r '.token // .plaintext // .secret // empty')"
	if [ -n "$token" ]; then
		printf '%s\n' "$token"
	else
		log_warn "no token field found in the response; printing it raw:"
		printf '%s\n' "$resp"
	fi
else
	printf '%s\n' "$resp"
fi
