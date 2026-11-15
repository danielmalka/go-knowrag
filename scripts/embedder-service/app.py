"""What each endpoint answers. No HTTP in here, so every rule below is testable with a fake model.

Endpoints, all JSON (the wire is described in README.md):

  GET  /health     {"status": "ok", "loaded": bool, "cuda": bool}   never loads the model
  GET  /handshake  the fields the Go client verifies against its model profile; never loads it
  POST /load       loads the model now (what ensure-embedder.sh calls before an ingestion)
  POST /tokenize   {"inputs": [str]} -> {"counts": [int]}    the chunker's token counter
  POST /embed      {"inputs": [str], "kind": "query"|"passage", "titles"?: [str]}
                   -> {"dense": [[float]], "tokens": [int]}

Two decisions worth knowing before changing anything:

`/handshake` does not load the model. The MCP server verifies it at start-up with a 1.5 s deadline
and nothing is searching yet; loading there would make every client start pay a load, and the model
would be unloaded again before the first question. What it reports is therefore the pinned
configuration, and the load is what makes that report true: `st_adapter.load_verified_model`
refuses to return a model whose width, dtype, pooling or norm differ from it. A lie would need the
load to have passed first, and the first `/embed` is that load.

`/tokenize` counts the text alone, special tokens included; `/embed` counts the prompted text, which
is what the model reads. The difference is the prefix the Go profile reserves
(`PromptReserveTokens`): the chunker sizes the text, the service refuses anything that, once the
prefix and title are in front of it, no longer fits the window.
"""

from collections.abc import Callable, Sequence
from dataclasses import dataclass
from typing import Any

from gpu_gate import GpuGate
from model_host import ModelHost
from prompts import apply_prompts
from settings import (
    DENSE_DIM,
    MODEL_ID,
    POOLING,
    PROMPT_VERSION,
    REVISION,
    Settings,
)


@dataclass(frozen=True)
class Reply:
    status: int
    body: dict[str, Any]


def _bad_request(message: str, **extra: Any) -> Reply:
    return Reply(400, {"error": message, **extra})


def read_inputs(req: dict[str, Any]) -> list[str] | None:
    inputs = req.get("inputs")
    if isinstance(inputs, list) and inputs and all(isinstance(s, str) for s in inputs):
        return inputs
    return None


def read_titles(req: dict[str, Any]) -> tuple[list[str] | None, bool]:
    """(titles, ok). Absent means no titles; present must be a list of strings."""
    if "titles" not in req:
        return None, True
    titles = req["titles"]
    if isinstance(titles, list) and all(isinstance(s, str) for s in titles):
        return titles, True
    return None, False


class EmbedderApp:
    def __init__(
        self,
        settings: Settings,
        host: ModelHost,
        gate: GpuGate,
        cuda_available: Callable[[], bool],
    ) -> None:
        self._settings = settings
        self._host = host
        self._gate = gate
        self._cuda_available = cuda_available

    def health(self) -> Reply:
        # Not a use of the model: a probe is not work, and counting it would keep the weights
        # resident for as long as anything polls.
        return Reply(200, {"status": "ok", "loaded": self._host.loaded, "cuda": self._cuda_available()})

    def handshake(self) -> Reply:
        if not self._cuda_available():
            return Reply(503, {
                "error": "no CUDA device is available; this service runs on the GPU only and has "
                         "no CPU fallback",
            })
        return Reply(200, {
            "model_id": MODEL_ID,
            "model_revision": REVISION,
            "tokenizer_revision": REVISION,  # same repository, same commit
            "dense_dim": DENSE_DIM,
            "normalized": True,  # the Normalize module; checked against a real vector at load
            "pooling": POOLING,
            "precision": self._settings.torch_dtype_name,
            "prompt_version": PROMPT_VERSION,
            "max_input_tokens": self._settings.max_input_tokens,
        })

    def load(self) -> Reply:
        with self._host.use():
            pass
        return Reply(200, {"loaded": True})

    def tokenize(self, req: dict[str, Any]) -> Reply:
        inputs = read_inputs(req)
        if inputs is None:
            return _bad_request("`inputs` must be a non-empty array of strings")
        # Batch class, though ingestion calls it thousands of times: its only caller is the
        # chunker, the query path never tokenizes, and a fast tokenizer is not safe to share
        # between threads, so it is serialized with the encode like everything else.
        with self._host.use() as model, self._gate.hold(priority=False):
            counts = model.count_tokens(inputs)
        return Reply(200, {"counts": counts})

    def embed(self, req: dict[str, Any]) -> Reply:
        inputs = read_inputs(req)
        if inputs is None:
            return _bad_request("`inputs` must be a non-empty array of strings")
        titles, titles_ok = read_titles(req)
        if not titles_ok:
            return _bad_request("`titles` must be an array of strings")
        kind = req.get("kind")
        try:
            texts = apply_prompts(kind, inputs, titles)
        except ValueError as e:
            return _bad_request(str(e))
        return self._embed_prompted(texts, priority=kind == "query")

    def _embed_prompted(self, texts: Sequence[str], priority: bool) -> Reply:
        limit = self._settings.max_input_tokens
        with self._host.use() as model:
            # Tokenize and encode under one admission. Two would leave a gap in which a query that
            # lost the GPU goes to the back of the queue and waits for a second batch, which breaks
            # the bound the gate provides.
            with self._gate.hold(priority=priority):
                counts = model.count_tokens(list(texts))
                oversized = next((i for i, n in enumerate(counts) if n > limit), None)
                vectors = None if oversized is not None else model.encode(
                    list(texts), self._settings.batch_size
                )
        # The refusal is sent after the release: writing a response is not GPU work, and a slow
        # reader must not hold the model.
        if oversized is not None:
            return _bad_request(
                "input exceeds the token limit once the task prefix and title are in front of it; "
                "it would be truncated silently and the vector would not represent the text",
                index=oversized, tokens=counts[oversized], max_tokens=limit,
            )
        return Reply(200, {"dense": vectors, "tokens": counts})
