"""The real model: sentence-transformers around the pinned EmbeddingGemma 2 repository.

This is the only module that imports torch or sentence-transformers, and nothing imports it at
module scope except `server.build_app`, so the rest of the service (and its tests) runs without a
GPU, without torch and without the 517 MB of weights.

Text only. The checkpoint ships vision and audio towers; `config_kwargs` drops both configs so they
are neither built nor held in VRAM. torchvision still has to be installed (transformers imports it
while resolving the processor), which is why requirements.txt lists it for a text-only service.

`load_verified_model` is where the handshake becomes true. `/handshake` reports the pinned
configuration without loading anything (see `app.py`); this function refuses to return a model that
contradicts it, so a served vector always comes from a model that matched what was reported.
"""

import gc
import math
from typing import Any

from settings import (
    DENSE_DIM,
    MODEL_ID,
    POOLING,
    REVISION,
    Settings,
    UnsupportedSetting,
)

_MIB = 1024 * 1024


def cuda_available() -> bool:
    import torch

    return bool(torch.cuda.is_available())


def require_cuda(device: str) -> None:
    """Refuse to start without the requested CUDA device. There is no CPU path to fall back to."""
    import torch

    if not torch.cuda.is_available():
        raise UnsupportedSetting(
            "no CUDA device is available to this process (torch.cuda.is_available() is False). "
            "This service runs on the GPU only and has no CPU fallback; check the driver "
            "(nvidia-smi) and that torch was installed with CUDA support"
        )
    index = int(device.partition(":")[2] or 0)
    if index >= torch.cuda.device_count():
        raise UnsupportedSetting(
            f"device {device!r} not found: torch sees {torch.cuda.device_count()} CUDA device(s)"
        )


class SentenceTransformerText:
    """`ModelHost`'s TextModel over a loaded SentenceTransformer."""

    def __init__(self, model: Any) -> None:
        self._model = model

    def count_tokens(self, texts: list[str]) -> list[int]:
        ids = self._model.tokenizer(texts, add_special_tokens=True)["input_ids"]
        return [len(x) for x in ids]

    def encode(self, texts: list[str], batch_size: int) -> list[list[float]]:
        vectors = self._model.encode(
            texts,
            batch_size=batch_size,
            normalize_embeddings=True,
            convert_to_numpy=True,
            show_progress_bar=False,
        )
        return vectors.astype("float32").tolist()

    def facts(self) -> dict[str, Any]:
        """What the loaded model is, read from the object doing the inference."""
        probe = self.encode(["handshake probe"], batch_size=1)[0]
        return {
            "dense_dim": self._model.get_embedding_dimension(),
            "dtype": str(next(self._model.parameters()).dtype).removeprefix("torch."),
            "pooling": _pooling_mode(self._model),
            "probe_norm": math.sqrt(sum(x * x for x in probe)),
            "probe_width": len(probe),
        }


def _pooling_mode(model: Any) -> str:
    """The pooling module's own mode ("mean"), read off the loaded model; "" when there is none."""
    for module in model:
        mode = getattr(module, "pooling_mode", None)
        if isinstance(mode, str):
            return mode
    return ""


def check_facts(facts: dict[str, Any], settings: Settings) -> None:
    """Raise when the loaded model is not the one `/handshake` reports. Pure, so it is unit-tested."""
    problems = []
    if facts["dense_dim"] != DENSE_DIM or facts["probe_width"] != DENSE_DIM:
        problems.append(f"width {facts['dense_dim']}/{facts['probe_width']}, pinned {DENSE_DIM}")
    if facts["dtype"] != settings.torch_dtype_name:
        problems.append(f"dtype {facts['dtype']}, configured {settings.torch_dtype_name}")
    if facts["pooling"] != POOLING:
        problems.append(f"pooling {facts['pooling']!r}, pinned {POOLING!r}")
    if abs(facts["probe_norm"] - 1.0) > 1e-2:
        problems.append(f"probe vector norm {facts['probe_norm']:.4f}, not L2-normalized")
    if problems:
        raise RuntimeError(
            f"loaded {MODEL_ID}@{REVISION[:8]} does not match what /handshake reports: "
            + "; ".join(problems)
        )


def load_verified_model(settings: Settings) -> SentenceTransformerText:
    import torch
    from sentence_transformers import SentenceTransformer

    st = SentenceTransformer(
        MODEL_ID,
        revision=REVISION,
        device=settings.device,
        model_kwargs={"dtype": getattr(torch, settings.torch_dtype_name)},
        config_kwargs={"vision_config": None, "audio_config": None},
    )
    # Explicit: at this revision the loaded model reports max_seq_length 1e30 (no limit at all), so
    # the real bound is whatever is set here. /embed refuses anything over it before it gets here,
    # and this keeps a future caller that bypasses /embed from running unbounded activations.
    st.max_seq_length = settings.max_input_tokens
    model = SentenceTransformerText(st)
    check_facts(model.facts(), settings)
    return model


def release_cuda_memory() -> None:
    """After the model reference is dropped: collect it and hand the cached blocks back to the driver."""
    import torch

    before = torch.cuda.memory_reserved() / _MIB
    gc.collect()
    torch.cuda.empty_cache()
    after = torch.cuda.memory_reserved() / _MIB
    print(f"cuda memory reserved by this process: {before:.0f} MiB -> {after:.0f} MiB", flush=True)
