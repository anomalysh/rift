#!/usr/bin/env python3
"""A mock of the Linode API v4 for tools/e2e-provision.sh.

Stdlib only, no pip. It never talks to a real cloud; it stands in for
api.linode.com so the provisioner can be exercised without a token or a bill.

What it models
--------------
* Bearer auth: every /v4/... request needs `Authorization: Bearer <token>` with
  a non-empty token, else 401. This is how the e2e proves the token is sent.
* POST   /v4/linode/instances        create; validates the body; status
                                     "provisioning"; assigns an id.
* GET    /v4/linode/instances/{id}   "provisioning" for the first N polls, then
                                     "running" with ipv4/ipv6. N is configurable
                                     so the e2e can prove the poll loop polls.
* DELETE /v4/linode/instances/{id}   204 the first time, 404 after. GET on a
                                     destroyed id: 404.
* GET    /v4/linode/instances        list (data[]).

Every /v4 request is appended to an in-memory JSON-lines log so the e2e asserts
against *observed HTTP traffic*, not the script's stdout.

Control endpoints (not authenticated, never logged), used only by the harness:
* GET  /_mock/requests   the request log, one JSON object per line.
* POST /_mock/reset      clear instances, log, and id counter; reset polls to
                         the env default.
* POST /_mock/config     JSON body {"provisioning_polls": N} to change the poll
                         count at runtime (so one running mock can serve the
                         "reaches running" and "never running" cases).

Configuration (environment):
    RIFT_MOCK_PORT                 listen port                 (default 8080)
    RIFT_MOCK_PROVISIONING_POLLS   provisioning polls before running (default 2)
    RIFT_MOCK_IPV4                 ipv4 reported once running  (default 127.0.0.1)
    RIFT_MOCK_IPV6                 ipv6 reported once running  (default 2001:db8::1/128)
    RIFT_MOCK_START_ID             first instance id           (default 1000)
"""

import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("RIFT_MOCK_PORT", "8080"))
DEFAULT_POLLS = int(os.environ.get("RIFT_MOCK_PROVISIONING_POLLS", "2"))
IPV4 = os.environ.get("RIFT_MOCK_IPV4", "127.0.0.1")
IPV6 = os.environ.get("RIFT_MOCK_IPV6", "2001:db8::1/128")
START_ID = int(os.environ.get("RIFT_MOCK_START_ID", "1000"))

_REQUIRED_CREATE_FIELDS = ("label", "region", "type", "image", "authorized_keys")

_lock = threading.Lock()
_state = {
    "instances": {},   # id -> instance dict
    "log": [],         # list of request records
    "next_id": START_ID,
    "polls": DEFAULT_POLLS,
}


def _reset():
    _state["instances"] = {}
    _state["log"] = []
    _state["next_id"] = START_ID
    _state["polls"] = DEFAULT_POLLS


def _status_for(inst):
    # provisioning for the first N polls, running thereafter.
    return "running" if inst["polls"] > _state["polls"] else "provisioning"


def _redact(raw):
    # Never persist a received root_pass, even in a throwaway mock's log.
    try:
        obj = json.loads(raw)
    except Exception:
        return raw[:2000]
    if isinstance(obj, dict) and "root_pass" in obj:
        obj["root_pass"] = "REDACTED"
    return json.dumps(obj, separators=(",", ":"))


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # silence per-request stderr noise
        pass

    # --- helpers ------------------------------------------------------------
    def _read_body(self):
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def _send_json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _send_text(self, code, text):
        body = text.encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _has_auth(self):
        auth = self.headers.get("Authorization", "")
        if not auth.startswith("Bearer "):
            return False
        return bool(auth[len("Bearer "):].strip())

    def _record(self, path, code, has_auth, body):
        rec = {
            "ts": time.time(),
            "method": self.command,
            "path": path,
            "status": code,
            "auth": has_auth,
        }
        if body is not None:
            rec["body"] = _redact(body.decode("utf-8", "replace"))
        _state["log"].append(rec)

    # --- control plane (unauthenticated, never logged) ----------------------
    def _control(self, path):
        if self.command == "GET" and path == "/_mock/requests":
            with _lock:
                lines = "\n".join(json.dumps(r) for r in _state["log"])
            self._send_text(200, lines + ("\n" if lines else ""))
            return True
        if self.command == "POST" and path == "/_mock/reset":
            self._read_body()
            with _lock:
                _reset()
            self._send_json(200, {"ok": True})
            return True
        if self.command == "POST" and path == "/_mock/config":
            raw = self._read_body()
            try:
                cfg = json.loads(raw or b"{}")
                with _lock:
                    if "provisioning_polls" in cfg:
                        _state["polls"] = int(cfg["provisioning_polls"])
                self._send_json(200, {"ok": True, "provisioning_polls": _state["polls"]})
            except Exception as exc:
                self._send_json(400, {"error": str(exc)})
            return True
        return False

    # --- routing ------------------------------------------------------------
    def do_GET(self):  # noqa: N802
        if self._control(self.path):
            return
        self._api()

    def do_POST(self):  # noqa: N802
        if self._control(self.path):
            return
        self._api()

    def do_DELETE(self):  # noqa: N802
        self._api()

    def _api(self):
        path = self.path
        body = self._read_body() if self.command == "POST" else None
        has_auth = self._has_auth()

        # Auth gate first, so a missing token is a logged 401.
        if not has_auth:
            with _lock:
                self._record(path, 401, has_auth, body)
            self._send_json(401, {"errors": [{"reason": "Invalid Token"}]})
            return

        with _lock:
            code, resp = self._dispatch(path, body)
            self._record(path, code, has_auth, body)
        self._send_json(code, resp)

    def _dispatch(self, path, body):
        instances = _state["instances"]

        if path == "/v4/linode/instances":
            if self.command == "POST":
                return self._create(body)
            if self.command == "GET":
                data = [self._view(i) for i in instances.values() if not i["destroyed"]]
                return 200, {"data": data, "page": 1, "pages": 1, "results": len(data)}
            return 405, {"errors": [{"reason": "method not allowed"}]}

        prefix = "/v4/linode/instances/"
        if path.startswith(prefix):
            key = path[len(prefix):]
            inst = instances.get(key)
            if self.command == "GET":
                if not inst or inst["destroyed"]:
                    return 404, {"errors": [{"reason": "Not found"}]}
                inst["polls"] += 1
                return 200, self._view(inst)
            if self.command == "DELETE":
                if not inst or inst["destroyed"]:
                    return 404, {"errors": [{"reason": "Not found"}]}
                inst["destroyed"] = True
                return 204, {}
            return 405, {"errors": [{"reason": "method not allowed"}]}

        return 404, {"errors": [{"reason": "Not found"}]}

    def _create(self, body):
        try:
            payload = json.loads(body or b"{}")
        except Exception:
            return 400, {"errors": [{"reason": "invalid JSON body"}]}
        missing = [f for f in _REQUIRED_CREATE_FIELDS if not payload.get(f)]
        if missing:
            return 400, {"errors": [{"reason": "missing field", "field": ",".join(missing)}]}

        inst_id = str(_state["next_id"])
        _state["next_id"] += 1
        inst = {
            "id": inst_id,
            "label": payload["label"],
            "region": payload["region"],
            "type": payload["type"],
            "image": payload["image"],
            "polls": 0,
            "destroyed": False,
        }
        _state["instances"][inst_id] = inst
        # Create returns provisioning with no address yet, mirroring Linode.
        resp = self._view(inst)
        resp["status"] = "provisioning"
        resp["ipv4"] = []
        resp["ipv6"] = None
        return 200, resp

    def _view(self, inst):
        status = _status_for(inst)
        running = status == "running"
        return {
            "id": int(inst["id"]) if inst["id"].isdigit() else inst["id"],
            "label": inst["label"],
            "region": inst["region"],
            "type": inst["type"],
            "image": inst["image"],
            "status": status,
            "ipv4": [IPV4] if running else [],
            "ipv6": IPV6 if running else None,
        }


def main():
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    server.daemon_threads = True
    print(
        f"mock linode api on 0.0.0.0:{PORT} "
        f"(provisioning_polls={DEFAULT_POLLS}, ipv4={IPV4})",
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
