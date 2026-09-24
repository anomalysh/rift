// A tiny Bun HTTP server that renders a "you're tunneling through rift" demo
// page. Run it, then expose it with the rift CLI and open the public URL.
//
//   bun run index.ts        # http://127.0.0.1:3000
//   rift http 3000 demo     # https://demo.<your-rift-domain>
//
// It listens on loopback only: rift reaches it there, and the public reaches it
// only through the tunnel (with whatever --basic-auth / --allow-ip you add),
// not directly over the LAN. Set HOST=0.0.0.0 to listen on every interface.
//
// The page calls /api/info to show the X-Forwarded-* headers rift's gateway and
// Caddy set on the way in, so you can see the request really arrived through the
// tunnel (and over HTTPS at the edge) even though this server only speaks HTTP.

const PORT = Number(process.env.PORT ?? 3000);
const HOST = process.env.HOST ?? "127.0.0.1";
// Bodies are only ever echoed back; anything larger is refused with 413 rather
// than buffered, since this server is meant to face the public internet.
const MAX_BODY_BYTES = 64 * 1024;
const page = Bun.file(new URL("./public/index.html", import.meta.url));

const server = Bun.serve({
  port: PORT,
  hostname: HOST,
  maxRequestBodySize: MAX_BODY_BYTES,
  async fetch(req) {
    const url = new URL(req.url);
    switch (url.pathname) {
      case "/":
        return new Response(page, {
          headers: { "content-type": "text/html; charset=utf-8" },
        });

      case "/api/info":
        return Response.json({
          host: req.headers.get("host"),
          forwardedFor: req.headers.get("x-forwarded-for"),
          forwardedProto: req.headers.get("x-forwarded-proto"),
          forwardedHost: req.headers.get("x-forwarded-host"),
          userAgent: req.headers.get("user-agent"),
          method: req.method,
        });

      case "/api/time":
        return Response.json({ now: new Date().toISOString() });

      case "/api/echo":
        if (req.method !== "POST") {
          return new Response("POST only\n", { status: 405 });
        }
        {
          const body = await readCapped(req, MAX_BODY_BYTES);
          if (body === null) {
            return new Response("body too large\n", { status: 413 });
          }
          return Response.json({
            youSent: body,
            at: new Date().toISOString(),
          });
        }

      case "/healthz":
        return new Response("ok\n");

      default:
        return new Response("not found\n", { status: 404 });
    }
  },
});

/**
 * Read a request body as text, or null once it exceeds `max` bytes. Checked
 * here as well as by maxRequestBodySize so a chunked body with no declared
 * length is bounded too.
 */
async function readCapped(req: Request, max: number): Promise<string | null> {
  if (Number(req.headers.get("content-length") ?? 0) > max) return null;
  if (req.body === null) return "";
  const reader = req.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.length;
    if (total > max) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  return new TextDecoder().decode(Buffer.concat(chunks));
}

const shownHost = HOST.includes(":") ? `[${HOST}]` : HOST;
console.log(`rift http-demo  ->  http://${shownHost}:${server.port}`);
console.log(`expose it:       rift http ${server.port} demo`);
