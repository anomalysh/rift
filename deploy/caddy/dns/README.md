# DNS-01 provider snippets

One file per Caddy DNS solver. `RIFT_ACME_DNS_PROVIDER` selects which is
imported, so only the chosen provider's environment variables need to be set.

A provider works only if its plugin was compiled into the Caddy image. Add it
to `RIFT_CADDY_DNS_PLUGINS` and run `tools/build-caddy.sh`. If the plugin is
missing, Caddy refuses to start with:

    module not registered: dns.providers.<name>

To add a provider not listed here, drop in `<name>.caddy` using the syntax from
that plugin's README, add its module path to `RIFT_CADDY_DNS_PLUGINS`, and run
`tools/build-caddy.sh --validate` to check the snippet parses before deploying.
