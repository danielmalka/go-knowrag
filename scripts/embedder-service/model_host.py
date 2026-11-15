"""Load the model on the first call, give the GPU memory back after it has been idle.

The decision (owner, 2026-11-15): the service always runs on the GPU with no CPU fallback;
ingestion runs on fixed timers; a search loads the model on the first question and the model is
unloaded after a configurable idle time (default 10 min), so the card is not held between uses.
The process stays up. What is unloaded is the model, not the server, and "unloaded" has to mean the
VRAM is back: the reference is dropped, `gc.collect()` runs and the CUDA cache is emptied (the
`release` hook). The CUDA context of the process itself stays allocated (a few hundred MB, measured
and recorded in the README); only ending the process returns that.

One lock covers three things that must never overlap: loading, unloading and the start of a call.
That is the whole race this class exists to close:

  - a call arriving while the model is loading waits for the load instead of starting a second one;
  - a call arriving while the model is being released waits, then loads it again;
  - the idle check cannot release a model that a call is using, because every call counts itself in
    `_active` under the same lock, and the check only acts at zero.

The GPU itself is serialized by `GpuGate`, a different lock: this one orders the lifecycle, that one
orders inference. A call takes this one first (to get a loaded model and be counted), then the gate.
"""

import contextlib
import threading
import time
from collections.abc import Callable, Iterator
from typing import Protocol


class TextModel(Protocol):
    def count_tokens(self, texts: list[str]) -> list[int]:
        """Tokens per text, special tokens included -- what the model reads."""
        ...

    def encode(self, texts: list[str], batch_size: int) -> list[list[float]]:
        """L2-normalized dense vectors, one per text, in order."""
        ...


def _print_line(message: str) -> None:
    print(message, flush=True)


class ModelHost:
    def __init__(
        self,
        loader: Callable[[], TextModel],
        release: Callable[[], None],
        clock: Callable[[], float] = time.monotonic,
        log: Callable[[str], None] = _print_line,
    ) -> None:
        self._loader = loader
        self._release = release
        self._clock = clock
        self._log = log
        self._cond = threading.Condition()
        self._model: TextModel | None = None
        self._active = 0
        self._last_used = clock()

    @property
    def loaded(self) -> bool:
        return self._model is not None

    @contextlib.contextmanager
    def use(self) -> Iterator[TextModel]:
        """The loaded model for one call; loads it first when it is not."""
        with self._cond:
            if self._model is None:
                self._load_locked()
            model = self._model
            self._active += 1
        try:
            yield model  # type: ignore[misc]  # non-None: set above under the same lock
        finally:
            with self._cond:
                self._active -= 1
                self._last_used = self._clock()
                self._cond.notify_all()

    def unload_if_idle(self, idle_seconds: float) -> bool:
        """Release the model if it has had no call for `idle_seconds`. False when it did not."""
        if idle_seconds <= 0:
            return False
        with self._cond:
            if self._model is None or self._active > 0:
                return False
            idle_for = self._clock() - self._last_used
            if idle_for < idle_seconds:
                return False
            self._unload_locked(f"idle for {idle_for:.0f}s (limit {idle_seconds:.0f}s)")
            return True

    def _load_locked(self) -> None:
        started = self._clock()
        self._model = self._loader()  # a raise leaves None, so the next call tries again
        self._log(f"model loaded in {self._clock() - started:.1f}s")

    def _unload_locked(self, reason: str) -> None:
        self._model = None
        self._release()
        self._last_used = self._clock()
        self._log(f"model unloaded: {reason}")


class IdleUnloader(threading.Thread):
    """Calls `unload_if_idle` on a fixed beat until `stop()`; daemon, so it never holds exit."""

    def __init__(self, host: ModelHost, idle_seconds: float) -> None:
        super().__init__(name="idle-unloader", daemon=True)
        self._host = host
        self._idle_seconds = idle_seconds
        self._stop_event = threading.Event()
        self._interval = min(5.0, max(0.05, idle_seconds / 4))

    def run(self) -> None:
        while not self._stop_event.wait(self._interval):
            self._host.unload_if_idle(self._idle_seconds)

    def stop(self) -> None:
        self._stop_event.set()
