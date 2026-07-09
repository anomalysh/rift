# Changelog

## [0.2.0](https://github.com/anomalysh/rift/compare/v0.1.0...v0.2.0) (2026-07-09)


### Features

* **auth:** token authentication and the admin API ([04f6a15](https://github.com/anomalysh/rift/commit/04f6a15fafdc69cc3c4ee202c19252c51ccf39d2))
* **cli:** generate the man page and shell completions from one spec ([54a24e3](https://github.com/anomalysh/rift/commit/54a24e3f9b79ef200afb2971a72d4d5e62b27316))
* **cli:** ngrok-style TUI with a fixed header and a scrolling log ([9e70039](https://github.com/anomalysh/rift/commit/9e70039d036e29547dab92b0aa9eb7b48d5acb98))
* **cli:** ngrok-style tunl agent as a Bun single binary ([f0aa3dc](https://github.com/anomalysh/rift/commit/f0aa3dc2902f7bb9e0bceaa5088257dce09046b6))
* **cli:** rift https — tunnel to a local HTTPS upstream ([c979a94](https://github.com/anomalysh/rift/commit/c979a948883cc3cd32486362696da9a0fb16a4ee))
* **config:** make TLS a first-class, explicit mode ([7a316c5](https://github.com/anomalysh/rift/commit/7a316c579d9b25186d576107cb1f26c424c60a4c))
* **core:** add Session and Registry ports ([5b62502](https://github.com/anomalysh/rift/commit/5b62502d5a2bfa83e027b6a6db440d95b5586793))
* **core:** friendly adjective-noun-number generated subdomains ([4f6a0c5](https://github.com/anomalysh/rift/commit/4f6a0c5c925b6c7106669716c9414f34a53c1a8d))
* **deploy:** Caddy on-demand TLS, compose stacks, and operator tooling ([8766383](https://github.com/anomalysh/rift/commit/8766383e38fe1e3202e49c4df5615159b0764a55))
* **deploy:** configurable DNS-01 resolvers for split-horizon DNS ([45eddd6](https://github.com/anomalysh/rift/commit/45eddd625fa0f2d1543fc9c7edb0e3cf76c73fe5))
* **deploy:** mode-driven Caddyfile with pluggable DNS-01 providers ([a241f96](https://github.com/anomalysh/rift/commit/a241f9653870d2709b4393f6cc901c9795b825ec))
* **gateway:** WebSocket agent endpoint with stream multiplexing ([fdf547d](https://github.com/anomalysh/rift/commit/fdf547da5ef89baec661fd9a445a6f0d4ebc035e))
* **ingress:** peer-failure resilience — retry, stale-lease invalidation, circuit breaking ([aa63768](https://github.com/anomalysh/rift/commit/aa63768732d5ca7b656aee2c29a48c5cc76eac96))
* **ingress:** public HTTP routing and Caddy TLS authorization ([3ad66ae](https://github.com/anomalysh/rift/commit/3ad66ae2f648261162cc98e3d7a47e1358af2175))
* negotiate a supported protocol version range ([cbe36e6](https://github.com/anomalysh/rift/commit/cbe36e6ce045f73ff8ba2199b958c59e8226e0fc))
* **registry:** subdomain routing with an optional Redis locator ([2a4bfad](https://github.com/anomalysh/rift/commit/2a4bfaddead5461f9c44aae8de6ade9e3eca4f7c))
* **release:** cross-platform binaries, installer, man page, completions ([d68ef50](https://github.com/anomalysh/rift/commit/d68ef50760e4b417ab2d9704335368163b8babbb))
* **release:** semver automation with release-please ([9be76e5](https://github.com/anomalysh/rift/commit/9be76e5147f38b5ec0d0d587b2d8a36cc4d74ef8))
* scaffold repo with wire protocol and core domain ([7be49ce](https://github.com/anomalysh/rift/commit/7be49cec04c101a32713ec0093308471af295a51))
* **store:** postgres persistence, migrations, and an in-memory store ([d385ed9](https://github.com/anomalysh/rift/commit/d385ed9f2f848ff6dd2671b6d76374a61550dbe9))
* **tools:** backup and restore, proven by destroy-then-restore ([8288fed](https://github.com/anomalysh/rift/commit/8288feda36a19102a2216be2627edd06de6483fc))
* **tools:** guided setup, staged ship pipeline, and a verify gate ([501bc9b](https://github.com/anomalysh/rift/commit/501bc9b64d711d571bcca1738228465babef4335))
* **tools:** host hardening, tested in a throwaway Debian container ([a559f9f](https://github.com/anomalysh/rift/commit/a559f9fe31605591dd8dd2deaaa0d6d5598cd3e5))
* **tools:** pluggable VPS provisioning, tested against a mock cloud API ([ebc2b7a](https://github.com/anomalysh/rift/commit/ebc2b7a4228b0fc6789eb6584eb9a5780d3783d0))
* **tools:** resource preflight guards before expensive operations ([a03e279](https://github.com/anomalysh/rift/commit/a03e2794336a57879966a5d06bd2e4805a633110))
* **tunld:** reaper, structured logging, and the server entrypoint ([8732583](https://github.com/anomalysh/rift/commit/8732583620e922c931c635d85e579de6e2df0ee4))
* WebSocket/TCP/TLS tunnels, smarter reconnect, and a modern CLI ([75a05b9](https://github.com/anomalysh/rift/commit/75a05b9f89f86509a2a3df69139e5442e242540e))


### Bug Fixes

* **ci:** make GitHub artifact attestation opt-in, not a hard failure ([540f74c](https://github.com/anomalysh/rift/commit/540f74c020b7a6817496172d77c48ab6ca04f534))
* **cli:** make the TUI redraw scroll-safe so events stop painting over it ([21bf68c](https://github.com/anomalysh/rift/commit/21bf68c39cd8d173f87b5329bb1bc0304a045768))
* **cli:** preserve content-length so local servers see the request body ([2c0f9c2](https://github.com/anomalysh/rift/commit/2c0f9c264ea9f29e641e9f4f9b99122c67953639))
* **cli:** TUI wrap corruption, version-aware reconnect, biome, TypeScript 7 ([8dfb110](https://github.com/anomalysh/rift/commit/8dfb110b56c46e9463135ff09b6f1900e797907f))
* **config:** the subdomain blocklist extends the defaults, it does not replace them ([27d7f3a](https://github.com/anomalysh/rift/commit/27d7f3a03cf15955223ae623485b7ebb8a025456))
* **deploy:** dns01 resolvers profile defaulted to an empty import path ([f73fd5f](https://github.com/anomalysh/rift/commit/f73fd5f21bab07d8b15d2b02bed4ba673af4ae16))
* **deploy:** gateway hostname never received a TLS certificate ([071f759](https://github.com/anomalysh/rift/commit/071f7592867d4d59ba3ce2e2ea53dbd1d824fc52))
* **deploy:** serve the base domain, and reload Caddy on config change ([c6ac434](https://github.com/anomalysh/rift/commit/c6ac434bfb3954894420ba04cd8fd50112a637b5))
* **gateway:** data race on the upgraded raw stream body reader ([f6e6fb3](https://github.com/anomalysh/rift/commit/f6e6fb3e92bbb80fb2d1d5c36a508b57ec304cd2))
* **gateway:** revoking a token now tears down its live tunnels ([d3882d7](https://github.com/anomalysh/rift/commit/d3882d7b1e390f74dedf1ac9dbeddb3f2c000469))
* **ingress:** peer forwarding lost the request path and body ([276f1c7](https://github.com/anomalysh/rift/commit/276f1c710b90406b7443bbfb8155dbb5f312fbcf))
* **install:** create parent dirs when installing the man page ([aac18b3](https://github.com/anomalysh/rift/commit/aac18b3f232277c7f191a2000d39b2075963c541))
* **server:** update dependencies and Go toolchain to clear known CVEs ([c5e775a](https://github.com/anomalysh/rift/commit/c5e775a00e25975fe8159513804eeb23e947edab))
* target the master branch, not main ([26220ee](https://github.com/anomalysh/rift/commit/26220eeaae2452eac73d24ed9d30633d35b9cf3b))


### Refactoring

* move to the anomalysh GitHub org ([0b98302](https://github.com/anomalysh/rift/commit/0b983025ed8f32d850a42e5af625ce7190e84d3e))
* rename tunl to rift and move to anomaly.sh ([ed8f2af](https://github.com/anomalysh/rift/commit/ed8f2affdd0dbc9331b4d3c0b8d41971af4ff9f4))
* **repo:** organize cli/server/docs-site under projects/ as a mise monorepo ([e9ae0c4](https://github.com/anomalysh/rift/commit/e9ae0c4b49b3ee996a1bcae86add665fb5d7f8f2))
* **tasks:** move dev/test workflows to mise file tasks, Makefile ops-only ([4827618](https://github.com/anomalysh/rift/commit/48276183bdb9ce1e6708830d29a09c3ee718ef1a))


### Documentation

* add README ([c9591fb](https://github.com/anomalysh/rift/commit/c9591fb6c0cbcc991f8b40112a646dd9c53f8ffb))
* **site:** full Astro/Starlight documentation site ([ea0f25a](https://github.com/anomalysh/rift/commit/ea0f25a7bc6feefa3e88a99e0e2ee38d5814d7d5))
* TLS modes, DNS providers, and the e2e harness ([a0396f9](https://github.com/anomalysh/rift/commit/a0396f923c33a5300d3114d9a6012119e08efdd1))
