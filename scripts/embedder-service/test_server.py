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

The handler tests drive a real ThreadingHTTPServer on an ephemeral port rather than calling
`do_POST` directly. BaseHTTPRequestHandler parses the request and writes the response inside
`__init__`, so calling the methods directly means building a fake socket, a fake request line and
then parsing the raw response bytes back out -- more code than starting the server, and it would
exercise a request pipeline that is not the one production uses. Going through http.client also
makes the assertions be about status codes and JSON bodies, which is what the Go client sees.
"""

import json
import threading
import unittest
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


if __name__ == "__main__":
    unittest.main()
