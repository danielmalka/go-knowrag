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

Resident by design, not by preference: loading the model measured **314.5 s**. A process that loads
on demand would make the first query of the day take five minutes and turn the measured 71 ms p99
into fiction. This process loads once at startup and stays up. It is never invoked per request.

Inference is serialized behind a lock. One CUDA model is not safe to call concurrently, and the
consumers do not need it: query embedding is one at a time by nature, and ingestion sends batches.

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
_lock = threading.Lock()


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
                with _lock:
                    enc = _model.tokenizer(inputs, add_special_tokens=True)["input_ids"]
                self._send(200, {"counts": [len(x) for x in enc]})
                return

            if self.path == "/embed":
                with _lock:
                    enc = _model.tokenizer(inputs, add_special_tokens=True)["input_ids"]

                # Refuse rather than truncate. FlagEmbedding silently clips anything past the
                # window, so without this the endpoint answers 200 with a 1024-float vector that
                # represents a fraction of the text -- and the `tokens` count reported alongside it
                # is the *untruncated* one, so nothing in a successful response says the vector is
                # wrong. Measured: a 27000-word input came back 200, tokens=27002, and its sparse
                # half collapsed to a single token, which is what a mangled encode looks like.
                #
                # `internal/chunk` already refuses a chunk over the same limit before it ever calls
                # here, so this is not reachable through the sanctioned path. It exists because
                # `/tokenize` already has a second Go client and this is a standalone HTTP service:
                # the next caller may not come through the chunker, and a wrong answer that looks
                # right is worse than a refusal.
                window = _model.model.model.config.max_position_embeddings
                for i, ids in enumerate(enc):
                    if len(ids) > window:
                        self._send(400, {
                            "error": "input exceeds the model window; it would be truncated "
                                     "silently and the vector would not represent the text",
                            "index": i, "tokens": len(ids), "max_tokens": window,
                        })
                        return

                with _lock:
                    out = _model.encode(
                        inputs, return_dense=True, return_sparse=True, return_colbert_vecs=False
                    )
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
