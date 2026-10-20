"""Pinned HA scenario worker. Stdin only; no household config or HA Core start.

This worker observes the actual Script/automation helpers. Its native streams
are projected by the Go contract authority, not by a second Python comparator.
"""

import asyncio
from collections import defaultdict
from contextlib import ExitStack, suppress
from contextvars import ContextVar
from dataclasses import asdict, dataclass, replace
from datetime import datetime, timedelta
from enum import StrEnum
from hashlib import sha256
import heapq
import importlib
from importlib.metadata import version
import inspect
import json
from pathlib import Path
import socket
import sys
import time
from unittest.mock import patch
from zoneinfo import ZoneInfo

from homeassistant import config_entries, loader
from homeassistant.core import Context, CoreState, HomeAssistant, callback
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import (
    area_registry, category_registry, condition, device_registry, entity as entity_helper,
    entity_registry, floor_registry, frame, issue_registry, label_registry, restore_state,
    script, translation, trigger as trigger_helper,
)
from homeassistant.helpers import event as event_helper
from homeassistant.setup import async_setup_component
from homeassistant.util import dt as dt_util
from homeassistant.util.async_ import _SHUTDOWN_RUN_CALLBACK_THREADSAFE
import yaml


def unique_object(items):
    result = {}
    for key, value in items:
        if key in result:
            raise ValueError("duplicate JSON key: " + key)
        result[key] = value
    return result


def load_json(text):
    return json.loads(text, object_pairs_hook=unique_object,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError(value)))


def network_evidence():
    names = [name for _, name in socket.if_nameindex()]
    if names != ["lo"]:
        raise RuntimeError("qualification requires a loopback-only Linux namespace")
    routes = {}
    for key, filename in (("ipv4_routes", "/proc/net/route"), ("ipv6_routes", "/proc/net/ipv6_route")):
        text = Path(filename).read_text()
        if len(text) > 65536:
            raise RuntimeError("route evidence too large")
        for index, line in enumerate(text.splitlines()):
            fields = line.split()
            if not fields or (key == "ipv4_routes" and index == 0 and fields[0] == "Iface"):
                continue
            if fields[0 if key == "ipv4_routes" else -1] != "lo":
                raise RuntimeError("route leaves loopback")
        routes[key] = text
    return {"interfaces": names, **routes, "loopback_only": True}


@dataclass(frozen=True)
class Execution:
    raw: str
    key: str
    object: str
    branches: tuple[int, ...] = ()

    @property
    def scope(self):
        return "node/" + self.key + "/root" + "".join("/" + str(index) for index in self.branches)


@dataclass(frozen=True)
class CancellationRequest:
    key: str
    raw: str
    position: int
    stop_joined: bool


@dataclass(frozen=True)
class NativeCancellation:
    key: str
    raw: str
    terminal_position: int
    cancelled: bool
    stopped: bool
    joined: bool


@dataclass(frozen=True)
class ExpectedCancellation:
    request: CancellationRequest
    task_cancelled: bool
    parent: NativeCancellation
    children: tuple[NativeCancellation, ...]
    direct: bool


def admit_cancelled_task(evidence):
    """Admit immutable native proofs; never catch or suppress cancellation."""
    if type(evidence) is not ExpectedCancellation:
        raise RuntimeError("expected cancellation evidence absent")
    request, parent = evidence.request, evidence.parent
    if type(request) is not CancellationRequest or type(parent) is not NativeCancellation:
        raise RuntimeError("native cancellation identity absent")
    if type(evidence.task_cancelled) is not bool or not evidence.task_cancelled:
        raise RuntimeError("task is not actually cancelled")
    if type(request.stop_joined) is not bool or not request.stop_joined:
        raise RuntimeError("native stop task not joined")
    if type(evidence.direct) is not bool or type(evidence.children) is not tuple or len(evidence.children) > 64:
        raise RuntimeError("invalid cancellation child evidence")
    if not evidence.direct and evidence.children or evidence.direct and not evidence.children:
        raise RuntimeError("direct child join evidence mismatch")
    for text in (request.key, request.raw):
        if type(text) is not str or not text or len(text) > 256:
            raise RuntimeError("invalid cancellation request identity")
    if parent.key != request.key or parent.raw != request.raw:
        raise RuntimeError("native cancellation belongs to another run")
    if type(request.position) is not int or not 0 <= request.position < 1024:
        raise RuntimeError("native cancel position absent")
    seen_keys, seen_raw = set(), set()
    for native in (parent, *evidence.children):
        if type(native) is not NativeCancellation:
            raise RuntimeError("native child cancellation absent")
        if type(native.key) is not str or not native.key or len(native.key) > 256 or native.key in seen_keys:
            raise RuntimeError("invalid native cancellation key")
        if type(native.raw) is not str or not native.raw or len(native.raw) > 256 or native.raw in seen_raw:
            raise RuntimeError("invalid native cancellation raw identity")
        seen_keys.add(native.key)
        seen_raw.add(native.raw)
        if (type(native.cancelled) is not bool or not native.cancelled
                or type(native.stopped) is not bool or not native.stopped
                or type(native.joined) is not bool or not native.joined):
            raise RuntimeError("native run has not terminated as cancelled, stopped and joined")
        if type(native.terminal_position) is not int or not request.position < native.terminal_position < 1024:
            raise RuntimeError("native terminal did not follow cancellation")
        if native is not parent:
            if not native.key.startswith(parent.key + "/child/") or native.terminal_position >= parent.terminal_position:
                raise RuntimeError("direct child did not join before parent terminal")


CURRENT = ContextVar("nha045_execution", default=None)
NATIVE = ContextVar("nha045_native", default=None)
CAUSE = ContextVar("nha045_input", default=None)


@dataclass(frozen=True)
class NativePoint:
    key: str
    ordinal: int


@dataclass(frozen=True)
class StateCause:
    state: str
    action: str


@dataclass(frozen=True)
class PendingAction:
    key: str
    raw: str
    scope: str
    path: str
    failed: bool


async def turn(loop):
    # A scheduling yield, never elapsed-time evidence. The predicate determines
    # progress, and the watchdog uses the unpatched process monotonic clock.
    future = loop.create_future()
    loop.call_soon(future.set_result, None)
    await future


async def until(loop, predicate):
    deadline = time.monotonic() + 5
    while not predicate():
        if time.monotonic() >= deadline:
            raise TimeoutError("observed HA phase barrier not reached")
        await turn(loop)


class Worker:
    def __init__(self, envelope):
        if set(envelope) != {"contract", "fixture", "fixture_sha256"}:
            raise ValueError("unknown worker envelope fields")
        self.contract = envelope["contract"]
        self.fixture_text = envelope["fixture"]
        self.fixture = load_json(self.fixture_text)
        self.fixture_hash = sha256(self.fixture_text.encode()).hexdigest()
        if self.fixture_hash != envelope["fixture_sha256"] or self.fixture["schema"] != self.contract["schema"]:
            raise ValueError("fixture/contract hash mismatch")
        self.Fact = StrEnum("Fact", {row["name"]: row["token"] for row in self.contract["facts"]})
        self.Phase = StrEnum("Phase", {row["name"]: row["token"] for row in self.contract["phases"]})
        self.Outcome = StrEnum("Outcome", {row["name"]: row["token"] for row in self.contract["outcomes"]})
        self.origin = datetime.fromisoformat(self.fixture["origin"].replace("Z", "+00:00"))
        self.zone = ZoneInfo(self.fixture["zone"])
        self.elapsed_ms = 0
        self.hass = HomeAssistant("/tmp/nha045/" + self.fixture["id"])
        self.monotone_origin = self.hass.loop.time()
        self.patches = ExitStack()
        self.known = {}
        self.objects = {}
        self.native_runs = {}
        self.runs = []
        self.facts = []
        self.control = []
        self.streams = defaultdict(list)
        self.counts = defaultdict(int)
        self.tasks = {}
        self.rejections = {}
        self.cancel_requests = {}
        self.accepted_cancellations = []
        self.registrations = []
        self.timers = []
        self.branches = {}
        self.raw_nodes = []
        self.states = {}
        self.sources = []
        self.components = []
        self.phase_log = []
        self.producer = "ha-worker/" + format(id(self), "x")
        self.native_points = []
        self.state_causes = []
        self.pending_actions = []
        self.native_ordinal = 0

    def native_point(self, key):
        if (type(key) is not str or not key or len(key) > 256
                or len(self.native_points) >= 1024
                or any(point.key == key for point in self.native_points)):
            raise RuntimeError("invalid, repeated or overflowing native callback witness")
        self.native_ordinal += 1
        self.native_points.append(NativePoint(key, self.native_ordinal))

    def add(self, kind, key, execution, node, data, *, object=None, control=False, stream=False, witness=True):
        if len(self.facts) >= 1024:
            raise RuntimeError("capture_overflow: HA facts")
        row = {"key": key, "kind": kind.value, "run": execution.raw if execution else None,
               "at_ms": self.elapsed_ms, "object": object or (execution.object if execution else "house"),
               "node": node, "data": data}
        self.facts.append(row)
        if control:
            self.control.append(key)
        if stream:
            self.streams[execution.scope].append(key)
        if witness and kind is not self.Fact.Admission:
            self.native_point(key)
        return row

    def publish(self, entity, state, brightness=None, execution=None, cause=None):
        if entity not in {row["id"] for row in self.fixture["entities"]}:
            raise ValueError("unregistered synthetic state")
        attributes = {} if brightness is None else {"brightness": brightness}
        self.hass.states.async_set(entity, state, attributes)
        actual = self.hass.states.get(entity)
        if actual is None:
            raise RuntimeError("state absent after actual HA publication")
        projection = (actual.state, actual.attributes.get("brightness"))
        if self.states.get(entity) == projection:
            return
        self.states[entity] = projection
        self.counts["state/" + entity] += 1
        key = f"state/{entity}/{self.counts['state/' + entity]}"
        data = {"entity": entity, "state": projection[0]}
        if projection[1] is not None:
            data["brightness"] = projection[1]
        self.add(self.Fact.State, key, execution, "state", data, object="house", control=execution is None)
        if execution is not None:
            if type(cause) is not str or not cause or len(self.state_causes) >= 1024:
                raise RuntimeError("effect state has no actual command cause")
            self.state_causes.append(StateCause(key, cause))

    def verify_runtime(self):
        if (version("homeassistant"), sys.version.split()[0], version("jinja2")) != (
            self.contract["ha_version"], self.contract["python"], self.contract["jinja"]
        ) or version("async-interrupt") != "1.2.2":
            raise RuntimeError("version_mismatch: HA/Python/Jinja/async-interrupt")
        for pin in self.contract["sources"]:
            module = importlib.import_module(pin["path"])
            digest = sha256(Path(inspect.getfile(module)).read_bytes()).hexdigest()
            if digest != pin["sha256"]:
                raise RuntimeError("version_mismatch: source " + pin["path"])
            self.sources.append({"path": pin["path"], "sha256": digest})

    async def initialize(self):
        self.verify_runtime()
        hass = self.hass
        Path(hass.config.config_dir).mkdir(parents=True, exist_ok=True)
        hass.config.skip_pip = True
        hass.config.skip_pip_packages = []
        hass.config_entries = config_entries.ConfigEntries(hass, {"_": "nha045"})
        await hass.config_entries.async_initialize()
        entity_helper.async_setup(hass)
        loader.async_setup(hass)
        frame.async_setup(hass)
        translation.async_setup(hass)
        await translation.async_load_integrations(hass, {"homeassistant"})
        await condition.async_setup(hass)
        await trigger_helper.async_setup(hass)
        device_registry.async_setup(hass)
        for registry in (area_registry, category_registry, device_registry, entity_registry,
                         floor_registry, issue_registry, label_registry, restore_state):
            await registry.async_load(hass)
        await hass.config.async_set_time_zone(self.fixture["zone"])
        hass.set_state(CoreState.running)

        @callback
        def service(call):
            execution = CURRENT.get()
            if execution is None:
                raise RuntimeError("synthetic action outside observed Script run")
            targets = call.data.get("entity_id", [])
            if isinstance(targets, str):
                targets = [targets]
            if not targets or len(targets) > 32:
                raise ValueError("invalid synthetic action targets")
            for entity in targets:
                if not entity.startswith("switch.nha045_"):
                    raise ValueError("non-synthetic target")
                slot = execution.key + "/action/" + entity
                self.counts[slot] += 1
                failed = entity == "switch.nha045_fail"
                key = f"{slot}/{self.counts[slot]}"
                self.add(self.Fact.Action, key, execution,
                         f"{entity}/{self.counts[slot]}",
                         {"outcome": (self.Outcome.CommandFailed if failed else self.Outcome.Completed).value,
                          "entity": entity, "service": "switch.turn_on"}, stream=True, witness=False)
                if len(self.pending_actions) >= 1024:
                    raise RuntimeError("pending native action witness overflow")
                self.pending_actions.append(PendingAction(key, execution.raw, execution.scope, script.trace_path_get(), failed))
                if failed:
                    raise HomeAssistantError("nha045 synthetic command failure")
                self.publish(entity, "on", execution=execution, cause=key)

        hass.services.async_register("switch", "turn_on", service)
        for state in self.fixture["entities"]:
            self.publish(state["id"], state["state"], state["brightness"])
        scripts = {}
        automations = []
        for item in self.fixture["objects"]:
            document = yaml.safe_load(item["yaml"])
            if item["id"].startswith("script."):
                scripts[item["id"].split(".", 1)[1]] = document
            else:
                automations.append(document)
        if scripts:
            if not await async_setup_component(hass, "script", {"script": scripts}):
                raise RuntimeError("real script setup failed")
            component = hass.data["script"]
            self.components.append(component)
            for entity in component.entities:
                self.known[id(entity.script)] = entity.entity_id
                self.objects[entity.entity_id] = entity.script
        if automations:
            if not await async_setup_component(hass, "automation", {"automation": automations}):
                raise RuntimeError("real automation setup failed")
            component = hass.data["automation"]
            self.components.append(component)
            for entity in component.entities:
                # IDs of the selected corpus are explicit, independent of HA's
                # generated entity slug (which is not an execution identity).
                object_id = self.fixture["root"]
                self.known[id(entity.action_script)] = object_id
                self.objects[object_id] = entity.action_script
        await hass.async_block_till_done()
        self.install_observers()

    def install_observers(self):
        hass = self.hass
        original_init = script._ScriptRun.__init__
        original_run = script._ScriptRun.async_run
        original_reason = script.script_execution_set
        original_template = script.async_track_template
        original_trigger = script.trigger_helper.async_initialize_triggers
        original_parallel = script.Script._async_get_parallel_scripts
        original_trace = script.trace_action
        original_call_at = hass.loop.call_at
        worker = self

        def run_init(native, *args, **kwargs):
            original_init(native, *args, **kwargs)
            object_id = worker.known.get(id(native._script))
            if object_id is None:
                return
            parent = CURRENT.get()
            if object_id.startswith("automation."):
                key, parent_key, callsite = "auto/1", None, None
            elif object_id == worker.fixture["root"]:
                key, parent_key, callsite = CAUSE.get(), None, None
            else:
                if parent is None:
                    raise RuntimeError("orphan HA child")
                key, parent_key, callsite = parent.key + "/child/1", parent.key, "child/1"
            if key is None or any(row["key"] == key for row in worker.runs):
                raise RuntimeError("ambiguous or duplicated HA run identity")
            if len(worker.runs) >= 64:
                raise RuntimeError("native HA run witness overflow")
            execution = Execution("ha-" + format(id(native), "x"), key, object_id)
            worker.native_runs[id(native)] = {"native": native, "execution": execution,
                                              "started": False, "terminal": False, "reason": None,
                                              "outcome": None, "terminal_position": None, "task": None}
            worker.runs.append({"key": key, "raw": execution.raw, "object": object_id,
                                "parent": parent_key, "callsite": callsite})
            outcome = worker.Outcome.Queued if native._script.script_mode == "queued" and native._script.runs else worker.Outcome.Accepted
            worker.add(worker.Fact.Admission, key + "/admission", execution, "admission", {"outcome": outcome.value})

        async def run(native):
            record = worker.native_runs.get(id(native))
            inherited = CURRENT.get()
            execution = record["execution"] if record else inherited
            branches = worker.branches.get(id(native._script))
            if execution is not None and branches is not None:
                execution = Execution(execution.raw, execution.key, execution.object, branches)
            token = CURRENT.set(execution)
            native_token = NATIVE.set(native)
            if record:
                record["started"] = True
                record["task"] = asyncio.current_task()
                worker.add(worker.Fact.Started, execution.key + "/start", execution, "entry", {}, control=True)
            failure = None
            try:
                return await original_run(native)
            except BaseException as error:
                failure = error
                raise
            finally:
                try:
                    if record:
                        if isinstance(failure, asyncio.CancelledError):
                            if not native._stop.done():
                                raise failure
                            # Preserve the original CancelledError propagation;
                            # record only what the real ScriptRun stop proves.
                            outcome = worker.Outcome.Cancelled
                        elif failure is not None:
                            if type(failure) is not HomeAssistantError or failure.args != ("nha045 synthetic command failure",):
                                raise failure
                            outcome = worker.Outcome.CommandFailed
                        elif native._stop.done():
                            outcome = worker.Outcome.Cancelled
                        elif record["reason"] == "aborted":
                            outcome = worker.Outcome.TimeoutStopped
                        else:
                            outcome = worker.Outcome.Finished
                        worker.add(worker.Fact.Terminal, execution.key + "/terminal", execution, "terminal", {"outcome": outcome.value}, control=True)
                        record["outcome"] = outcome
                        record["terminal_position"] = len(worker.control) - 1
                        record["terminal"] = True
                finally:
                    NATIVE.reset(native_token)
                    CURRENT.reset(token)

        def reason(value, response=None):
            original_reason(value, response)
            native = NATIVE.get()
            record = worker.native_runs.get(id(native)) if native is not None else None
            if record:
                record["reason"] = value
            elif CAUSE.get() is not None and value in ("failed_single", "failed_max_runs"):
                key = CAUSE.get()
                outcome = worker.Outcome.SingleBusy if value == "failed_single" else worker.Outcome.MaxExceeded
                worker.rejections[key] = outcome
                worker.add(worker.Fact.Admission, key + "/admission", None, "admission", {"outcome": outcome.value}, object=worker.fixture["root"])

        def observe_subscription(kind, original, *args, **kwargs):
            unsubscribe = original(*args, **kwargs)
            execution = CURRENT.get()
            if execution is None:
                return unsubscribe
            entry = {"kind": kind, "run": execution.raw, "object": execution.object, "closed": False}
            worker.registrations.append(entry)

            @callback
            def close():
                entry["closed"] = True
                return unsubscribe()
            return close

        def track_template(*args, **kwargs):
            return observe_subscription("template", original_template, *args, **kwargs)

        async def initialize_triggers(*args, **kwargs):
            unsubscribe = await original_trigger(*args, **kwargs)
            if not unsubscribe:
                raise RuntimeError("actual trigger registration failed")
            return observe_subscription("trigger", lambda: unsubscribe)

        async def parallel(instance, step):
            children = await original_parallel(instance, step)
            current = CURRENT.get()
            if current:
                for index, child in enumerate(children):
                    worker.branches[id(child)] = current.branches + (index,)
            return children

        class Trace(original_trace):
            def __init__(self, hass_, native, stop, variables):
                super().__init__(hass_, native, stop, variables)
                self.native = native

            async def __aenter__(self):
                result = await super().__aenter__()
                self.path = script.trace_path_get()
                worker.raw_nodes.append({"path": script.trace_path_get(), "phase": "enter", "at_ms": worker.elapsed_ms})
                return result

            async def __aexit__(self, exc_type, exc_value, exc_tb):
                try:
                    return await super().__aexit__(exc_type, exc_value, exc_tb)
                finally:
                    native = self.native
                    action = native._action
                    execution = CURRENT.get()
                    if execution is not None:
                        matches = [row for row in worker.pending_actions
                                   if row.raw == execution.raw and row.scope == execution.scope and row.path == self.path]
                        if len(matches) > 1:
                            raise RuntimeError("ambiguous action result at its native trace exit")
                        for row in matches:
                            if (row.failed and (type(exc_value) is not HomeAssistantError
                                                or exc_value.args != ("nha045 synthetic command failure",))
                                    or not row.failed and exc_value is not None):
                                raise RuntimeError("actual command result contradicts its native trace exit")
                            worker.native_point(row.key)
                            worker.pending_actions.remove(row)
                    if execution is not None and ("wait_template" in action or "wait_for_trigger" in action):
                        value = native._variables.get("wait")
                        if type(value) is not dict or type(value.get("completed")) is not bool:
                            raise TypeError("HA wait variables absent or invalid")
                        remaining = value.get("remaining")
                        if remaining is not None:
                            if type(remaining) not in (int, float) or abs(remaining * 1000 - round(remaining * 1000)) > 0.000001:
                                raise TypeError("HA remaining is not exact milliseconds")
                            remaining = round(remaining * 1000)
                        trigger = value.get("trigger")
                        if trigger is not None:
                            trigger = {"id": trigger["id"], "index": int(trigger["idx"]), "platform": trigger["platform"]}
                        outcome = worker.Outcome.Completed
                        if native._stop.done():
                            outcome = worker.Outcome.Cancelled
                        elif isinstance(exc_value, script._AbortScript) and isinstance(exc_value.__cause__, TimeoutError):
                            outcome = worker.Outcome.TimeoutStopped
                        worker.counts[execution.key + "/wait"] += 1
                        node = "wait/" + str(worker.counts[execution.key + "/wait"])
                        worker.add(worker.Fact.Wait, execution.key + "/" + node, execution, node,
                                   {"outcome": outcome.value, "wait": {"completed": value["completed"],
                                    "remaining_ms": remaining, "trigger": trigger}}, stream=True)

        def call_at(when, function, *args, context=None):
            handle = original_call_at(when, function, *args, context=context)
            module = getattr(function, "__module__", "")
            name = getattr(function, "__name__", "")
            relevant = (module == script.__name__ and name == "_set_result_unless_done")
            if module == event_helper.__name__ and name == "_run_async_call_action" and len(args) == 2:
                relevant = getattr(args[1].target, "__name__", "") == "state_for_listener"
            if relevant:
                if len(worker.timers) >= 1024:
                    raise RuntimeError("HA timer bound exceeded")
                # Preserve the actual TimerHandle and callback; remove only its
                # automatic dispatch so each virtual advance is explicit.
                hass.loop._scheduled.remove(handle)
                heapq.heapify(hass.loop._scheduled)
                execution = CURRENT.get()
                worker.timers.append({"handle": handle, "deadline_ms": round((when - worker.monotone_origin) * 1000),
                                      "run": execution.raw if execution else None, "callback": module + "." + name,
                                      "fired": False})
            return handle

        self.patches.enter_context(patch.object(script._ScriptRun, "__init__", run_init))
        self.patches.enter_context(patch.object(script._ScriptRun, "async_run", run))
        self.patches.enter_context(patch.object(script, "script_execution_set", reason))
        self.patches.enter_context(patch.object(script, "async_track_template", track_template))
        self.patches.enter_context(patch.object(script.trigger_helper, "async_initialize_triggers", initialize_triggers))
        self.patches.enter_context(patch.object(script.Script, "_async_get_parallel_scripts", parallel))
        self.patches.enter_context(patch.object(script, "trace_action", Trace))
        self.patches.enter_context(patch.object(hass.loop, "call_at", call_at))
        self.patches.enter_context(patch.object(hass.loop, "time", lambda: self.monotone_origin + self.elapsed_ms / 1000))
        self.patches.enter_context(patch.object(dt_util, "utcnow", lambda: self.origin + timedelta(milliseconds=self.elapsed_ms)))
        self.patches.enter_context(patch.object(dt_util, "now", lambda time_zone=None: (self.origin + timedelta(milliseconds=self.elapsed_ms)).astimezone(time_zone or self.zone)))
        self.patches.enter_context(patch.object(script, "utcnow", lambda: self.origin + timedelta(milliseconds=self.elapsed_ms)))
        self.patches.enter_context(patch.object(event_helper, "time_tracker_utcnow", lambda: self.origin + timedelta(milliseconds=self.elapsed_ms)))
        self.patches.enter_context(patch.object(event_helper, "time_tracker_timestamp", lambda: self.origin.timestamp() + self.elapsed_ms / 1000))

    def record_for_key(self, key):
        return next((record for record in self.native_runs.values() if record["execution"].key == key), None)

    def admit_expected_cancellation(self, key, task):
        request = self.cancel_requests.get(key)
        if type(request) is not CancellationRequest:
            raise RuntimeError("cancelled task had no explicit native stop request")
        if not 0 <= request.position < len(self.control) or self.control[request.position] != key + "/cancel":
            raise RuntimeError("native cancellation request witness missing")

        def snapshot(record):
            if record is None or not record["terminal"]:
                raise RuntimeError("native cancelled terminal missing")
            execution, position = record["execution"], record["terminal_position"]
            if type(position) is not int or not 0 <= position < len(self.control) or self.control[position] != execution.key + "/terminal":
                raise RuntimeError("native terminal position witness missing")
            return NativeCancellation(execution.key, execution.raw, position,
                                      record["outcome"] is self.Outcome.Cancelled,
                                      record["native"]._stopped.is_set(),
                                      record["task"] is not None and record["task"].done())

        parent = snapshot(self.record_for_key(key))
        direct = self.fixture["id"] == "08-direct-parent-cancel"
        children = tuple(snapshot(self.record_for_key(row["key"]))
                         for row in self.runs if row["parent"] == key) if direct else ()
        evidence = ExpectedCancellation(request, task.cancelled(), parent, children, direct)
        admit_cancelled_task(evidence)
        if len(self.accepted_cancellations) >= 64:
            raise RuntimeError("cancellation proof bound exceeded")
        self.accepted_cancellations.append(evidence)

    async def execute_phase(self, phase):
        kind = self.Phase(phase["kind"])
        if kind is not self.Phase.Advance and phase["at_ms"] != self.elapsed_ms:
            raise ValueError("missing explicit clock advance")
        if kind is self.Phase.Advance:
            if phase["at_ms"] < self.elapsed_ms:
                raise ValueError("nonmonotone virtual time")
            before = sum(record["terminal"] for record in self.native_runs.values())
            self.elapsed_ms = phase["at_ms"]
            due = 0
            for timer in sorted(self.timers, key=lambda item: item["deadline_ms"]):
                if not timer["fired"] and not timer["handle"].cancelled() and timer["deadline_ms"] <= self.elapsed_ms:
                    timer["fired"] = True
                    due += 1
                    timer["handle"]._run()
                    timer["handle"].cancel()
            if due:
                await until(self.hass.loop, lambda: sum(record["terminal"] for record in self.native_runs.values()) >= before + due)
        elif kind is self.Phase.Invoke:
            async def invoke():
                token = CAUSE.set(phase["run"])
                try:
                    return await self.objects[phase["object"]].async_run(context=Context())
                finally:
                    CAUSE.reset(token)
            task = self.hass.async_create_task(invoke())
            self.tasks[phase["run"]] = task
            await until(self.hass.loop, lambda: self.record_for_key(phase["run"]) is not None or task.done())
            if task.done() and task.exception() is not None:
                raise task.exception()
        elif kind is self.Phase.AwaitStartup:
            def ready():
                if phase["run"] in self.rejections:
                    return True
                record = self.record_for_key(phase["run"])
                if record is None:
                    return False
                if self.objects[phase["object"]].script_mode == "queued" and not record["started"] and phase["at_ms"] < 5000:
                    return True
                return record["terminal"] or any(timer["run"] == record["execution"].raw for timer in self.timers)
            await until(self.hass.loop, ready)
        elif kind is self.Phase.AwaitWaiting:
            await until(self.hass.loop, lambda: any(row["object"] == phase["object"] and not row["closed"] for row in self.registrations))
        elif kind is self.Phase.AwaitTerminal:
            task = self.tasks[phase["run"]]
            await until(self.hass.loop, task.done)
            if task.cancelled():
                self.admit_expected_cancellation(phase["run"], task)
            else:
                error = task.exception()
                if error is not None and (type(error) is not HomeAssistantError or error.args != ("nha045 synthetic command failure",)):
                    raise error
        elif kind is self.Phase.AwaitRunTerminal:
            await until(self.hass.loop, lambda: any(row["execution"].object == phase["object"] and row["terminal"] for row in self.native_runs.values()))
        elif kind is self.Phase.Cancel:
            record = self.record_for_key(phase["run"])
            if record is None:
                raise RuntimeError("cancel has no admitted run")
            execution = record["execution"]
            self.add(self.Fact.Cancellation, execution.key + "/cancel", execution, "cancel", {}, control=True)
            request = CancellationRequest(execution.key, execution.raw, len(self.control) - 1, False)
            self.cancel_requests[execution.key] = request
            task = self.hass.async_create_task(self.objects[phase["object"]].async_stop())
            await until(self.hass.loop, task.done)
            task.result()
            self.cancel_requests[execution.key] = replace(request, stop_joined=True)
        elif kind is self.Phase.Publish:
            self.publish(phase["entity"], phase["state"], phase["brightness"])
            await self.hass.async_block_till_done()
        elif kind is self.Phase.Emit:
            if phase["entity"] != "nha045_release":
                raise ValueError("non-synthetic event")
            self.hass.bus.async_fire("nha045_release")
            # The following terminal barrier observes actual callback handling.
            await turn(self.hass.loop)
        elif kind is self.Phase.AwaitActions:
            await until(self.hass.loop, lambda: sum(row["kind"] == self.Fact.Action.value for row in self.facts) >= phase["count"])
        else:
            raise ValueError("unhandled phase variant")
        self.phase_log.append(phase)

    async def close(self):
        errors = []
        for component in reversed(self.components):
            for entity in list(component.entities):
                try:
                    await component.async_remove_entity(entity.entity_id)
                except BaseException as error:
                    errors.append(error)
        for task in self.tasks.values():
            if not task.done():
                task.cancel()
        if self.tasks:
            await asyncio.gather(*self.tasks.values(), return_exceptions=True)
        for timer in self.timers:
            timer["handle"].cancel()
        self.patches.close()
        try:
            await self.hass.async_stop(force=True)
        except BaseException as error:
            errors.append(error)
        with suppress(AttributeError):
            delattr(self.hass.loop, _SHUTDOWN_RUN_CALLBACK_THREADSAFE)
        if errors:
            raise BaseExceptionGroup("HA cleanup failed", errors)
        cleanup = {"tasks_done": all(task.done() for task in self.tasks.values()),
                   "entities_removed": all(not list(component.entities) for component in self.components),
                   "subscriptions_closed": all(row["closed"] for row in self.registrations),
                   "timers_terminal": all(row["handle"].cancelled() for row in self.timers),
                   "hass_stopped": self.hass.state is CoreState.stopped}
        if not all(cleanup.values()):
            raise RuntimeError("HA cleanup terminal evidence incomplete")
        return cleanup

    async def collect(self):
        network = network_evidence()
        primary = None
        result = None
        cleanup = None
        completed_signature = None
        try:
            await self.initialize()
            for phase in self.fixture["phases"]:
                await self.execute_phase(phase)
            if not self.runs or any(not row["terminal"] for row in self.native_runs.values()):
                raise RuntimeError("capture before actual HA terminal")
            if any(not row["closed"] for row in self.registrations):
                raise RuntimeError("HA wait subscription survived terminal")
            for key, task in self.tasks.items():
                await until(self.hass.loop, task.done)
                if task.cancelled():
                    if not any(proof.request.key == key for proof in self.accepted_cancellations):
                        self.admit_expected_cancellation(key, task)
                else:
                    error = task.exception()
                    if error is not None and (type(error) is not HomeAssistantError or error.args != ("nha045 synthetic command failure",)):
                        raise error
            for record in self.native_runs.values():
                native, task = record["native"], record["task"]
                if task is None:
                    raise RuntimeError("native terminal has no owning task")
                await until(self.hass.loop, lambda task=task: task.done())
                if not native._stopped.is_set():
                    raise RuntimeError("native terminal run not stopped and joined")
                if task.cancelled():
                    if not any(record["execution"].raw == joined.raw
                               for proof in self.accepted_cancellations
                               for joined in (proof.parent, *proof.children)):
                        raise RuntimeError("native task cancelled without an admitted stop and join")
                else:
                    error = task.exception()
                    if error is not None and (type(error) is not HomeAssistantError or error.args != ("nha045 synthetic command failure",)):
                        raise error
            if self.pending_actions:
                raise RuntimeError("actual action result has no native trace-exit witness")
            completed_signature = self.order_signature()
            result = {"schema": self.contract["schema"], "scenario": self.fixture["id"],
                    "fixture_sha256": self.fixture_hash,
                    "runtime": {"name": "ha", "version": version("homeassistant"), "python": sys.version.split()[0],
                                "jinja": version("jinja2"), "source_sha256": self.sources},
                    "runs": self.runs, "facts": self.facts, "edges": [],
                    "orders": [{"scope": "control", "keys": self.control}] +
                              [{"scope": scope, "keys": keys} for scope, keys in self.streams.items()],
                    "evicted": 0, "terminal": True,
                    "raw": {"producer": self.producer,
                            "order": {"producer": self.producer, "healthy": True, "joined": True,
                                      "decision_evicted": 0, "node_evicted": 0,
                                      "points": [asdict(point) for point in self.native_points],
                                      "state_causes": [asdict(cause) for cause in self.state_causes]},
                            "phases": self.phase_log, "nodes": self.raw_nodes,
                            "registrations": self.registrations, "network": network,
                            "cancellations": [asdict(proof) for proof in self.accepted_cancellations],
                            "timers": [{key: value for key, value in timer.items() if key != "handle"} |
                                       {"cancelled": timer["handle"].cancelled()} for timer in self.timers]}}
        except BaseException as error:
            primary = error
            raise
        finally:
            try:
                cleanup = await self.close()
            except BaseException as cleanup:
                if primary is None:
                    raise
                print("NHA045_CLEANUP " + repr(cleanup), file=sys.stderr, flush=True)
        result["raw"]["cleanup"] = cleanup
        if completed_signature != self.order_signature():
            raise RuntimeError("HA cleanup manufactured a native fact or ordering witness")
        return result

    def order_signature(self):
        return json.dumps({"runs": self.runs, "facts": self.facts, "control": self.control,
                           "streams": self.streams, "points": [asdict(point) for point in self.native_points],
                           "causes": [asdict(cause) for cause in self.state_causes], "ordinal": self.native_ordinal},
                          sort_keys=True, allow_nan=False)


async def run_worker(envelope):
    # HomeAssistant binds to the currently running event loop at construction.
    return await Worker(envelope).collect()


if __name__ == "__main__":
    text = sys.stdin.read((1 << 20) + 65537)
    if len(text) > (1 << 20) + 65536:
        raise ValueError("worker input exceeds bound")
    result = asyncio.run(run_worker(load_json(text)))
    encoded = json.dumps(result, sort_keys=True, allow_nan=False)
    if len(encoded.encode()) > 8 << 20:
        raise RuntimeError("HA capture output exceeds bound")
    print(encoded)
