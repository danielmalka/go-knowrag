"""One call on the GPU at a time, with interactive queries admitted ahead of queued batches (D-27).

One CUDA model is not safe to call concurrently, and the consumers do not need it: query embedding
is one text at a time, ingestion sends batches. Serialized is not first-come-first-served, though.
A plain lock has no idea who is waiting, so a query issued during an ingestion queued behind every
batch already waiting, and the wait grew with the ingestion's concurrency. Here a batch yields to
any query that is already waiting.

The bound that buys, because it is the property the next edit has to preserve: a query waits for the
call in flight when it registers, plus at most one batch that won the "the GPU just freed" race
before the increment below landed. Never more, and the reason is the lock: the increment and every
batch's admission check happen under the same condition, so a batch either sees `_queries_waiting`
and blocks, or got in before the query existed. A registered query cannot be overtaken. That is O(1)
in queue depth. Measured on the BGE-M3 service that preceded this one (2026-08-10, batches of 32
chunks of 485-901 tokens, a ~1.2 s encode each): a query behind four queued batches waited 4.4 s
with a plain lock and 1.2 s with this gate. The shape is what carries over to this model, not the
numbers. One batch is the floor: an encode on the GPU cannot be preempted.

Under sustained query load this starves ingestion. The rule is strict priority with no aging, and
the duty cycle (a query holds the GPU for tens of milliseconds, a batch for about a second) is what
bounds it in practice, not fairness. An aging counter would be one more tuned constant guarding a
variable nobody watches, which is the failure this gate exists to remove.
"""

import contextlib
import threading
from collections.abc import Iterator


class GpuGate:
    def __init__(self) -> None:
        self._cond = threading.Condition()
        self._busy = False
        self._queries_waiting = 0

    @contextlib.contextmanager
    def hold(self, priority: bool) -> Iterator[None]:
        """Hold the GPU for one call. `priority` callers are admitted ahead of queued batches."""
        self._admit(priority)
        try:
            yield
        finally:
            with self._cond:
                self._busy = False
                self._cond.notify_all()

    def _admit(self, priority: bool) -> None:
        with self._cond:
            if priority:
                self._queries_waiting += 1
            try:
                # The second half of the condition is the whole mechanism: a batch declines a free
                # GPU while a query is queued. Without it notify_all is a race the batch usually
                # wins, because it has been waiting longer and the scheduler does not know what
                # these are.
                while self._busy or (not priority and self._queries_waiting):
                    self._cond.wait()
                self._busy = True
            finally:
                # An admission that raises must give the count back: a leaked `_queries_waiting` is
                # not a slow service, it is a wedged one (every batch waits on a counter nothing can
                # clear, while /health still answers). No raise is reachable today -- Condition.wait
                # raises only without the lock, which `with` guarantees -- but a timeout on the
                # wait, asyncio or request cancellation would each make it reachable without
                # looking like they touch this invariant. `_busy` doubles as "admission succeeded":
                # if it is clear nobody will notify, and batches deferring to this query would park
                # forever.
                if priority:
                    self._queries_waiting -= 1
                    if not self._busy:
                        self._cond.notify_all()
