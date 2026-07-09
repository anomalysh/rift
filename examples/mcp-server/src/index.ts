// A minimal MCP server over the Streamable HTTP transport, built with Bun and
// the official @modelcontextprotocol/sdk. Run it, expose the port with rift, and
// point any MCP client (Claude, the MCP Inspector, ...) at the public /mcp URL.
//
//   bun install
//   bun run src/index.ts          # http://localhost:3939/mcp
//   rift http 3939 mcp            # -> https://mcp.<your-rift-domain>/mcp
//
// One MCP session is kept per `mcp-session-id` (returned on initialize), so it
// works with real clients that do initialize -> tools/list -> tools/call.

import { randomUUID } from "node:crypto";
import {
  createServer,
  type IncomingMessage,
  type ServerResponse,
} from "node:http";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { isInitializeRequest } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";

const PORT = Number(process.env.PORT ?? 3939);

/** Build a fresh MCP server with the demo tools. One per session. */
function buildServer(): McpServer {
  const server = new McpServer({ name: "rift-mcp-demo", version: "0.1.0" });

  server.registerTool(
    "echo",
    {
      title: "Echo",
      description: "Echo back whatever text you send.",
      inputSchema: { text: z.string().describe("the text to echo") },
    },
    async ({ text }) => ({ content: [{ type: "text", text }] }),
  );

  server.registerTool(
    "roll_dice",
    {
      title: "Roll dice",
      description: "Roll one or more dice and report each roll and the total.",
      inputSchema: {
        count: z.number().int().min(1).max(100).default(1).describe("how many dice"),
        sides: z.number().int().min(2).max(1000).default(6).describe("sides per die"),
      },
    },
    async ({ count, sides }) => {
      const rolls = Array.from(
        { length: count },
        () => 1 + Math.floor(Math.random() * sides),
      );
      const total = rolls.reduce((a, b) => a + b, 0);
      return {
        content: [{ type: "text", text: `🎲 ${rolls.join(", ")}  (total ${total})` }],
      };
    },
  );

  server.registerTool(
    "server_time",
    {
      title: "Server time",
      description: "The current time on the server, ISO 8601.",
      inputSchema: {},
    },
    async () => ({ content: [{ type: "text", text: new Date().toISOString() }] }),
  );

  return server;
}

// One transport per active session id.
const transports = new Map<string, StreamableHTTPServerTransport>();

async function readJson(req: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) chunks.push(chunk as Buffer);
  if (chunks.length === 0) return undefined;
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

function fail(res: ServerResponse, status: number, message: string): void {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify({ jsonrpc: "2.0", id: null, error: { code: -32000, message } }));
}

const http = createServer(async (req, res) => {
  const url = req.url ?? "/";

  // A friendly root / health route for a human hitting the tunnel in a browser.
  if (url === "/" || url === "/healthz") {
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("rift MCP demo — POST JSON-RPC to /mcp (tools: echo, roll_dice, server_time)\n");
    return;
  }
  if (!url.startsWith("/mcp")) {
    res.writeHead(404).end("not found\n");
    return;
  }

  const sessionId = req.headers["mcp-session-id"] as string | undefined;

  try {
    if (req.method === "POST") {
      const body = await readJson(req);
      let transport = sessionId ? transports.get(sessionId) : undefined;

      if (!transport) {
        // A new session may only begin with an `initialize` request.
        if (!isInitializeRequest(body)) {
          fail(res, 400, "No valid session; send an initialize request first.");
          return;
        }
        transport = new StreamableHTTPServerTransport({
          sessionIdGenerator: () => randomUUID(),
          onsessioninitialized: (id) =>
            transports.set(id, transport as StreamableHTTPServerTransport),
        });
        transport.onclose = () => {
          if (transport?.sessionId) transports.delete(transport.sessionId);
        };
        await buildServer().connect(transport);
      }

      await transport.handleRequest(req, res, body);
      return;
    }

    // GET opens the server->client SSE stream; DELETE ends the session.
    if (req.method === "GET" || req.method === "DELETE") {
      const transport = sessionId ? transports.get(sessionId) : undefined;
      if (!transport) {
        fail(res, 400, "Unknown or missing mcp-session-id.");
        return;
      }
      await transport.handleRequest(req, res);
      return;
    }

    res.writeHead(405).end();
  } catch (err) {
    console.error("mcp request failed:", err);
    if (!res.headersSent) fail(res, 500, "internal error");
  }
});

http.listen(PORT, () => {
  console.log(`rift MCP demo  ->  http://localhost:${PORT}/mcp`);
  console.log(`expose it:       rift http ${PORT} mcp`);
});
