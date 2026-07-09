# Provisioning providers

`tools/provision.sh` is provider-agnostic. Everything cloud-specific lives in one
file per provider, `tools/providers/<name>.sh`, which `provision.sh` **sources**.
Adding Hetzner, Vultr, DigitalOcean, … is a single new file that implements the
contract below — no change to `provision.sh`.

A provider file is sourced, not executed: it must only define functions and
variables, with no side effects at source time. It may use the helpers from
`tools/lib/common.sh` (`log_info`, `log_warn`, `die`, `require_cmd`,
`require_env`), which `provision.sh` sources first.

## The contract

Implement exactly these functions. Output goes to **stdout**; logs and errors go
to stderr (use the `log_*` helpers). A function that cannot proceed calls `die`.

| Function | Args | Must output / do |
| --- | --- | --- |
| `provider_name` | — | echo the provider's name |
| `provider_require_env` | — | verify prerequisites; `die` on anything the client genuinely cannot proceed without |
| `provider_create` | `NAME REGION TYPE IMAGE PUBKEY` | create an instance; echo its **id** |
| `provider_status` | `ID` | echo a status string; it must echo exactly `running` once the instance is ready |
| `provider_ipv4` | `ID` | echo the public IPv4 |
| `provider_ipv6` | `ID` | echo the public IPv6, or empty if none |
| `provider_destroy` | `ID` | destroy the instance; **idempotent** — success if it is already gone |
| `provider_list` | — | echo one instance per line as `id<TAB>label<TAB>status<TAB>ipv4` |

### Conventions every provider must honour

- **The API base URL comes from the environment**, `RIFT_PROVIDER_API_BASE`,
  which `provision.sh` sets from `--api-base`. This is what lets the e2e point a
  provider at a mock server instead of the real cloud. Fall back to the real
  base URL when it is unset.

- **Secrets never appear in `argv`.** Anything visible in `ps` is visible to
  every user on the box. Pass tokens and generated passwords to `curl` through a
  `--config` file read from **stdin**, and to helper processes through the
  **environment**, never as command-line arguments.

- **Dry-run.** When `RIFT_PROVIDER_DRY_RUN` is truthy, a provider must make **no
  network calls**: it prints the call it *would* make and returns a plausible
  canned value so `provision.sh` can walk the whole flow without touching the
  API. `provision.sh` sets this variable for `--dry-run`.

- **`provider_require_env` and credentials.** `die` on missing *client-side*
  prerequisites. A credential the API itself validates (a bearer token the
  server answers with `401`) is best left to the server: that answer is
  authoritative and lets the e2e prove the credential is actually transmitted.
  The Linode provider therefore only *warns* on an empty token and treats the
  server's `401` as fatal — see `linode.sh`.

## Reference implementation

`linode.sh` implements the contract against Linode API v4. Use it as the
template for a new provider.
