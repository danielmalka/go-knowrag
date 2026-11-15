"""Embedding service for go-knowrag: EmbeddingGemma 2 (text, dense 768) over HTTP, GPU only.

This file is the HTTP shell and the process entry point; the rules live in `app.py`, the model
lifecycle in `model_host.py`, the GPU ordering in `gpu_gate.py`, the prefixes in `prompts.py`, the
pins and tunables in `settings.py` and the real model in `st_adapter.py`. Run it with

    python server.py [--port 7999] [--precision bf16|fp32] [--idle-unload 600] ...

or with the `KNOWRAG_EMBEDDER_*` variables the systemd unit reads; the README lists them all.

The process starts without loading the model, fails at once (exit 78) when there is no CUDA device
or the settings ask for CPU or fp16, and from then on is always up: the model is loaded by the first
call that needs it and released after the idle time. See `model_host.py`.
"""

import json
import os
import socket
import sys
from collections.abc import Sequence
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from app import EmbedderApp, Reply
from gpu_gate import GpuGate
from model_host import IdleUnloader, ModelHost
from settings import EXIT_UNSUPPORTED, LOOPBACK_LITERALS, MODEL_ID, REVISION, Settings, UnsupportedSetting, load_settings

POST_ROUTES = ("/embed", "/tokenize", "/load")


class LoopbackServer(ThreadingHTTPServer):
    """ThreadingHTTPServer that binds only a loopback literal, IPv6 included for `::1`.

    The guard is at the bind itself, not only in settings: a name would be resolved by the system at
    bind time and could land on a non-loopback address, so anything but the two literals is refused
    here even if a future caller skips `parse_host`. The stdlib default family is IPv4, hence the
    switch for `::1`.
    """

    def __init__(self, address: tuple[str, int], handler: type[BaseHTTPRequestHandler]) -> None:
        if address[0] not in LOOPBACK_LITERALS:
            raise ValueError(f"refusing to bind {address[0]!r}: only {LOOPBACK_LITERALS} are allowed, never a name")
        if ":" in address[0]:
            self.address_family = socket.AF_INET6
        super().__init__(address, handler)


def make_handler(app: EmbedderApp) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        # TCP_NODELAY on every accepted connection. Without it a /tokenize answers in ~45 ms on a
        # reused connection and ~1.5 ms on a fresh one (D-25, where 94% of an ingestion run was
        # spent waiting here): the response leaves as two writes, Nagle holds the second until the
        # first is acknowledged, and Linux delays that ACK by 40 ms. Why a fresh connection escaped
        # it was not established; the fix does not depend on that.
        disable_nagle_algorithm = True

        def log_message(self, fmt, *args):  # noqa: A002 - stdlib signature
            pass  # the caller already has its own logs

        def _send(self, reply: Reply) -> None:
            body = json.dumps(reply.body).encode()
            self.send_response(reply.status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _read_json(self) -> dict | None:
            length = int(self.headers.get("Content-Length") or 0)
            try:
                value = json.loads(self.rfile.read(length) or b"{}")
            except json.JSONDecodeError as e:
                self._send(Reply(400, {"error": f"invalid JSON: {e}"}))
                return None
            if not isinstance(value, dict):
                self._send(Reply(400, {"error": "request body must be a JSON object"}))
                return None
            return value

        def do_GET(self):  # noqa: N802 - stdlib signature
            routes = {"/health": app.health, "/handshake": app.handshake}
            handler = routes.get(self.path)
            if handler is None:
                self._send(Reply(404, {"error": "no such endpoint", "path": self.path}))
                return
            self._send(handler())

        def do_POST(self):  # noqa: N802 - stdlib signature
            req = self._read_json()
            if req is None:
                return
            if self.path not in POST_ROUTES:
                self._send(Reply(404, {"error": "no such endpoint", "path": self.path}))
                return
            try:
                handler = {"/embed": app.embed, "/tokenize": app.tokenize}.get(self.path)
                self._send(handler(req) if handler else app.load())
            except Exception as e:  # noqa: BLE001 - the client must see the failure, not a hang
                self._send(Reply(500, {"error": f"{type(e).__name__}: {e}"}))

    return Handler


def build_app(settings: Settings) -> tuple[EmbedderApp, ModelHost]:
    import st_adapter  # torch and sentence-transformers are imported here, not at module scope

    host = ModelHost(
        loader=lambda: st_adapter.load_verified_model(settings),
        release=st_adapter.release_cuda_memory,
    )
    return EmbedderApp(settings, host, GpuGate(), st_adapter.cuda_available), host


def main(argv: Sequence[str] | None = None) -> None:
    try:
        settings = load_settings(sys.argv[1:] if argv is None else argv, os.environ)
        import st_adapter

        st_adapter.require_cuda(settings.device)
    except UnsupportedSetting as e:
        print(f"refusing to start: {e.message}", file=sys.stderr, flush=True)
        raise SystemExit(EXIT_UNSUPPORTED) from None

    app, host = build_app(settings)
    httpd = LoopbackServer((settings.host, settings.port), make_handler(app))
    if settings.idle_unload_seconds > 0:
        IdleUnloader(host, settings.idle_unload_seconds).start()
    print(
        f"{MODEL_ID}@{REVISION[:8]} {settings.precision} on {settings.device}, loaded on first use, "
        f"idle unload {settings.idle_unload_seconds:.0f}s; ready on http://{settings.host}:{settings.port}",
        flush=True,
    )
    httpd.serve_forever()


if __name__ == "__main__":
    main()
