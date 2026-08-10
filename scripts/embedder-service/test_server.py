"""Tests for the embedding service, paying D-24.

D-24 named exactly what the two existing mitigations cannot see: a bug in `to_sparse` or in the
handler that changes neither the norm of the dense vector nor the emptiness of the sparse half. The
startup self-check only asserts those two properties, and the Go golden fixture compares cosine
against a pinned dense vector -- so reversing the sort in `to_sparse` passes both and corrupts the
sparse vector written to Qdrant, which degrades hybrid retrieval and reports nothing.

D-24 and two comments on the Go side described that consequence as destabilising `point_hash`, and
that is wrong: `ComputePointHash` takes note metadata, the chunk text, the pipeline config and the
seven handshake fields, and `embedder_sparse_params` among them is the static {kind, id_space} the
handshake declares -- no per-chunk index or weight reaches the hash. The bug is silent either way,
which is why nobody noticed the reason was wrong.

The correction moves the target rather than removing it. What actually feeds `point_hash` is
`/handshake`, so that endpoint is tested here too, along with the startup self-check whose own logic
was untested while the debt entry leaned on it as a mitigation, plus the two remaining things the
debt names: the window guard and request validation.

No test loads the real model. `load_model` is the only writer of `server._model`, and every code
path under test reads that global, so substituting a stub is enough -- which is what keeps this
suite runnable without a GPU and without the 2.3 GB of weights. Importing `server` still needs the
virtualenv that has torch and FlagEmbedding, because those imports are at module scope; see the
`test-embedder` target in the Makefile for how that is kept out of CI's way.

PriorityTest pays a different debt, D-27: the service used to serialize every request first-come
-first-served, so an interactive query queued behind every ingestion batch that happened to be
waiting. Those tests assert an ordering, and an ordering test that depends on timing is a test that
lies on a loaded machine -- so the stub blocks inside `encode` on an event the test controls, and
the test only ever advances on a predicate about real state (a request was admitted; N requests are
parked). No test there sleeps to make an order happen.

The handler tests drive a real ThreadingHTTPServer on an ephemeral port rather than calling
`do_POST` directly. BaseHTTPRequestHandler parses the request and writes the response inside
`__init__`, so calling the methods directly means building a fake socket, a fake request line and
then parsing the raw response bytes back out -- more code than starting the server, and it would
exercise a request pipeline that is not the one production uses. Going through http.client also
makes the assertions be about status codes and JSON bodies, which is what the Go client sees.
"""

import json
import threading
import time
import unittest
from unittest import mock
from http.client import HTTPConnection
from http.server import ThreadingHTTPServer
from types import SimpleNamespace

import numpy as np

import server


class StubModel:
    """The three things the handlers touch on `_model`, and nothing else.

    Token counts are one per whitespace-separated word plus two special tokens. That is not BGE-M3's
    tokenization and is not trying to be: these tests are about the guard around the count, so the
    count has to be something a test can state exactly.
    """

    def __init__(self, window=8, weights=None, hidden_size=None):
        # hidden_size defaults to something other than window so a test that forgets to set it
        # explicitly still fails loudly if dense_dim and max_position_embeddings ever get swapped.
        hidden_size = window + 1000 if hidden_size is None else hidden_size
        self.model = SimpleNamespace(
            model=SimpleNamespace(
                config=SimpleNamespace(max_position_embeddings=window, hidden_size=hidden_size),
                # handshake() reads dtype off the first parameter; a plain string stands in for a
                # torch dtype because str() of a string is the string itself.
                parameters=lambda: iter([SimpleNamespace(dtype="torch.float16")]),
            )
        )
        self._weights = {"7": 0.5, "2": 0.25} if weights is None else weights

    def tokenizer(self, inputs, add_special_tokens=True):
        return {"input_ids": [["t"] * (len(s.split()) + 2) for s in inputs]}

    def encode(self, inputs, **kwargs):
        return {
            "dense_vecs": [[0.1, 0.2] for _ in inputs],
            "lexical_weights": [dict(self._weights) for _ in inputs],
        }


class ToSparseTest(unittest.TestCase):
    def test_pairs_are_sorted_by_token_id_numerically(self):
        # The one D-24 names. Ids arrive as strings from FlagEmbedding, so this fails both for a
        # reversed sort and for a lexicographic one ("10" < "100" < "9"), and the paired values
        # assertion fails if indices and values ever stop being the same permutation.
        out = server.to_sparse({"10": 0.1, "9": 0.2, "100": 0.3})
        self.assertEqual(out["indices"], [9, 10, 100])
        self.assertEqual(out["values"], [0.2, 0.1, 0.3])

    def test_drops_zero_and_negative_weights(self):
        out = server.to_sparse({"1": 0.0, "2": -0.5, "3": 0.25})
        self.assertEqual(out, {"indices": [3], "values": [0.25]})

    def test_converts_ids_to_int_and_weights_to_float(self):
        # The contract's types. Without the conversions the id would serialize as a JSON string and
        # the weight as a JSON integer, neither of which the Go side will decode.
        out = server.to_sparse({"4": 1})
        self.assertIsInstance(out["indices"][0], int)
        self.assertIsInstance(out["values"][0], float)

    def test_empty_weights_give_empty_lists(self):
        self.assertEqual(server.to_sparse({}), {"indices": [], "values": []})


class ProbeCheckTest(unittest.TestCase):
    """server.check_probe is the startup self-check, extracted so it can run without a GPU.

    In production `probe["dense_vecs"][0]` is a numpy array -- `(x ** 2).sum() ** 0.5` is numpy
    arithmetic, not list arithmetic -- so these use real numpy arrays rather than plain lists,
    which do not support `**` or `.sum()` and would let a broken norm computation pass silently.
    """

    def _probe(self, dense, sparse):
        return {"dense_vecs": [np.array(dense)], "lexical_weights": [sparse]}

    def test_good_probe_passes(self):
        server.check_probe(self._probe([1.0, 0.0], {"7": 0.5}))  # must not raise

    def test_non_normalized_dense_raises(self):
        with self.assertRaisesRegex(SystemExit, "not normalized"):
            server.check_probe(self._probe([2.0, 0.0], {"7": 0.5}))

    def test_empty_sparse_raises(self):
        with self.assertRaisesRegex(SystemExit, "sparse weights came back empty"):
            server.check_probe(self._probe([1.0, 0.0], {}))

    def test_norm_just_inside_tolerance_passes(self):
        server.check_probe(self._probe([1.0009, 0.0], {"7": 0.5}))  # 9e-4 < 1e-3

    def test_norm_just_outside_tolerance_raises(self):
        with self.assertRaisesRegex(SystemExit, "not normalized"):
            server.check_probe(self._probe([1.0011, 0.0], {"7": 0.5}))  # 1.1e-3 > 1e-3


class ServiceTestCase(unittest.TestCase):
    """One server for the whole run; each test installs the stub it wants.

    Resetting the module-level `server._model` global in setUp() is only safe because tests run
    sequentially in a single process -- true today because the `test-embedder` Makefile target
    runs plain `unittest`, not a parallel runner, but not enforced by anything here.
    """

    @classmethod
    def setUpClass(cls):
        cls.httpd = ThreadingHTTPServer(("127.0.0.1", 0), server.Handler)
        cls.port = cls.httpd.server_address[1]
        cls.thread = threading.Thread(target=cls.httpd.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.httpd.shutdown()
        cls.thread.join()
        cls.httpd.server_close()

    def setUp(self):
        server._model = StubModel()

    def request(self, method, path, body=None):
        conn = HTTPConnection("127.0.0.1", self.port, timeout=5)
        try:
            conn.request(method, path, body=body)
            resp = conn.getresponse()
            return resp.status, json.loads(resp.read())
        finally:
            conn.close()


class WindowGuardTest(ServiceTestCase):
    def test_refuses_over_window_and_names_index_tokens_and_max(self):
        # The index matters: the guard has to check every input, not just the first, because the
        # caller needs to know which chunk of a batch it has to split.
        server._model = StubModel(window=8)
        status, body = self.request(
            "POST", "/embed", json.dumps({"inputs": ["one two", "a b c d e f g h i"]})
        )
        self.assertEqual(status, 400)
        self.assertEqual(body["index"], 1)
        self.assertEqual(body["tokens"], 11)
        self.assertEqual(body["max_tokens"], 8)

    def test_accepts_input_exactly_at_the_window(self):
        # An off-by-one here refuses valid text, which is the opposite failure and just as silent:
        # the chunker sizes chunks to exactly this limit.
        server._model = StubModel(window=8)
        status, body = self.request("POST", "/embed", json.dumps({"inputs": ["a b c d e f"]}))
        self.assertEqual(status, 200)
        self.assertEqual(body["tokens"], [8])

    def test_embed_response_shape(self):
        # The sparse half comes back sorted through the handler too, not only through to_sparse
        # directly, because the response is what point_hash actually consumes.
        server._model = StubModel(window=8, weights={"9": 0.5, "3": 0.25, "0": 0.0})
        status, body = self.request("POST", "/embed", json.dumps({"inputs": ["a b"]}))
        self.assertEqual(status, 200)
        self.assertEqual(body["dense"], [[0.1, 0.2]])
        self.assertEqual(body["sparse"], [{"indices": [3, 9], "values": [0.25, 0.5]}])
        self.assertEqual(body["tokens"], [4])


class HandshakeTest(ServiceTestCase):
    def test_handshake_fields(self):
        # dense_dim and max_position_embeddings are both plain ints read off the same config
        # object -- a transposition between them returns a normal 200 with swapped values and
        # nothing raises, so the two stub values are deliberately distinct (not the fixture's
        # shared default) and the whole dict is compared, not just the fields under test.
        server._model = StubModel(window=555, hidden_size=999)
        status, body = self.request("GET", "/handshake")
        self.assertEqual(status, 200)
        self.assertEqual(body, {
            "model_id": server.MODEL_ID,
            "model_revision": server.REVISION,
            "tokenizer_revision": server.REVISION,
            "dense_dim": 999,
            "normalized": True,
            "pooling": "cls",
            "precision": "float16",  # derived from the parameter dtype, "torch." stripped
            "sparse": {"kind": "lexical_weights", "id_space": "tokenizer_vocab"},
            "max_position_embeddings": 555,
        })


class ValidationTest(ServiceTestCase):
    def test_invalid_json(self):
        status, body = self.request("POST", "/embed", "{not json")
        self.assertEqual(status, 400)
        self.assertIn("invalid JSON", body["error"])

    def test_rejects_malformed_inputs(self):
        # A loosened check does not raise here -- it lets the request through to the tokenizer,
        # which answers 500 or, worse, 200 -- so the assertion is on the 400, not on an exception.
        for name, payload in [
            ("missing", "{}"),
            ("not a list", '{"inputs": "hello"}'),
            ("empty list", '{"inputs": []}'),
            ("non-string element", '{"inputs": ["ok", 7]}'),
        ]:
            with self.subTest(name):
                status, body = self.request("POST", "/embed", payload)
                self.assertEqual(status, 400)
                self.assertIn("non-empty array of strings", body["error"])

    def test_tokenize_counts(self):
        status, body = self.request("POST", "/tokenize", json.dumps({"inputs": ["a b", "c"]}))
        self.assertEqual(status, 200)
        self.assertEqual(body["counts"], [4, 3])

    def test_unknown_endpoints(self):
        for method, body in [("GET", None), ("POST", '{"inputs": ["a"]}')]:
            with self.subTest(method):
                status, payload = self.request(method, "/nope", body)
                self.assertEqual(status, 404)
                self.assertEqual(payload["path"], "/nope")

    def test_health_reports_loaded_honestly(self):
        # `loaded` is read from the global rather than hardcoded, which is the only reason it can
        # ever say false -- a health check that always answers ok is not a health check.
        status, body = self.request("GET", "/health")
        self.assertEqual((status, body), (200, {"status": "ok", "loaded": True}))

        server._model = None
        status, body = self.request("GET", "/health")
        self.assertEqual((status, body), (200, {"status": "ok", "loaded": False}))


class GatedModel(StubModel):
    """A stub that holds the GPU until the test lets go, and records who got it, in order.

    `admitted` is appended in `tokenizer`, not in `encode`, because the handlers call the tokenizer
    as the first statement inside their critical section -- `/tokenize` has nothing else, and
    `/embed` tokenizes and encodes under one acquisition. So one entry per request, written at the
    instant that request was admitted, which is exactly the order under test.
    """

    def __init__(self, **kwargs):
        super().__init__(**kwargs)
        self.admitted = []  # list.append is atomic under the GIL, and every caller holds the GPU
        self.gate = threading.Event()

    def tokenizer(self, inputs, add_special_tokens=True):
        self.admitted.append(inputs[0])
        return super().tokenizer(inputs, add_special_tokens)

    def encode(self, inputs, **kwargs):
        # Blocks the one call that is executing, which is how a test creates the "in flight" state
        # the whole design is about. The timeout is a backstop so a broken server fails the suite
        # instead of hanging it.
        if not self.gate.wait(timeout=10):
            raise AssertionError("the test never opened the gate")
        return super().encode(inputs, **kwargs)


class PriorityTest(ServiceTestCase):
    """D-27: a query is admitted ahead of queued batches, and waits only for the one in flight.

    Every request in these tests carries a single one-word input, batches included. That is
    deliberate: if the class came from a size heuristic instead of the `kind` field, none of these
    orderings would happen, and an ingestion's last sub-batch really can be one chunk.
    """

    def setUp(self):
        self.model = GatedModel()
        server._model = self.model
        self.threads = []
        self.failures = []

    def tearDown(self):
        self.model.gate.set()
        for t in self.threads:
            t.join(timeout=10)
            self.assertFalse(t.is_alive(), "a request never finished")
        self.assertEqual(self.failures, [])

    def restart(self):
        """Fresh model and fresh threads between subTests: each one asserts a whole ordering."""
        self.tearDown()
        self.setUp()

    def send(self, path, payload):
        """Fire a request in the background; its failure, if any, surfaces in tearDown."""
        def run():
            try:
                self.request("POST", path, json.dumps(payload))
            except Exception as e:  # noqa: BLE001 - reported, never swallowed
                self.failures.append(e)

        t = threading.Thread(target=run, daemon=True)
        t.start()
        self.threads.append(t)

    def until(self, pred, what, timeout=5):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if pred():
                return
            time.sleep(0.001)
        self.fail(f"timed out waiting for {what}; admitted={self.model.admitted}")

    def hold_the_gpu(self, label="A"):
        """Get one batch executing and blocked, which is the state a query has to wait out."""
        self.send("/embed", {"inputs": [label], "kind": "passage"})
        self.until(lambda: self.model.admitted == [label], f"{label} to reach the GPU")

    def until_parked(self, n):
        """Wait until n requests are queued inside server.gpu().

        threading.Condition parks its waiters in `_waiters`, and reading that length is what makes
        these tests deterministic: the alternative is sleeping and hoping the queue formed, which
        is the flakiness this file's docstring refuses. Private, and stable in CPython since the
        module was written.
        """
        self.until(lambda: len(server._gpu._waiters) == n, f"{n} request(s) queued")

    def test_query_overtakes_queued_batches_however_many_there_are(self):
        # The D-27 acceptance criterion. 1, 2 and 4 are the queue depths the debt entry measured:
        # under the old lock the query came out last, and its wait grew with the depth. The
        # assertion is that the query is second no matter the depth -- the wait stops depending on
        # the ingestion profile's MaxConcurrent, which is the variable nobody watches.
        for queued in (1, 2, 4):
            with self.subTest(queued=queued):
                self.restart()
                self.hold_the_gpu()
                for i in range(queued):
                    self.send("/embed", {"inputs": [f"B{i}"], "kind": "passage"})
                self.until_parked(queued)

                self.send("/embed", {"inputs": ["Q"], "kind": "query"})
                self.until_parked(queued + 1)

                self.model.gate.set()
                self.until(lambda: len(self.model.admitted) == queued + 2, "every request to finish")
                self.assertEqual(self.model.admitted[:2], ["A", "Q"])
                self.assertEqual(sorted(self.model.admitted[2:]), [f"B{i}" for i in range(queued)])

    def test_only_one_request_holds_the_model_at_a_time(self):
        # Priority must not be bought by letting two calls into the model: one CUDA model is not
        # safe to call concurrently, and a query that overtakes a batch by running *beside* it
        # would corrupt both. The query is parked, not admitted, while the batch is in flight.
        self.hold_the_gpu()
        self.send("/embed", {"inputs": ["Q"], "kind": "query"})
        self.until_parked(1)
        self.assertEqual(self.model.admitted, ["A"])

        self.model.gate.set()
        self.until(lambda: len(self.model.admitted) == 2, "the query to be admitted")
        self.assertEqual(self.model.admitted, ["A", "Q"])

    def test_tokenize_waits_behind_a_query(self):
        # /tokenize is ingestion's, not the query path's: the chunker calls it thousands of times
        # per run and nothing on the search path calls it at all. So it is in the batch class, and
        # a query queued after it still goes first.
        self.hold_the_gpu()
        self.send("/tokenize", {"inputs": ["T"]})
        self.until_parked(1)
        self.send("/embed", {"inputs": ["Q"], "kind": "query"})
        self.until_parked(2)

        self.model.gate.set()
        self.until(lambda: len(self.model.admitted) == 3, "every request to finish")
        self.assertEqual(self.model.admitted, ["A", "Q", "T"])

    def test_a_failed_admission_does_not_wedge_the_service(self):
        # No raise reaches gpu()'s wait today, which is why the guard needs a test rather than an
        # argument: `_queries_waiting` has no other code path that can clear it, so leaking one
        # parks every batch-class request forever while /health goes on answering ok. The raise is
        # injected because the real ones are hypothetical -- a wait timeout, asyncio, cancellation.
        self.hold_the_gpu()
        with mock.patch.object(server._gpu, "wait", side_effect=RuntimeError("interrupted")):
            with self.assertRaises(RuntimeError):
                with server.gpu(priority=True):
                    self.fail("the body must not run when the admission failed")
        self.assertEqual(server._queries_waiting, 0)

        self.model.gate.set()
        status, _ = self.request("POST", "/embed", json.dumps({"inputs": ["B"], "kind": "passage"}))
        self.assertEqual(status, 200)

    def test_only_an_exact_kind_query_is_interactive(self):
        # The class is read off `kind`, so what does *not* count matters as much as what does: a
        # caller that omits the field, sends a passage, or misspells the value must not be able to
        # push ingestion aside. Asserted on the waiting-query count rather than on an order,
        # because two requests of the same class have no defined order between them -- an
        # ordering assertion here would pass or fail on the scheduler.
        self.hold_the_gpu()
        for payload in ({"inputs": ["x"]},
                        {"inputs": ["x"], "kind": "passage"},
                        {"inputs": ["x"], "kind": "Query"}):
            self.send("/embed", payload)
        self.until_parked(3)
        self.assertEqual(server._queries_waiting, 0)

        self.send("/embed", {"inputs": ["Q"], "kind": "query"})
        self.until_parked(4)
        self.assertEqual(server._queries_waiting, 1)


if __name__ == "__main__":
    unittest.main()
