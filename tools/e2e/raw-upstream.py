#!/usr/bin/env python3
"""Raw upstreams for the rift tcp/tls tunnel e2e (tools/e2e.sh run_tunnels).

Stdlib only, matching upstream.py, so the harness needs no runtime beyond
python3.

    raw-upstream.py tcp <port>                      -- a TCP echo server
    raw-upstream.py tls <port> <certfile> <keyfile> -- a TLS server that reads a
                                                       line and replies with a
                                                       fixed banner

The tcp echo proves a raw byte stream survives the tunnel intact. The tls server
holds its own certificate and terminates TLS itself, which is the whole point of
`rift tls`: the gateway routes by SNI and never decrypts.
"""

import socket
import ssl
import sys
import threading

TLS_BANNER = b"tls-upstream-ok\n"


def _serve(port: int, handler) -> None:
    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", port))
    srv.listen(16)
    print(f"raw upstream listening on 127.0.0.1:{port}", file=sys.stderr, flush=True)
    while True:
        conn, _ = srv.accept()
        threading.Thread(target=handler, args=(conn,), daemon=True).start()


def _echo(conn: socket.socket) -> None:
    with conn:
        while True:
            data = conn.recv(4096)
            if not data:
                return
            conn.sendall(data)


def _tls_reply(ctx: ssl.SSLContext, raw: socket.socket):
    def handle(_conn: socket.socket) -> None:
        try:
            tls = ctx.wrap_socket(raw, server_side=True)
        except ssl.SSLError:
            raw.close()
            return
        with tls:
            tls.recv(4096)  # drain whatever the client sent
            tls.sendall(TLS_BANNER)

    return handle


def main() -> int:
    if len(sys.argv) < 3:
        print("usage: raw-upstream.py tcp <port> | tls <port> <cert> <key>", file=sys.stderr)
        return 2
    mode, port = sys.argv[1], int(sys.argv[2])
    if mode == "tcp":
        _serve(port, _echo)
    elif mode == "tls":
        cert, key = sys.argv[3], sys.argv[4]
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(cert, key)
        # The handler closure needs the raw socket, so wrap per-connection here.
        _serve(port, lambda raw: _tls_reply(ctx, raw)(raw))
    else:
        print(f"unknown mode {mode!r}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except KeyboardInterrupt:
        pass
