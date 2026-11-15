"""D-27: a query is admitted ahead of queued batches, and waits only for the one in flight.

Ported from the BGE-M3 service's suite, where these orderings were measured. The tests assert an
order, and an order test that depends on timing lies on a loaded machine, so each one advances only
on a predicate about real state: a thread was admitted, or N threads are parked in the condition.
"""

import threading
import time
from unittest import mock

import pytest

from gpu_gate import GpuGate


class Rig:
    def __init__(self) -> None:
        self.gate = GpuGate()
        self.admitted: list[str] = []
        self.release = threading.Event()
        self.threads: list[threading.Thread] = []
        self.failures: list[BaseException] = []

    def call(self, label: str, priority: bool) -> None:
        def run() -> None:
            try:
                with self.gate.hold(priority):
                    self.admitted.append(label)  # one writer at a time: the caller holds the GPU
                    assert self.release.wait(timeout=10), "the test never released the GPU"
            except BaseException as e:  # noqa: BLE001 - reported at teardown, never swallowed
                self.failures.append(e)

        t = threading.Thread(target=run, daemon=True)
        t.start()
        self.threads.append(t)

    def until(self, predicate, what: str, timeout: float = 5) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(0.001)
        pytest.fail(f"timed out waiting for {what}; admitted={self.admitted}")

    def until_parked(self, n: int) -> None:
        # Condition parks its waiters in `_waiters`; reading that length is what makes these tests
        # deterministic. Private, and stable in CPython for as long as this module has existed.
        self.until(lambda: len(self.gate._cond._waiters) == n, f"{n} caller(s) queued")

    def hold_with(self, label: str = "A") -> None:
        self.call(label, priority=False)
        self.until(lambda: self.admitted == [label], f"{label} to reach the GPU")

    def finish(self) -> None:
        self.release.set()
        for t in self.threads:
            t.join(timeout=10)
            assert not t.is_alive(), "a call never finished"
        assert self.failures == []


@pytest.fixture
def rig():
    r = Rig()
    yield r
    r.finish()


@pytest.mark.parametrize("queued", [1, 2, 4])
def test_a_query_overtakes_queued_batches_however_many_there_are(queued):
    r = Rig()
    r.hold_with("A")
    for i in range(queued):
        r.call(f"B{i}", priority=False)
    r.until_parked(queued)
    r.call("Q", priority=True)
    r.until_parked(queued + 1)

    r.release.set()
    r.until(lambda: len(r.admitted) == queued + 2, "every call to be admitted")
    assert r.admitted[:2] == ["A", "Q"]
    assert sorted(r.admitted[2:]) == [f"B{i}" for i in range(queued)]
    r.finish()


def test_priority_is_not_bought_by_running_beside_the_batch_in_flight(rig):
    # One CUDA model is not safe to call concurrently: the query is parked, not admitted.
    rig.hold_with("A")
    rig.call("Q", priority=True)
    rig.until_parked(1)
    assert rig.admitted == ["A"]


def test_only_priority_calls_count_as_waiting_queries(rig):
    rig.hold_with("A")
    for i in range(3):
        rig.call(f"B{i}", priority=False)
    rig.until_parked(3)
    assert rig.gate._queries_waiting == 0
    rig.call("Q", priority=True)
    rig.until_parked(4)
    assert rig.gate._queries_waiting == 1


def test_a_failed_admission_does_not_wedge_the_gate(rig):
    # No raise reaches the wait today, which is why the guard needs a test rather than an argument:
    # a leaked `_queries_waiting` parks every batch forever while /health keeps answering ok.
    rig.hold_with("A")
    with mock.patch.object(rig.gate._cond, "wait", side_effect=RuntimeError("interrupted")):
        with pytest.raises(RuntimeError):
            with rig.gate.hold(priority=True):
                pytest.fail("the body must not run when the admission failed")
    assert rig.gate._queries_waiting == 0

    rig.call("B", priority=False)
    rig.release.set()
    rig.until(lambda: rig.admitted == ["A", "B"], "the batch to be admitted after the failure")
