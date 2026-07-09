// A tiny Bun HTTP server that renders a "you're tunneling through rift" demo
// page. Run it, then expose it with the rift CLI and open the public URL.
//
//   bun run index.ts        # http://localhost:3000
//   rift http 3000 demo     # https://demo.<your-rift-domain>
//
// The page calls /api/info to show the X-Forwarded-* headers rift's gateway and
// Caddy set on the way in, so you can see the request really arrived through the
// tunnel (and over HTTPS at the edge) even though this server only speaks HTTP.

const PORT = Number(process.env.PORT ?? 3000);
const page = Bun.file(new URL("./public/index.html", import.meta.url));

const server = Bun.serve({
  port: PORT,
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
        return Response.json({
          youSent: await req.text(),
          at: new Date().toISOString(),
        });

      case "/healthz":
        return new Response("ok\n");

      default:
        return new Response("not found\n", { status: 404 });
    }
  },
});

console.log(`rift http-demo  ->  http://localhost:${server.port}`);
console.log(`expose it:       rift http ${server.port} demo`);
