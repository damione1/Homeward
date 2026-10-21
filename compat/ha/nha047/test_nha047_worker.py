"""Pure consumption oracles; importing the producer never imports HA."""

from hashlib import sha256
import importlib.util
from pathlib import Path
import sys

import pytest


def worker_module():
    name = "nha047_worker_consumption"
    if name not in sys.modules:
        spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name("ha_worker.py"))
        module = importlib.util.module_from_spec(spec)
        sys.modules[name] = module
        spec.loader.exec_module(module)
    return sys.modules[name]


def test_nha047_real_loader_handoff_hashes_the_bytes_it_receives():
    worker = worker_module()
    original = b"actions:\n- service: switch.turn_on\n"
    candidate = b"actions:\n- action: switch.turn_on\n"
    seen = []
    def observed_loader(buffer):
        seen.append(buffer)
        return {"observed": buffer.decode()}
    # The inverted input is deliberately real at the consumption port.
    consumed = worker.consume_yaml(candidate, observed_loader)
    assert seen == [candidate]
    assert consumed.document == {"observed": candidate.decode()}
    assert consumed.input_sha256 == sha256(seen[0]).hexdigest()
    assert consumed.input_sha256 != sha256(original).hexdigest()


@pytest.mark.parametrize("buffer", [b"", "not-bytes", b"x" * ((1 << 20) + 1)])
def test_nha047_loader_rejects_unadmitted_buffers_before_loading(buffer):
    calls = []
    with pytest.raises(ValueError):
        worker_module().consume_yaml(buffer, lambda value: calls.append(value))
    assert calls == []


def test_nha047_consumption_preserves_native_annotated_mapping_subclass():
    class AnnotatedMapping(dict):
        pass
    native = AnnotatedMapping({"actions": []})
    observed = worker_module().consume_yaml(b"actions: []\n", lambda buffer: native)
    assert observed.document is native
    assert observed.input_sha256 == sha256(b"actions: []\n").hexdigest()


def test_nha047_virtual_clock_preserves_wall_deadline_and_native_timer_boundary():
    import asyncio
    from contextlib import ExitStack
    from datetime import datetime, timezone
    from types import SimpleNamespace
    import time

    worker = worker_module()
    origin = datetime.fromisoformat("2024-03-11T00:00:00-04:00")
    elapsed = [0.0]
    loop = asyncio.new_event_loop()
    dt_util = SimpleNamespace(utcnow=lambda: datetime.now(timezone.utc),
                              now=lambda time_zone=None: datetime.now(time_zone),
                              get_default_time_zone=lambda: origin.tzinfo)
    script = SimpleNamespace(utcnow=dt_util.utcnow)
    event_helper = SimpleNamespace(time=time, time_tracker_timestamp=time.time,
                                   time_tracker_utcnow=dt_util.utcnow)
    original_wall_time = time.time
    requested = origin.timestamp() + 9 * 3600
    # A submicrosecond point catches datetime rounding in the wall clock.
    deadline = worker.trigger_deadline([requested + 0.1480744], requested)
    assert deadline != datetime.fromtimestamp(deadline, timezone.utc).timestamp()
    observed = []

    async def turn():
        await asyncio.sleep(0)

    try:
        with ExitStack() as patches:
            worker.patch_qualification_clock(patches, loop, dt_util, script,
                                              event_helper, origin, lambda: elapsed[0])
            assert dt_util.utcnow().utcoffset().total_seconds() == 0
            assert dt_util.now().isoformat() == origin.isoformat()
            assert dt_util.utcnow() == script.utcnow() == event_helper.time_tracker_utcnow()
            assert time.time() == event_helper.time_tracker_timestamp() == origin.timestamp()
            # This is HA _TrackPointUTCTime.async_attach's actual conversion.
            handle = loop.call_at(loop.time() + deadline - event_helper.time.time(),
                                  lambda: observed.append(event_helper.time_tracker_timestamp()))
            assert handle.when() == deadline - origin.timestamp() > 0
            loop.run_until_complete(turn())
            assert observed == []
            elapsed[0] = deadline - origin.timestamp() - 0.001
            loop.run_until_complete(turn())
            assert observed == []
            assert event_helper.time_tracker_timestamp() < deadline
            elapsed[0] = deadline - origin.timestamp()
            assert event_helper.time_tracker_timestamp() == deadline
            loop.run_until_complete(turn())
            assert observed == [deadline]
            assert abs(dt_util.utcnow().timestamp() - deadline) <= 0.000001
        assert time.time is original_wall_time
    finally:
        loop.close()


def test_nha047_native_trigger_deadline_rejects_missing_or_ambiguous_timer():
    worker = worker_module()
    for points in ([], [1710162001.0], [1710162000.1, 1710162000.2]):
        with pytest.raises(ValueError, match="native trigger deadline missing or ambiguous"):
            worker.trigger_deadline(points, 1710162000.0)
