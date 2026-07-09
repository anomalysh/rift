#!/usr/bin/env python3
"""A local service for the rift e2e harness to expose through a tunnel.

Stdlib only, so the harness needs no runtime beyond python3.

Routes:
    GET  /            -> echoes method and path, sets X-Upstream
    POST /echo        -> streams the request body straight back
    GET  /stream      -> two chunks, 300ms apart, to prove nothing buffers
    GET  /big         -> N bytes of deterministic filler (?n=BYTES)
"""

import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

CHUNK_GAP_SECONDS = 0.3
READ_CHUNK = 64 * 1024


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # noqa: A003 - silence per-request noise
        pass

    def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler's naming
        url = urlparse(self.path)

        if url.path == "/stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            self._chunk(b"chunk-1\n")
            time.sleep(CHUNK_GAP_SECONDS)
            self._chunk(b"chunk-2\n")
            self._chunk(b"")
            return

        if url.path == "/big":
            n = int(parse_qs(url.query).get("n", ["1048576"])[0])
            # Deterministic, so the caller can verify the body without a hash of
            # random data crossing the process boundary.
            body = (b"rift" * ((n // 4) + 1))[:n]
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        body = f"local app saw {self.command} {self.path}".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("X-Upstream", "yes")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):  # noqa: N802
        url = urlparse(self.path)
        length = int(self.headers.get("Content-Length") or 0)

        if url.path != "/echo":
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(length))
        self.send_header("X-Upstream", "echo")
        self.end_headers()

        remaining = length
        while remaining > 0:
            data = self.rfile.read(min(READ_CHUNK, remaining))
            if not data:
                break
            self.wfile.write(data)
            remaining -= len(data)

    def _chunk(self, data: bytes) -> None:
        self.wfile.write(b"%x\r\n%s\r\n" % (len(data), data))
        self.wfile.flush()


def main() -> int:
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 13099
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    server.daemon_threads = True
    print(f"upstream listening on 127.0.0.1:{port}", file=sys.stderr, flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
