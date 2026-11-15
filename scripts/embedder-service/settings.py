"""What the embedding service is pinned to, and what an operator may change.

Two different things live here and they are kept apart on purpose:

  - `MODEL_ID`, `REVISION`, `PROMPT_VERSION` and the dense width describe the ARTIFACT. They match
    `schema.ModelProfile` "embeddinggemma-2" on the Go side, and the client's handshake refuses the
    service if any of them disagrees. Changing one means reindexing, because the profile is part of
    `point_hash`. They are constants, not configuration.
  - `Settings` is what an operator tunes per machine: port, idle unload, batch, token ceiling,
    precision between the two the profile allows. None of it changes what a vector means.

Precision is the one setting with a refusal attached: float16 is not an option the service offers.
The EmbeddingGemma card says not to use it, and the failure it describes is silent (NaN or garbage
vectors that still have the right shape), so the refusal is here, at startup, rather than in a
code path that a typo could route around.
"""

import argparse
from collections.abc import Mapping, Sequence
from dataclasses import dataclass

MODEL_ID = "google/embeddinggemma-2"
# Head of the repository as read on 2026-11-15 (last commit 2026-10-28); the trial itself did not
# pin one. Same value as `schema.ModelProfile.Revision` on the Go side. The tokenizer ships in the
# same repository at the same commit, so it is the same pin.
REVISION = "f9972ac22e696a105c482202646979e33e4b5a1e"
PROMPT_VERSION = "gemma-search-v1"
DENSE_DIM = 768
POOLING = "mean"

# The model card's context window ("8,192 tokens", text 1 token per subword). The text config
# reports max_position_embeddings 262144, which is the rotary table and not what the model was
# trained to read, so it is not used as a limit.
MODEL_WINDOW_TOKENS = 8192

# Exit status for "this machine or this setting cannot run the service" (sysexits EX_CONFIG). The
# unit lists it in RestartPreventExitStatus so a missing GPU is one clear failure, not a restart loop.
EXIT_UNSUPPORTED = 78

# Operator-facing precision names -> torch dtype names. The values are what /handshake reports and
# what the Go client maps to "bf16"/"fp32".
PRECISIONS = {"bf16": "bfloat16", "fp32": "float32"}
REFUSED_PRECISIONS = {"fp16", "float16", "half"}

DEFAULT_IDLE_UNLOAD_SECONDS = 600.0
# Conservative on purpose, and measured (README, "Batch and token ceiling"): activations grow linearly
# with batch x tokens, so the worst input the service accepts (8,000 tokens) peaks at 4.3 GB of torch
# allocation at batch 4 and 8.1 GB at batch 8, while throughput on typical chunks (~370 tokens) is
# within 15% from batch 4 to batch 16. Raising it is an operator decision with those numbers in hand.
DEFAULT_BATCH_SIZE = 4


class UnsupportedSetting(SystemExit):
    """A refusal that ends the process with EXIT_UNSUPPORTED and a message naming the value."""

    def __init__(self, message: str) -> None:
        super().__init__(message)
        self.code = EXIT_UNSUPPORTED
        self.message = message


@dataclass(frozen=True)
class Settings:
    host: str
    port: int
    device: str
    precision: str  # "bf16" | "fp32"
    idle_unload_seconds: float  # 0 disables unloading
    batch_size: int
    max_input_tokens: int

    @property
    def torch_dtype_name(self) -> str:
        return PRECISIONS[self.precision]


def parse_precision(value: str) -> str:
    name = value.strip().lower()
    if name in REFUSED_PRECISIONS:
        raise UnsupportedSetting(
            f"precision {value!r} refused: the {MODEL_ID} card says not to use float16 (it fails "
            f"silently, with vectors of the right shape). Use one of {sorted(PRECISIONS)}."
        )
    if name not in PRECISIONS:
        raise UnsupportedSetting(
            f"unknown precision {value!r}; this service offers {sorted(PRECISIONS)}"
        )
    return name


def parse_device(value: str) -> str:
    if not value.startswith("cuda"):
        raise UnsupportedSetting(
            f"device {value!r} refused: this service runs on CUDA only and has no CPU fallback "
            "(use 'cuda' or 'cuda:<index>')"
        )
    return value


# The service has no authentication of any kind, and `/load`, `/embed` and `/tokenize` hand arbitrary
# GPU work to whoever can reach them. That is acceptable only because it listens on loopback, so
# loopback is enforced here rather than left to a default: there is no opt-in to a network address in
# this service. Exposing it would mean adding authentication first, as its own decision.
LOOPBACK_HOSTS = ("127.0.0.1", "::1", "localhost")


def parse_host(value: str) -> str:
    host = value.strip()
    if host not in LOOPBACK_HOSTS:
        raise UnsupportedSetting(
            f"host {value!r} refused: this service has no authentication, so it listens on loopback "
            f"only ({', '.join(LOOPBACK_HOSTS)}); it must not be reachable from the network"
        )
    return host


def _positive_int(name: str, raw: str, ceiling: int | None = None) -> int:
    try:
        value = int(raw)
    except ValueError:
        raise UnsupportedSetting(f"{name} = {raw!r}, expected a positive integer") from None
    if value <= 0 or (ceiling is not None and value > ceiling):
        bound = f" up to {ceiling}" if ceiling is not None else ""
        raise UnsupportedSetting(f"{name} = {value}, expected a positive integer{bound}")
    return value


def _non_negative_float(name: str, raw: str) -> float:
    try:
        value = float(raw)
    except ValueError:
        raise UnsupportedSetting(f"{name} = {raw!r}, expected a number of seconds") from None
    if value < 0:
        raise UnsupportedSetting(f"{name} = {value}, expected 0 (never unload) or more seconds")
    return value


def build_parser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(description="go-knowrag embedding service (EmbeddingGemma 2)")
    ap.add_argument("--host", help="loopback address: 127.0.0.1 (default), ::1 or localhost; nothing else")
    ap.add_argument("--port", help="TCP port (default 7999)")
    ap.add_argument("--device", help="CUDA device (default cuda:0); there is no CPU option")
    ap.add_argument("--precision", help="bf16 (default) or fp32; fp16 is refused")
    ap.add_argument("--idle-unload", help="seconds idle before the model is unloaded; 0 never")
    ap.add_argument("--batch-size", help="texts per forward pass (default 4)")
    ap.add_argument("--max-input-tokens", help="refuse any input over this many tokens")
    return ap


def load_settings(argv: Sequence[str], environ: Mapping[str, str]) -> Settings:
    """Flags win over `KNOWRAG_EMBEDDER_*` variables, which win over the defaults."""
    args = build_parser().parse_args(argv)

    def pick(flag: str | None, env_name: str, default: str) -> str:
        return flag if flag is not None else environ.get(env_name, default)

    return Settings(
        host=parse_host(pick(args.host, "KNOWRAG_EMBEDDER_HOST", "127.0.0.1")),
        port=_positive_int("port", pick(args.port, "KNOWRAG_EMBEDDER_PORT", "7999"), 65535),
        device=parse_device(pick(args.device, "KNOWRAG_EMBEDDER_DEVICE", "cuda:0")),
        precision=parse_precision(pick(args.precision, "KNOWRAG_EMBEDDER_PRECISION", "bf16")),
        idle_unload_seconds=_non_negative_float(
            "idle unload",
            pick(args.idle_unload, "KNOWRAG_EMBEDDER_IDLE_UNLOAD", str(DEFAULT_IDLE_UNLOAD_SECONDS)),
        ),
        batch_size=_positive_int(
            "batch size", pick(args.batch_size, "KNOWRAG_EMBEDDER_BATCH_SIZE", str(DEFAULT_BATCH_SIZE))
        ),
        max_input_tokens=_positive_int(
            "max input tokens",
            pick(args.max_input_tokens, "KNOWRAG_EMBEDDER_MAX_INPUT_TOKENS", str(MODEL_WINDOW_TOKENS)),
            MODEL_WINDOW_TOKENS,
        ),
    )
