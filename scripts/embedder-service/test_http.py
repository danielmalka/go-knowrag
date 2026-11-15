"""The HTTP shell and the process entry point, over a real ThreadingHTTPServer and a fake model.

Going through http.client (not calling the handler methods) makes the assertions about the status
codes and JSON bodies the Go client sees, and exercises the request pipeline production uses.
"""

import json
import socket
import sys
import threading
import types
from http.client import HTTPConnection
from http.server import ThreadingHTTPServer

import pytest

import server
from fakes import Wiring
from settings import EXIT_UNSUPPORTED, UnsupportedSetting


@pytest.fixture
def served():
    w = Wiring()
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), server.make_handler(w.app))
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    w.port = httpd.server_address[1]
    yield w
    w.model.hold.set()
    httpd.shutdown()
    thread.join(timeout=5)
    httpd.server_close()


def request(w: Wiring, method: str, path: str, body: str | None = None):
    conn = HTTPConnection("127.0.0.1", w.port, timeout=10)
    try:
        conn.request(method, path, body=body)
        resp = conn.getresponse()
        return resp.status, json.loads(resp.read())
    finally:
        conn.close()


def post(w: Wiring, path: str, payload) -> tuple[int, dict]:
    return request(w, "POST", path, json.dumps(payload))


def test_embed_answers_the_go_clients_request_shape(served):
    status, body = post(served, "/embed", {"inputs": ["a"], "kind": "passage"})
    assert status == 200 and set(body) == {"dense", "tokens"}


def test_embed_with_titles_reaches_the_model_with_the_title(served):
    post(served, "/embed", {"inputs": ["corpo"], "kind": "passage", "titles": ["Plano"]})
    assert served.model.encoded == [["title: Plano | text: corpo"]]


def test_invalid_json_and_a_non_object_body_are_400(served):
    status, body = request(served, "POST", "/embed", "{not json")
    assert status == 400 and "invalid JSON" in body["error"]
    status, body = request(served, "POST", "/embed", "[1]")
    assert status == 400 and "JSON object" in body["error"]


def test_unknown_paths_are_404_for_both_methods(served):
    for method, payload in [("GET", None), ("POST", '{"inputs": ["a"]}')]:
        status, body = request(served, method, "/nope", payload)
        assert (status, body["path"]) == (404, "/nope")


def test_handshake_and_health_do_not_load_the_model(served):
    assert request(served, "GET", "/handshake")[0] == 200
    assert request(served, "GET", "/health") == (200, {"status": "ok", "loaded": False, "cuda": True})
    assert served.loads == 0


def test_load_endpoint_loads_and_health_then_says_so(served):
    assert post(served, "/load", {}) == (200, {"loaded": True})
    assert request(served, "GET", "/health")[1]["loaded"] is True


def test_a_model_failure_is_a_500_that_names_the_error_not_a_hang(served):
    def boom(texts, batch_size):
        raise RuntimeError("CUDA out of memory")

    served.model.encode = boom
    status, body = post(served, "/embed", {"inputs": ["a"], "kind": "passage"})
    assert status == 500 and "CUDA out of memory" in body["error"]
    assert request(served, "GET", "/health")[0] == 200  # the process is still serving


def test_a_query_issued_during_an_ingestion_is_admitted_ahead_of_queued_batches(served):
    """D-27 end to end: the `kind` field is what makes a request interactive."""
    served.model.hold.clear()
    results = []

    def send(label: str, kind: str) -> None:
        results.append(post(served, "/embed", {"inputs": [label], "kind": kind})[0])

    def until(predicate, what: str) -> None:
        for _ in range(5000):
            if predicate():
                return
            threading.Event().wait(0.001)
        pytest.fail(f"timed out waiting for {what}; counted={served.model.counted}")

    threads = [threading.Thread(target=send, args=("A", "passage"), daemon=True)]
    threads[0].start()
    until(lambda: len(served.model.counted) == 1, "A to reach the model")
    for label, kind in [("B1", "passage"), ("B2", "passage"), ("Q", "query")]:
        t = threading.Thread(target=send, args=(label, kind), daemon=True)
        threads.append(t)
        t.start()
        until(lambda: len(served.gate._cond._waiters) == len(threads) - 1, f"{label} to queue")

    served.model.hold.set()
    for t in threads:
        t.join(timeout=10)
    order = [s.split(" | ")[-1].removeprefix("text: ").removeprefix("query: ") for s in served.model.counted]
    assert order[:2] == ["A", "Q"] and sorted(order[2:]) == ["B1", "B2"]
    assert results == [200] * 4


# --- the entry point --------------------------------------------------------------------------


def test_main_refuses_fp16_with_the_unsupported_exit_status_and_a_clear_message(capsys):
    with pytest.raises(SystemExit) as e:
        server.main(["--precision", "fp16"])
    assert e.value.code == EXIT_UNSUPPORTED
    assert "float16" in capsys.readouterr().err


def test_main_refuses_a_cpu_device(capsys):
    with pytest.raises(SystemExit) as e:
        server.main(["--device", "cpu"])
    assert e.value.code == EXIT_UNSUPPORTED
    assert "no CPU fallback" in capsys.readouterr().err


def test_main_refuses_to_start_without_a_gpu_instead_of_falling_back(monkeypatch, capsys):
    def no_gpu(device):
        raise UnsupportedSetting("no CUDA device is available to this process")

    monkeypatch.setitem(sys.modules, "st_adapter", types.SimpleNamespace(require_cuda=no_gpu))
    with pytest.raises(SystemExit) as e:
        server.main([])
    assert e.value.code == EXIT_UNSUPPORTED
    assert "no CUDA device" in capsys.readouterr().err


def test_main_refuses_to_bind_a_network_address_before_touching_anything(capsys):
    with pytest.raises(SystemExit) as e:
        server.main(["--host", "0.0.0.0"])
    assert e.value.code == EXIT_UNSUPPORTED
    err = capsys.readouterr().err
    assert "0.0.0.0" in err and "no authentication" in err


def test_ipv6_loopback_binds_an_ipv6_socket():
    try:
        httpd = server.LoopbackServer(("::1", 0), server.make_handler(Wiring().app))
    except OSError:
        pytest.skip("no IPv6 loopback on this machine")
    try:
        assert httpd.address_family == socket.AF_INET6
    finally:
        httpd.server_close()


@pytest.mark.parametrize("given,bound", [("localhost", "127.0.0.1"), ("127.0.0.1", "127.0.0.1"), ("::1", "::1")])
def test_the_bind_only_ever_receives_a_literal_never_a_name(monkeypatch, given, bound):
    seen = []

    class Recorder:
        def __init__(self, address, handler):
            seen.append(address)

        def serve_forever(self):
            raise KeyboardInterrupt

    stub = types.SimpleNamespace(require_cuda=lambda device: None, cuda_available=lambda: True)
    monkeypatch.setitem(sys.modules, "st_adapter", stub)
    monkeypatch.setattr(server, "build_app", lambda settings: (Wiring().app, Wiring().host))
    monkeypatch.setattr(server, "LoopbackServer", Recorder)
    with pytest.raises(KeyboardInterrupt):
        server.main(["--host", given, "--port", "17997"])
    assert seen == [(bound, 17997)]


@pytest.mark.parametrize("name", ["localhost", "example.com", "0.0.0.0", "::ffff:127.0.0.1", ""])
def test_the_server_itself_refuses_a_name_or_a_non_loopback_address(name):
    with pytest.raises(ValueError, match="never a name"):
        server.LoopbackServer((name, 0), server.make_handler(Wiring().app))
