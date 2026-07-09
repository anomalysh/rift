# rift MCP demo

A minimal [MCP](https://modelcontextprotocol.io) server built with Bun and the
official `@modelcontextprotocol/sdk`, over the **Streamable HTTP** transport — so
you can put an MCP endpoint on a public URL with rift.

## Run

```sh
bun install
bun run start        # http://localhost:3939/mcp   (bun run dev to reload)
```

## Expose it with rift

```sh
rift http 3939 mcp   # -> https://mcp.<your-rift-domain>/mcp
```

The endpoint is `/mcp`. Point any MCP client at it:

- **MCP Inspector:** `bunx @modelcontextprotocol/inspector`, then connect to the
  public `…/mcp` URL with transport **Streamable HTTP**.
- **Claude / other clients:** add it as a remote (HTTP) MCP server pointing at
  the `…/mcp` URL.

## Tools

| Tool | Args | Does |
| --- | --- | --- |
| `echo` | `text` | Echoes the text back. |
| `roll_dice` | `count`, `sides` | Rolls dice and reports each roll + the total. |
| `server_time` | — | Returns the server's current time. |

## Smoke test with curl (no client)

An `initialize` handshake — a `200` with an `mcp-session-id` response header and a
`result` body means it works:

```sh
curl -isS http://localhost:3939/mcp \
  -H 'content-type: application/json' \
  -H 'accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"0"}}}'
```
