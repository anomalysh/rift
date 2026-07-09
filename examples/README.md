# rift examples

Two tiny Bun projects to point a rift tunnel at.

| Project | What it is | Expose it with |
| --- | --- | --- |
| [`http-demo`](./http-demo) | A plain HTTP server that renders a "you're tunneling through rift" landing page (live clock, echo box, the forwarded request headers). | `rift http 3000 demo` |
| [`mcp-server`](./mcp-server) | An MCP server (official `@modelcontextprotocol/sdk`, Streamable HTTP transport) with a few demo tools, so you can put an MCP endpoint on a public URL. | `rift http 3939 mcp` |

Each is standalone (its own `package.json`); they are not part of the rift
monorepo build. From either directory:

```sh
bun install        # mcp-server only; http-demo has no dependencies
bun run start      # or `bun run dev` to reload on change
```

Then, from another shell, tunnel the port with the rift CLI:

```sh
rift http 3000 demo     # -> https://demo.<your-rift-domain>
```

The public endpoint is HTTPS even though the local server speaks plain HTTP —
rift's edge terminates TLS. To tunnel a local server that itself speaks HTTPS,
use `rift https <port>` instead.
