"""Load on first use, unload after idle, and the races between loading, unloading and calls.

Time is a fake clock the test advances, and every wait on another thread is on a state that thread
reached (an Event it set, a counter it bumped), never on a sleep that hopes an order happened.
"""

import threading

import pytest

from fakes import FakeClock, FakeTextModel
from model_host import IdleUnloader, ModelHost


class Host:
    """A ModelHost whose loader and release hook the test can observe and block."""

    def __init__(self) -> None:
        self.clock = FakeClock()
        self.loads = 0
        self.releases = 0
        self.logs: list[str] = []
        self.release_started = threading.Event()
        self.release_may_finish = threading.Event()
        self.release_may_finish.set()
        self.fail_next_load = False
        self.host = ModelHost(self._load, self._release, clock=self.clock, log=self.logs.append)

    def _load(self):
        if self.fail_next_load:
            self.fail_next_load = False
            raise RuntimeError("CUDA out of memory")
        self.loads += 1
        return FakeTextModel()

    def _release(self) -> None:
        self.release_started.set()
        assert self.release_may_finish.wait(timeout=10)
        self.releases += 1


def test_nothing_is_loaded_until_the_first_call():
    h = Host()
    assert (h.host.loaded, h.loads) == (False, 0)
    with h.host.use():
        assert h.host.loaded
    assert h.loads == 1


def test_a_second_call_reuses_the_loaded_model():
    h = Host()
    with h.host.use() as first:
        pass
    with h.host.use() as second:
        pass
    assert (first is second, h.loads) == (True, 1)


def test_idle_beyond_the_limit_unloads_and_releases_memory():
    h = Host()
    with h.host.use():
        pass
    h.clock.advance(599)
    assert h.host.unload_if_idle(600) is False
    h.clock.advance(2)
    assert h.host.unload_if_idle(600) is True
    assert (h.host.loaded, h.releases) == (False, 1)
    assert any("unloaded" in line for line in h.logs)


def test_a_zero_limit_never_unloads():
    h = Host()
    with h.host.use():
        pass
    h.clock.advance(10**6)
    assert h.host.unload_if_idle(0) is False
    assert h.host.loaded


def test_use_resets_the_idle_clock():
    h = Host()
    with h.host.use():
        pass
    h.clock.advance(500)
    with h.host.use():
        pass
    h.clock.advance(500)
    assert h.host.unload_if_idle(600) is False


def test_a_call_in_flight_blocks_the_unload_however_long_it_runs():
    h = Host()
    with h.host.use():
        h.clock.advance(10_000)
        assert h.host.unload_if_idle(600) is False
        assert h.host.loaded
    # the call just ended, so the clock starts from now: still not idle
    assert h.host.unload_if_idle(600) is False


def test_the_model_is_loaded_again_after_an_unload():
    h = Host()
    with h.host.use():
        pass
    h.clock.advance(601)
    h.host.unload_if_idle(600)
    with h.host.use():
        pass
    assert (h.loads, h.releases) == (2, 1)


def test_a_failed_load_leaves_nothing_loaded_and_the_next_call_retries():
    h = Host()
    h.fail_next_load = True
    with pytest.raises(RuntimeError, match="out of memory"):
        with h.host.use():
            pytest.fail("the body must not run without a model")
    assert h.host.loaded is False
    with h.host.use():
        pass
    assert h.loads == 1


def test_a_call_that_arrives_during_the_release_waits_and_then_reloads():
    h = Host()
    with h.host.use():
        pass
    h.clock.advance(601)
    h.release_may_finish.clear()
    unloader = threading.Thread(target=h.host.unload_if_idle, args=(600,))
    unloader.start()
    assert h.release_started.wait(timeout=5)

    reached_body = threading.Event()

    def call():
        with h.host.use():
            reached_body.set()

    caller = threading.Thread(target=call)
    caller.start()
    assert not reached_body.wait(timeout=0.2), "the call ran while the model was being released"

    h.release_may_finish.set()
    unloader.join(timeout=5)
    caller.join(timeout=5)
    assert reached_body.is_set()
    assert (h.loads, h.releases, h.host.loaded) == (2, 1, True)


def test_concurrent_first_calls_load_the_model_once():
    h = Host()
    start = threading.Barrier(6)
    inside = []

    def call():
        start.wait(timeout=5)
        with h.host.use() as model:
            inside.append(model)

    threads = [threading.Thread(target=call) for _ in range(6)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=5)
    assert h.loads == 1
    assert len({id(m) for m in inside}) == 1 and len(inside) == 6


def test_the_unloader_thread_releases_an_idle_model_by_itself():
    h = Host()
    with h.host.use():
        pass
    h.clock.advance(10)
    unloader = IdleUnloader(h.host, idle_seconds=1.0)
    unloader.start()
    try:
        assert h.release_started.wait(timeout=5), "the idle unloader never released the model"
    finally:
        unloader.stop()
        unloader.join(timeout=5)
    assert h.host.loaded is False
