#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/../../lib/common.sh"

usage() {
	cat >&2 <<'EOF'
Usage: rift-ops secret mint-token NAME

Create an admin API token named NAME and print the plaintext token to stdout.

The admin API is NOT published in production: it listens only on riftd's
container address on the private compose network. Point RIFT_ADMIN_URL at that
address, on the VPS itself or through an SSH tunnel, e.g.:
    # on the VPS: the riftd container's address
    ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' rift-riftd-1)"
    # on your machine, in another terminal:
    tools/rift-ops ssh ssh -L "8082:$ip:8082"
    RIFT_ADMIN_URL=http://127.0.0.1:8082 rift-ops secret mint-token my-laptop

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

# Build the JSON body safely: jq escapes NAME. Without jq, only accept a name
# that needs no escaping, rather than splice arbitrary text into JSON.
if command -v jq >/dev/null 2>&1; then
	body="$(jq -nc --arg n "$name" '{name: $n}')"
else
	case "$name" in
	*[!A-Za-z0-9._@\ -]*) die "NAME may only contain letters, digits, space and . _ @ - (or install jq)" ;;
	esac
	body="{\"name\":\"$name\"}"
fi

log_info "creating token '$name' via $url"
# The admin token goes to curl through a config file on stdin, never on its
# argv, where any local user could read it from ps or /proc/<pid>/cmdline.
resp="$(printf 'header = "Authorization: Bearer %s"\n' "$RIFT_ADMIN_TOKEN" |
	curl -fsS -K - -X POST "$url" \
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
