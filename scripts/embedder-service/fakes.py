"""Stand-ins for the model and the app wiring, shared by the test modules.

`FakeTextModel` counts one token per whitespace-separated word plus two special tokens. That is not
Gemma's tokenization and is not trying to be: the tests are about the rules around the count (the
window, the prefix, the ordering), so the count has to be something a test can state exactly. The
prompted texts it sees are recorded, which is how a test proves the prefix reached the model.
"""

import threading
from collections.abc import Callable

from app import EmbedderApp
from gpu_gate import GpuGate
from model_host import ModelHost
from settings import Settings

SPECIAL_TOKENS = 2


def make_settings(**overrides) -> Settings:
    base = dict(
        host="127.0.0.1", port=7999, device="cuda:0", precision="bf16",
        idle_unload_seconds=600.0, batch_size=8, max_input_tokens=64,
    )
    return Settings(**{**base, **overrides})


class FakeTextModel:
    def __init__(self) -> None:
        self.encoded: list[list[str]] = []   # prompted texts per encode call, in call order
        self.batch_sizes: list[int] = []
        self.counted: list[str] = []         # first text of every count_tokens call: admission order
        self.hold = threading.Event()        # tests that need a call "in flight" clear it first
        self.hold.set()

    def count_tokens(self, texts: list[str]) -> list[int]:
        self.counted.append(texts[0])
        return [len(t.split()) + SPECIAL_TOKENS for t in texts]

    def encode(self, texts: list[str], batch_size: int) -> list[list[float]]:
        if not self.hold.wait(timeout=10):
            raise AssertionError("the test never released the model")
        self.encoded.append(list(texts))
        self.batch_sizes.append(batch_size)
        return [[float(len(t.split())), float(sum(map(ord, t)) % 997)] for t in texts]


class FakeClock:
    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += seconds


class Wiring:
    """A ModelHost over a FakeTextModel with a fake clock, and an app on top of it."""

    def __init__(
        self,
        settings: Settings | None = None,
        cuda: Callable[[], bool] = lambda: True,
        model: FakeTextModel | None = None,
    ) -> None:
        self.model = model or FakeTextModel()
        self.clock = FakeClock()
        self.loads = 0
        self.releases = 0
        self.logs: list[str] = []
        self.host = ModelHost(self._load, self._release, clock=self.clock, log=self.logs.append)
        self.gate = GpuGate()
        self.settings = settings or make_settings()
        self.app = EmbedderApp(self.settings, self.host, self.gate, cuda)

    def _load(self) -> FakeTextModel:
        self.loads += 1
        return self.model

    def _release(self) -> None:
        self.releases += 1
