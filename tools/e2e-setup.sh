#!/usr/bin/env bash
set -euo pipefail

# End-to-end test for the setup wizard (tools/setup.sh). It runs the wizard for
# real and proves the files it writes are valid -- crucially by feeding each one
# to the REAL riftd config loader, so the wizard can never emit a .env that
# fails config.Load. Nothing here touches the VPS or an existing .env.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/lib/common.sh
. "$SCRIPT_DIR/lib/common.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SETUP="$SCRIPT_DIR/setup.sh"

usage() {
	cat >&2 <<EOF
Usage: tools/e2e-setup.sh [--keep]

Exercises tools/setup.sh non-interactively for every TLS mode and asserts:
  - each file is written mode 600 with a valid RIFT_TLS_MODE and a >=32-char
    admin token, and PASSES the real riftd config validator (config.Load);
  - production always sets a TLS mode, and an invalid mode is refused;
  - the wizard refuses to overwrite without --force and refuses any path git
    would commit;
  - two runs generate different admin tokens (the CSPRNG is real);
  - a production+Redis run yields a >=32-char peer secret;
  - dns01 without a provider is refused, and dns01+rfc2136 emits the rfc2136 vars.

Options:
  --keep   Leave the temp workdir (and built riftd) in place for inspection.
  -h, --help
EOF
}

keep=false
while [ "$#" -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--keep) keep=true ;;
	*) die "unexpected argument: $1 (see --help)" ;;
	esac
	shift
done

require_cmd go git tar timeout stat

WORK="$(mktemp -d)"
cleanup() {
	local status=$?
	if [ "$keep" != true ]; then
		rm -rf "$WORK"
	else
		log_warn "workdir kept: $WORK"
	fi
	exit "$status"
}
trap cleanup EXIT INT TERM

pass=0
fail=0
ok() {
	printf '    ok    %s\n' "$*"
	pass=$((pass + 1))
}
no() {
	printf '    FAIL  %s\n' "$*"
	fail=$((fail + 1))
}

# ---------------------------------------------------------------------------
# Build the real validator. Prefer the working tree; fall back to a pristine
# export of HEAD when the tree is mid-edit and does not compile (the config
# package is what we exercise, and it is self-contained). Either way this is
# the SAME config.Load the server boots with.
# ---------------------------------------------------------------------------
RIFTD_BIN="$WORK/riftd"
build_validator() {
	log_info "building riftd (the real config validator)..."
	if ( cd "$REPO_ROOT/server" && CGO_ENABLED=0 go build -o "$RIFTD_BIN" ./cmd/riftd ) >/dev/null 2>&1; then
		log_info "built from the working tree"
		return 0
	fi
	log_warn "working tree does not compile (concurrent edit?); building from a pristine HEAD export"
	local src="$WORK/head-src"
	mkdir -p "$src"
	git -C "$REPO_ROOT" archive HEAD | tar -x -C "$src"
	( cd "$src/server" && CGO_ENABLED=0 go build -o "$RIFTD_BIN" ./cmd/riftd ) >/dev/null 2>&1 ||
		die "could not build riftd from HEAD either"
	log_info "built from HEAD"
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# get_var FILE VARNAME -- value as the shell would see it (quotes stripped).
get_var() { (
	set -a
	# shellcheck disable=SC1090
	. "$1"
	set +a
	printf '%s' "${!2:-}"
); }

file_mode() { stat -c '%a' "$1"; }

# validate_boots_past_config FILE LABEL -- run riftd with the generated env and
# assert it clears config.Load. A valid file then fails to reach the (absent)
# Postgres; an invalid one prints "config: N problem(s)". Grepping tells them
# apart. This is the load-bearing assertion: it proves the wizard cannot write
# a file the server would reject at boot.
validate_boots_past_config() {
	local envfile="$1" label="$2" rc=0
	local log="$WORK/${label}.riftd.log"
	(
		set -a
		# shellcheck disable=SC1090
		. "$envfile"
		set +a
		# Bound the wait; the bundled DSN points at an unresolvable host anyway.
		export RIFT_POSTGRES_CONNECT_TIMEOUT=3s
		exec timeout 25 "$RIFTD_BIN"
	) >"$log" 2>&1 || rc=$?
	if grep -qE 'config: [0-9]+ problem' "$log"; then
		no "$label: passes config.Load (validator reported a config error)"
		sed 's/^/          /' "$log" >&2
	elif [ "$rc" -ne 0 ] && grep -q 'postgres' "$log"; then
		ok "$label: passes config.Load, then fails later at Postgres (as expected)"
	else
		no "$label: passes config.Load (unexpected outcome, rc=$rc)"
		sed 's/^/          /' "$log" >&2
	fi
}

# gen ENVSTRING OUT -- run the wizard non-interactively. ENVSTRING is a set of
# VAR=value words applied only to this invocation.
gen() {
	local envstr="$1" out="$2"
	# shellcheck disable=SC2086
	env $envstr "$SETUP" --non-interactive --output "$out" >"$WORK/gen.log" 2>&1
}

build_validator

# ---------------------------------------------------------------------------
# 1 + 2. Every TLS mode: written 600, valid mode, >=32-char token, passes the
#        real validator.
# ---------------------------------------------------------------------------
printf '\n=== per-mode generation + real config.Load ===\n'

declare -A MODE_ENV
MODE_ENV[internal]=""
MODE_ENV[http01]="SETUP_ENV=production SETUP_TLS_MODE=http01 SETUP_ACME_EMAIL=ops@example.com"
MODE_ENV[dns01]="SETUP_ENV=production SETUP_TLS_MODE=dns01 SETUP_ACME_DNS_PROVIDER=rfc2136 SETUP_ACME_EMAIL=ops@example.com SETUP_DNS_SERVER=ns1.example.com:53 SETUP_DNS_TSIG_KEY_NAME=rift-acme. SETUP_DNS_TSIG_KEY=dGVzdC1rZXk= SETUP_ACME_DNS_RESOLVERS=1.1.1.1,8.8.8.8"
MODE_ENV[self]="SETUP_ENV=production SETUP_TLS_MODE=self SETUP_TLS_CERT_FILE=/certs/fullchain.pem SETUP_TLS_KEY_FILE=/certs/key.pem"

for mode in internal http01 dns01 self; do
	out="$WORK/.env.$mode"
	if ! gen "${MODE_ENV[$mode]}" "$out"; then
		no "$mode: wizard exited non-zero"
		sed 's/^/          /' "$WORK/gen.log" >&2
		continue
	fi
	[ -f "$out" ] && ok "$mode: file written" || { no "$mode: no file written"; continue; }

	m="$(file_mode "$out")"
	[ "$m" = "600" ] && ok "$mode: mode is 600" || no "$mode: mode is $m, want 600"

	tlsmode="$(get_var "$out" RIFT_TLS_MODE)"
	[ "$tlsmode" = "$mode" ] && ok "$mode: RIFT_TLS_MODE=$tlsmode" || no "$mode: RIFT_TLS_MODE=[$tlsmode], want $mode"

	tok="$(get_var "$out" RIFT_ADMIN_TOKEN)"
	[ "${#tok}" -ge 32 ] && ok "$mode: admin token is ${#tok} chars (>=32)" || no "$mode: admin token only ${#tok} chars"

	validate_boots_past_config "$out" "$mode"
done

# self mode must have emitted both cert/key paths, dns01 the provider vars.
selfcert="$(get_var "$WORK/.env.self" RIFT_TLS_CERT_FILE)"
selfkey="$(get_var "$WORK/.env.self" RIFT_TLS_KEY_FILE)"
{ [ -n "$selfcert" ] && [ -n "$selfkey" ]; } && ok "self: cert and key paths emitted" || no "self: cert/key paths missing"

# ---------------------------------------------------------------------------
# 3. Production always sets a TLS mode; an invalid mode is refused.
# ---------------------------------------------------------------------------
printf '\n=== production always sets a valid TLS mode ===\n'
gen "SETUP_ENV=production SETUP_TLS_MODE=http01 SETUP_ACME_EMAIL=ops@example.com" "$WORK/.env.prod"
prodmode="$(get_var "$WORK/.env.prod" RIFT_TLS_MODE)"
case "$prodmode" in
internal | http01 | dns01 | self) ok "production emits a valid TLS mode ($prodmode)" ;;
*) no "production TLS mode is [$prodmode]" ;;
esac
if grep -q '^RIFT_TLS_MODE=' "$WORK/.env.prod"; then ok "RIFT_TLS_MODE is present, never missing"; else no "RIFT_TLS_MODE line missing"; fi
if env SETUP_ENV=production SETUP_TLS_MODE=bogus "$SETUP" --non-interactive --output "$WORK/.env.bogus" >/dev/null 2>&1; then
	no "an invalid TLS mode was accepted"
else
	ok "an invalid TLS mode is refused"
fi
[ -f "$WORK/.env.bogus" ] && no "refused run still wrote a file" || ok "refused run wrote nothing"

# ---------------------------------------------------------------------------
# 4. Refuse to overwrite without --force; refuse a committable path.
# ---------------------------------------------------------------------------
printf '\n=== overwrite + gitignore guards ===\n'
exists="$WORK/.env.exists"
printf 'PRE-EXISTING\n' >"$exists"
if "$SETUP" --non-interactive --output "$exists" >/dev/null 2>&1; then
	no "overwrote an existing file without --force"
else
	ok "refuses to overwrite an existing file without --force"
fi
grep -q PRE-EXISTING "$exists" && ok "existing file left untouched" || no "existing file was modified"
if "$SETUP" --non-interactive --force --output "$exists" >/dev/null 2>&1; then
	ok "--force overwrites an existing file"
else
	no "--force failed to overwrite"
fi

# A path git would track must be refused even WITH --force (safety floor, not a
# preference). deploy/**/*.example is git-negated, i.e. explicitly committable.
committable="$REPO_ROOT/deploy/zz-wizard-refuse.example"
if git -C "$REPO_ROOT" check-ignore -q -- "$committable"; then
	no "test path is unexpectedly gitignored; cannot exercise the guard"
else
	if "$SETUP" --non-interactive --force --output "$committable" >/dev/null 2>&1; then
		no "wrote to a committable path"
	else
		ok "refuses a committable (non-gitignored) path, even with --force"
	fi
	[ -e "$committable" ] && { no "a file was created at the committable path"; rm -f "$committable"; } || ok "committable path left nonexistent"
fi

# Proof it will not clobber the real, tracked .env.example.
before="$(sha256sum "$REPO_ROOT/.env.example")"
"$SETUP" --non-interactive --force --output "$REPO_ROOT/.env.example" >/dev/null 2>&1 || true
after="$(sha256sum "$REPO_ROOT/.env.example")"
[ "$before" = "$after" ] && ok ".env.example is never clobbered" || no ".env.example was modified!"

# ---------------------------------------------------------------------------
# 5. Two runs -> different admin tokens (the CSPRNG is actually used).
# ---------------------------------------------------------------------------
printf '\n=== CSPRNG uniqueness ===\n'
gen "" "$WORK/.env.rand1"
gen "" "$WORK/.env.rand2"
t1="$(get_var "$WORK/.env.rand1" RIFT_ADMIN_TOKEN)"
t2="$(get_var "$WORK/.env.rand2" RIFT_ADMIN_TOKEN)"
[ -n "$t1" ] && [ "$t1" != "$t2" ] && ok "two runs produced different admin tokens" || no "admin tokens were equal or empty"

# ---------------------------------------------------------------------------
# 6. Production + Redis: admin token and peer secret both >=32 chars.
# ---------------------------------------------------------------------------
printf '\n=== production + Redis secrets ===\n'
gen "SETUP_ENV=production SETUP_TLS_MODE=http01 SETUP_ACME_EMAIL=ops@example.com SETUP_REDIS=yes SETUP_NODE_ADVERTISE_URL=http://10.0.0.4:8080" "$WORK/.env.redis"
rtok="$(get_var "$WORK/.env.redis" RIFT_ADMIN_TOKEN)"
peer="$(get_var "$WORK/.env.redis" RIFT_PEER_SECRET)"
[ "${#rtok}" -ge 32 ] && ok "prod admin token is ${#rtok} chars (>=32)" || no "prod admin token only ${#rtok} chars"
[ "${#peer}" -ge 32 ] && ok "prod peer secret is ${#peer} chars (>=32)" || no "prod peer secret only ${#peer} chars"
validate_boots_past_config "$WORK/.env.redis" "prod+redis"

# ---------------------------------------------------------------------------
# 7. dns01 without a provider is refused; dns01+rfc2136 emits the rfc2136 vars.
# ---------------------------------------------------------------------------
printf '\n=== dns01 provider handling ===\n'
if env SETUP_ENV=production SETUP_TLS_MODE=dns01 SETUP_ACME_EMAIL=ops@example.com \
	"$SETUP" --non-interactive --output "$WORK/.env.dns-noprov" >/dev/null 2>&1; then
	no "dns01 without a provider was accepted"
else
	ok "dns01 without a provider is refused"
fi
missing=0
for v in RIFT_ACME_DNS_PROVIDER RIFT_DNS_SERVER RIFT_DNS_TSIG_KEY_NAME RIFT_DNS_TSIG_KEY_ALG RIFT_DNS_TSIG_KEY; do
	grep -q "^$v=" "$WORK/.env.dns01" || { missing=1; log_warn "dns01 file missing $v"; }
done
[ "$missing" -eq 0 ] && ok "dns01+rfc2136 emits the rfc2136 vars" || no "dns01+rfc2136 is missing rfc2136 vars"
prov="$(get_var "$WORK/.env.dns01" RIFT_ACME_DNS_PROVIDER)"
[ "$prov" = "rfc2136" ] && ok "RIFT_ACME_DNS_PROVIDER=rfc2136" || no "provider is [$prov]"

# ---------------------------------------------------------------------------
# 8. No unmasked secret ever reached the wizard's own stdout/stderr.
# ---------------------------------------------------------------------------
printf '\n=== secret never printed unmasked ===\n'
logf="$WORK/leak.log"
env SETUP_ENV=production SETUP_TLS_MODE=http01 SETUP_ACME_EMAIL=ops@example.com \
	"$SETUP" --non-interactive --output "$WORK/.env.leak" >"$logf" 2>&1
leaktok="$(get_var "$WORK/.env.leak" RIFT_ADMIN_TOKEN)"
if grep -qF "$leaktok" "$logf"; then
	no "the full admin token appeared in wizard output"
else
	ok "the full admin token never appeared in wizard output (only masked)"
fi

printf '\n=== summary ===\n  passed=%d failed=%d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || die "e2e-setup failed"
log_info "e2e-setup passed"
