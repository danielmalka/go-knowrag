"""Embedding service for go-knowrag: BGE-M3 dense + sparse over HTTP.

Why this file exists at all, since writing and maintaining a server is a cost the ADR counts
against a candidate (criterion 7): every off-the-shelf server was eliminated first.

  - TEI loads the ONNX graph, and BGE-M3's sparse head (`sparse_linear.pt`) was never exported into
    it. Not a version or a flag -- the weights are not in the graph it loads.
  - Infinity does not start on current dependencies: it imports `optimum.bettertransformer`, which
    `optimum` removed. Working around it means pinning a stale `optimum` forever, in the runtime the
    whole pipeline depends on.

Both are recorded in ADR-001 §6.2. FlagEmbedding is the reference implementation from the model's
own authors, where sparse is first-class -- so what is left to write is the thin HTTP shell around
it, which is this file.

Resident by design, not by preference: loading the model measured **~11 s** with the weights already
in the Hugging Face cache, which is the normal case for a restart. The **314.5 s** of the first run
is a different number and not this one -- it included downloading the 2.2 GB of weights, and only
happens on a machine that has never loaded the model. Both are recorded in ADR-001 §6.2, and the
systemd unit next to this file quotes the ~11 s. A process that loaded on demand would put eleven
seconds in front of the first query of the day, against a measured 71 ms p99 -- three orders of
magnitude. This process loads once at startup and stays up. It is never invoked per request.

Inference is serialized. One CUDA model is not safe to call concurrently, and the consumers do not
need it: query embedding is one at a time by nature, and ingestion sends batches. Serialized is not
the same as first-come-first-served, though -- see `gpu()` for why an interactive query is admitted
ahead of queued ingestion batches, and for what that costs ingestion.

Endpoints, all JSON:

  GET  /health     -> {"status": "ok", "loaded": bool}
  GET  /handshake  -> the seven fields ADR-001 §4 pins; S04 refuses to run if any disagrees
  POST /embed      -> {"inputs": [str], "kind": "query"|"passage"}
                      {"dense": [[float]], "sparse": [{"indices": [int], "values": [float]}],
                       "tokens": [int]}
  POST /tokenize   -> {"inputs": [str]} -> {"counts": [int]}

`/tokenize` exists because S03's clamp needs the real BGE-M3 token count and refuses to approximate:
counting words or characters over four makes the clamp diverge from what the model actually reads.
Serving it from the same process is what keeps that count and the count used at embed time the same
tokenizer, rather than two that can drift.

`/embed` returns `tokens` alongside the vectors for the same reason -- the caller that just embedded
should not have to ask again to learn the size of what it embedded.
"""

import argparse
import contextlib
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch
from FlagEmbedding import BGEM3FlagModel

# Pinned in PRD-contrato §2.3b. Changing it means reindexing all three collections, because the
# embedder's effective config is part of point_hash.
MODEL_ID = "BAAI/bge-m3"
REVISION = "5617a9f61b028005a4858fdac845db406aefb181"

_model = None

# The GPU, as a resource with two classes of waiter. `_gpu_busy` says a call is executing;
# `_queries_waiting` counts the interactive requests that arrived and have not been admitted yet.
_gpu = threading.Condition()
_gpu_busy = False
_queries_waiting = 0


@contextlib.contextmanager
def gpu(priority: bool):
    """Hold the model for one call. `priority` requests are admitted ahead of queued batches.

    A plain lock was the whole of D-27. It has no notion of who is waiting, so an interactive query
    issued during an ingestion queued behind every batch already in flight *and* every batch merely
    waiting. The wait grew with the ingestion profile's MaxConcurrent, a variable nothing was
    watching, while the timeout guarding it stayed a constant measured once. Here a batch yields to
    any query that is already waiting.

    The bound that buys, stated exactly, because it is the property the next edit has to preserve:
    a query waits for the call in flight when it registers, plus at most one batch that won the
    "the GPU just freed" race in the window before the increment below landed. It can never be more
    than that one, and the reason is the lock: the increment and every batch's admission check
    happen under the same `_gpu`, so a batch either sees `_queries_waiting` and blocks, or it got in
    before the query existed. A registered query cannot be overtaken. Losing the race costs one
    batch once, not one per batch in the queue -- which is what keeps this O(1) in queue depth and
    independent of MaxConcurrent, the whole point of the change. Stress-tested at 1200 trials with
    up to 16 batches released simultaneously against a registering query: zero violations, and the
    table below never hit the window.

    Measured on the live service, 2026-08-10, one batch = 32 chunks of real Portuguese prose from
    this repo's markdown, 485-901 tokens each (median 651, 21330 tokens per request), which is the
    band cmd/cli/ingest.go's chunker produces; query = 16 tokens. Medians, n=5 (n=10 for the query
    alone), before and after on the same payload with a service restart between:

        query alone, idle          0,05 s     0,04 s
        one 32-chunk batch alone   1,28 s     1,19 s
        query behind 1 batch       1,23 s     1,18 s
        query behind 2 batches     2,39 s     1,22 s
        query behind 4 batches     4,42 s     1,24 s

    A latency here is meaningless without that payload line: the same scenario measured with
    32 short strings puts a batch at ~0,3 s and every row shrinks with it. What does not depend on
    the payload is the shape -- before, the wait is depth x batch; after, it is one batch.

    That one batch is the floor, and it is why the rows after do not drop below the batch-alone
    row: an encode running on the GPU cannot be preempted, and pretending otherwise would mean a
    second model instance, which ADR-001's memory budget does not have.

    Under sustained query load this starves ingestion, and saying otherwise would be a lie: the rule
    is strict priority with no aging. What bounds it in practice is duty cycle, not fairness -- a
    query holds the GPU for ~45 ms against a batch's ~1,2 s. Ingestion throughput at BatchSize 32,
    MaxConcurrent 2 measured 28,4-30,3 chunks/s before and 28,4-29,3 after, so the cost of the
    scheme itself is at most ~2% and sits inside the run-to-run band; one query per second beside a
    running ingestion cost 3% in a separate pass. Ingestion only stops progressing if queries arrive
    faster than one every ~45 ms, sustained, from a path where a human waits for each answer -- and
    a machine that busy is undersized for the query load by itself. The fix if it ever happens is
    not an aging counter: that would be one more measured constant guarding a variable nobody
    watches, which is the failure this function exists to remove.
    """
    global _gpu_busy, _queries_waiting
    with _gpu:
        if priority:
            _queries_waiting += 1
        try:
            # The second half of the condition is the entire mechanism: a batch declines to take a
            # free GPU while a query is queued. Without it `notify_all` is a race the batch usually
            # wins, because it has been waiting longer and the OS scheduler does not know what
            # these are.
            while _gpu_busy or (not priority and _queries_waiting):
                _gpu.wait()
            _gpu_busy = True
        finally:
            # An admission that raises has to give the count back, and this half is symmetric with
            # the release below for one reason: a leaked `_queries_waiting` is not a slow service,
            # it is a wedged one. Every batch-class request would then wait on a counter that no
            # code path can ever clear again, forever, with the process still answering /health.
            #
            # No reachable raise exists today -- Condition.wait() raises RuntimeError only if the
            # lock is not held, which the `with` guarantees, and signal-driven exceptions land in
            # the main thread, never in a ThreadingHTTPServer worker (verified: SIGINT delivered
            # while a worker sat in wait() raised KeyboardInterrupt in the main thread and the
            # worker returned normally). The guard is here because that argument is about today's
            # code: a timeout on the wait, a move to asyncio, or request cancellation each turn it
            # false, and none of them would look like they were touching this invariant.
            #
            # `_gpu_busy` doubles as the flag for whether the admission succeeded, so the notify
            # only fires on the path that needs it: if it is set, either we hold the GPU (normal
            # exit, nothing to wake) or someone else does and their release will notify. If it is
            # clear, nobody is going to, and any batch deferring to this query would park forever.
            if priority:
                _queries_waiting -= 1
                if not _gpu_busy:
                    _gpu.notify_all()
    try:
        yield
    finally:
        with _gpu:
            _gpu_busy = False
            _gpu.notify_all()


def load_model(device: str, use_fp16: bool) -> None:
    global _model
    _model = BGEM3FlagModel(MODEL_ID, revision=REVISION, use_fp16=use_fp16, devices=device)


def handshake() -> dict:
    """The seven fields of ADR-001 §4, read from the loaded model rather than declared.

    Read, not declared, is the point: a value typed into a config file agrees with itself. These
    come from the object actually doing the inference, so a runtime that quietly loaded something
    else is caught by the client refusing to proceed.
    """
    cfg = _model.model.model.config
    dtype = str(next(_model.model.model.parameters()).dtype).removeprefix("torch.")
    return {
        "model_id": MODEL_ID,
        "model_revision": REVISION,
        "tokenizer_revision": REVISION,  # same repo, same pin -- one artifact, not two
        "dense_dim": cfg.hidden_size,
        "normalized": True,  # modules.json ships 2_Normalize; asserted below at startup
        "pooling": "cls",  # 1_Pooling/config.json: pooling_mode_cls_token
        "precision": dtype,
        "sparse": {"kind": "lexical_weights", "id_space": "tokenizer_vocab"},
        "max_position_embeddings": cfg.max_position_embeddings,
    }


def check_probe(probe: dict) -> None:
    """Assert the two handshake fields that are claims about behaviour rather than about config.

    A model that silently stopped normalizing would still report normalized:true from a constant,
    and the client would trust it -- so the constant is checked against a real vector once, here,
    where failing is loud and cheap. Split out of `main()` so this logic -- one of the two D-24
    mitigations that justified shipping without tests -- can be exercised without a GPU.
    """
    norm = float((probe["dense_vecs"][0] ** 2).sum() ** 0.5)
    if abs(norm - 1.0) > 1e-3:
        raise SystemExit(f"dense vectors are not normalized (norm={norm:.6f}); handshake would lie")
    if not probe["lexical_weights"][0]:
        raise SystemExit("sparse weights came back empty; this build cannot serve hybrid search")


def to_sparse(weights: dict) -> dict:
    """Convert FlagEmbedding's {token_id: weight} into the contract's indices/values pair.

    Three things happen here rather than in Go, because they are properties of this model's output
    and not of the transport (PRD-contrato §2.3b):

      - zero and negative weights are dropped: they carry no lexical signal and cost payload;
      - ids are emitted as uint32, which is the contract's type;
      - the pairs are sorted by id, so the same text always serializes identically. That matters
        because the embedding feeds point_hash, and an unstable order would make an unchanged note
        look changed on every run.
    """
    pairs = sorted(
        (int(tok), float(w)) for tok, w in weights.items() if float(w) > 0.0
    )
    return {"indices": [p[0] for p in pairs], "values": [p[1] for p in pairs]}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    # TCP_NODELAY on every accepted connection. Without it this service answers a /tokenize in ~45 ms
    # on a reused connection and ~1,5 ms on a fresh one — the inversion that D-25 measured, and the
    # reason 94% of an ingestion run was spent waiting here. StreamRequestHandler leaves wfile
    # unbuffered, so a response leaves as two writes: end_headers() batches the status line and the
    # headers into one, then _send() writes the body. Nagle holds that second write until the first
    # is acknowledged, and Linux delays that ACK by 40 ms.
    #
    # Why a fresh connection escaped it is NOT established here, and the obvious guess is wrong: the
    # unbuffered wfile has no application buffer, so the flush on close is a no-op and cannot explain
    # it. The likely cause is that Linux starts a connection in quickack mode and only falls back to
    # delayed ACK after an interactive exchange — plausible, and not verified with a packet trace.
    # The fix does not depend on that question being settled.
    disable_nagle_algorithm = True

    def log_message(self, fmt, *args):  # noqa: A002 - stdlib signature
        pass  # the default logs every request to stderr; the caller already has its own logs

    def _send(self, code: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read(self) -> dict:
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n) or b"{}")

    def do_GET(self):  # noqa: N802 - stdlib signature
        if self.path == "/health":
            self._send(200, {"status": "ok", "loaded": _model is not None})
        elif self.path == "/handshake":
            self._send(200, handshake())
        else:
            self._send(404, {"error": "no such endpoint", "path": self.path})

    def do_POST(self):  # noqa: N802 - stdlib signature
        try:
            req = self._read()
        except json.JSONDecodeError as e:
            self._send(400, {"error": f"invalid JSON: {e}"})
            return

        inputs = req.get("inputs")
        if not isinstance(inputs, list) or not inputs or not all(isinstance(s, str) for s in inputs):
            self._send(400, {"error": "`inputs` must be a non-empty array of strings"})
            return

        try:
            if self.path == "/tokenize":
                # Batch class deliberately, even though ingestion calls this thousands of times per
                # run: those thousands are exactly why it is not interactive. Its only caller is the
                # chunker in cmd/cli/ingest.go -- the query path never tokenizes -- so promoting it
                # would let ingestion overtake ingestion, and would put every clamp check ahead of
                # the batches the same run is waiting on. It costs a query nothing to be behind one:
                # the critical section is a CPU tokenize of a single chunk, milliseconds.
                with gpu(priority=False):
                    enc = _model.tokenizer(inputs, add_special_tokens=True)["input_ids"]
                self._send(200, {"counts": [len(x) for x in enc]})
                return

            if self.path == "/embed":
                # The class comes from the request's own `kind`, which the Go client has always sent
                # (internal/embed/http.go) and this server used to ignore. An explicit field rather
                # than a size heuristic: one-chunk batches are legal at the tail of an ingestion run,
                # so "small means interactive" would misclassify silently, and the caller is the only
                # party that knows whether someone is waiting for the answer. Anything that is not
                # exactly "query" -- passage, absent, misspelled -- is a batch, because the safe
                # default is to not jump the queue.
                priority = req.get("kind") == "query"

                # Tokenize and encode under one acquisition, where they used to take the lock twice.
                # Two acquisitions leave a gap between them, and a query that lost the GPU in that
                # gap would go back to the end of the queue and wait for a *second* batch, which
                # breaks the bound this whole design exists to provide. The cost is that a batch now
                # holds the GPU across its own tokenize, measured at 10,7 ms for 32 chunks against
                # a ~1,1 s encode.
                window = _model.model.model.config.max_position_embeddings
                with gpu(priority=priority):
                    enc = _model.tokenizer(inputs, add_special_tokens=True)["input_ids"]

                    # Refuse rather than truncate. FlagEmbedding silently clips anything past the
                    # window, so without this the endpoint answers 200 with a 1024-float vector that
                    # represents a fraction of the text -- and the `tokens` count reported alongside
                    # it is the *untruncated* one, so nothing in a successful response says the
                    # vector is wrong. Measured: a 27000-word input came back 200, tokens=27002, and
                    # its sparse half collapsed to a single token, which is what a mangled encode
                    # looks like.
                    #
                    # `internal/chunk` already refuses a chunk over the same limit before it ever
                    # calls here, so this is not reachable through the sanctioned path. It exists
                    # because `/tokenize` already has a second Go client and this is a standalone
                    # HTTP service: the next caller may not come through the chunker, and a wrong
                    # answer that looks right is worse than a refusal.
                    #
                    # The refusal itself is sent after the release below: writing a response is not
                    # GPU work, and a client that reads slowly must not hold the model while it does.
                    oversized = next((i for i, ids in enumerate(enc) if len(ids) > window), None)
                    out = None if oversized is not None else _model.encode(
                        inputs, return_dense=True, return_sparse=True, return_colbert_vecs=False
                    )

                if oversized is not None:
                    self._send(400, {
                        "error": "input exceeds the model window; it would be truncated "
                                 "silently and the vector would not represent the text",
                        "index": oversized, "tokens": len(enc[oversized]), "max_tokens": window,
                    })
                    return

                self._send(
                    200,
                    {
                        "dense": [[float(x) for x in v] for v in out["dense_vecs"]],
                        "sparse": [to_sparse(w) for w in out["lexical_weights"]],
                        "tokens": [len(x) for x in enc],
                    },
                )
                return

            self._send(404, {"error": "no such endpoint", "path": self.path})
        except Exception as e:  # noqa: BLE001 - the client must see the failure, not a hang
            self._send(500, {"error": f"{type(e).__name__}: {e}"})


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=7999)
    ap.add_argument("--device", default="cuda:0")
    ap.add_argument("--fp32", action="store_true", help="load in fp32 instead of fp16")
    args = ap.parse_args()

    print(f"loading {MODEL_ID}@{REVISION[:8]} on {args.device} (measured ~315s)...", flush=True)
    load_model(args.device, use_fp16=not args.fp32)

    probe = _model.encode(["handshake probe"], return_dense=True, return_sparse=True,
                          return_colbert_vecs=False)
    check_probe(probe)

    hs = handshake()
    print(json.dumps(hs), flush=True)
    print(f"ready on http://{args.host}:{args.port}", flush=True)
    ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
