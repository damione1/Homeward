"""Observe real HA scripts inside the pinned, networkless local image.

This worker never starts HA Core, opens an integration connection, or imports
household configuration. Every entity and service handler is synthetic. Timer
callbacks are advanced using the technique in pinned HA tests/common.py.
"""

import asyncio
from contextlib import ExitStack, suppress
from datetime import timedelta
from hashlib import sha256
from importlib.metadata import version
import inspect
import json
from pathlib import Path
import sys
from unittest.mock import patch

from homeassistant.components import script as script_component
from homeassistant import config_entries, loader
from homeassistant.core import Context, CoreState, HomeAssistant, callback
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import config_validation as cv, script
from homeassistant.helpers import event as event_helper, script_variables
from homeassistant.helpers import (
    area_registry, category_registry, condition, device_registry, entity as entity_helper,
    entity_registry, floor_registry, issue_registry, label_registry, restore_state, trigger as trigger_helper, frame, translation,
)
from homeassistant.setup import async_setup_component
import homeassistant.const
from homeassistant.util import dt as dt_util
from homeassistant.util.async_ import _SHUTDOWN_RUN_CALLBACK_THREADSAFE


def switch_action(name):
    if not name.startswith("switch.nha042_"):
        raise ValueError("non-synthetic target")
    return {"action": "switch.turn_on", "target": {"entity_id": name}, "data": {}}


async def prepare_helper(hass, sequence, name):
    sequence = cv.SCRIPT_SCHEMA(sequence)
    sequence = await script.async_validate_actions_config(hass, sequence)
    return script.Script(hass, sequence, name, "nha042", log_exceptions=False)


async def settle(hass):
    await asyncio.wait_for(hass.async_block_till_done(), timeout=5)


async def wait_until(predicate):
    # Like upstream async_watch_for_action, wait for a real observed phase.
    # async_block_till_done would wait for the intentionally active Script too.
    async with asyncio.timeout(5):
        while not predicate():
            await asyncio.sleep(0)


async def observe_case(case):
    hass = HomeAssistant("/tmp/nha042/" + case["id"])
    effects = []

    @callback
    def record(call):
        targets = call.data.get("entity_id", [])
        if isinstance(targets, str):
            targets = [targets]
        for target in targets:
            if not target.startswith("switch.nha042_"):
                raise ValueError("non-synthetic service target")
            effects.append(target)
            if target == "switch.nha042_fail":
                raise HomeAssistantError("synthetic branch error")

    hass.services.async_register("switch", "turn_on", record)
    hass.states.async_set("switch.nha042_gate", "off")
    instance = task = None
    component = None
    patches = ExitStack()
    template_registrations = []
    original_track_template = script.async_track_template

    @callback
    def observed_track_template(hass_, template_, action_, *args, **kwargs):
        unsub = original_track_template(hass_, template_, action_, *args, **kwargs)
        registration = {"template": template_.template, "unsubscribe_calls": 0}
        template_registrations.append(registration)

        @callback
        def observed_unsubscribe():
            registration["unsubscribe_calls"] += 1
            return unsub()

        return observed_unsubscribe

    patches.enter_context(patch.object(script, "async_track_template", observed_track_template))
    try:
        # Same real initialization order as pinned tests/common.py:217-352.
        # All stores are empty in this container's synthetic /tmp config.
        Path(hass.config.config_dir).mkdir(parents=True, exist_ok=True)
        hass.config.skip_pip = True
        hass.config.skip_pip_packages = []
        hass.config_entries = config_entries.ConfigEntries(hass, {"_": "nha042"})
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
        await hass.config.async_set_time_zone("America/Toronto")
        hass.set_state(CoreState.running)
        kind = case["kind"]
        if kind == "call":
            child = {
                "mode": "parallel",
                "sequence": [
                    switch_action("switch.nha042_child_enter"),
                    {"wait_template": "{{ is_state('switch.nha042_gate', 'on') }}"},
                    switch_action("switch.nha042_child_exit"),
                ],
            }
            if not await async_setup_component(hass, "script", {"script": {"nha042_child": child}}):
                raise RuntimeError("real script component setup failed")
            await settle(hass)
            component = hass.data["script"]
            call = {"action": "script.nha042_child"}
            if case["mode"] == "detached":
                call = {"action": "script.turn_on", "target": {"entity_id": "script.nha042_child"}}
            parent = [call, switch_action("switch.nha042_parent_after_call")]
            if case["cancel_parent"]:
                parent += [{"wait_template": "{{ false }}"}, switch_action("switch.nha042_parent_exit")]
            instance = await prepare_helper(hass, parent, case["id"])
            child_entity = component.get_entity("script.nha042_child")
            if child_entity is None:
                raise RuntimeError("real script entity was not installed")
            task = hass.async_create_task(instance.async_run(context=Context()))
            gate_registered = lambda: any("switch.nha042_gate" in item["template"] for item in template_registrations)
            if case["mode"] == "detached":
                # The parent may finish before the detached child registers its
                # wait. That is valid; only a failed parent aborts this barrier.
                await wait_until(lambda: gate_registered() or
                                 (task.done() and (task.cancelled() or task.exception() is not None)))
                if not gate_registered():
                    await task
                    raise RuntimeError("parent failed before the child wait subscription existed")
                if case["cancel_parent"]:
                    parent_registered = lambda: any(item["template"].strip() == "{{ false }}"
                                                    for item in template_registrations)
                    await wait_until(lambda: parent_registered() or task.done())
                    if not parent_registered():
                        await task
                        raise RuntimeError("parent ended before its own cancellation wait existed")
                else:
                    await asyncio.wait_for(asyncio.shield(task), 5)
            else:
                await wait_until(lambda: gate_registered() or task.done())
                if not gate_registered():
                    await task
                    raise RuntimeError("direct call ended before the child wait subscription existed")
            before = {"effects": list(effects), "parent_running": instance.is_running, "child_running": child_entity.script.is_running}
            if case["cancel_parent"]:
                await instance.async_stop()
            after_cancel = {"effects": list(effects), "parent_running": instance.is_running, "child_running": child_entity.script.is_running}
            hass.states.async_set("switch.nha042_gate", "on")
            # Cancellation is a terminal observation, not a collector failure.
            # Keep the timeout independent of the parent; cleanup below still
            # unloads and joins it if this bounded collection itself fails.
            values = await asyncio.wait_for(
                asyncio.shield(asyncio.gather(task, return_exceptions=True)), 5
            )
            parent_result = values[0]
            if isinstance(parent_result, BaseException) and not isinstance(parent_result, asyncio.CancelledError):
                raise parent_result
            if parent_result is not None and not isinstance(parent_result, (script.ScriptRunResult, asyncio.CancelledError)):
                raise TypeError(f"unexpected parent Script result: {type(parent_result).__name__}")
            await wait_until(lambda: not child_entity.script.is_running)
            return {"id": case["id"], "before": before, "after_cancel": after_cancel,
                    "final_effects": list(effects), "parent_running": instance.is_running,
                    "child_running": child_entity.script.is_running,
                    "parent_result": {"type": type(parent_result).__name__,
                                      "task_cancelled": task.cancelled(),
                                      "stop_requested": case["cancel_parent"],
                                      "wait": parent_result.variables.get("wait") if isinstance(parent_result, script.ScriptRunResult) else None},
                    "template_subscriptions": template_registrations}

        if kind == "zero":
            hass.states.async_set("switch.nha042_gate", "on" if case["initial"] else "off")
            wait = {"timeout": {"seconds": 0}, "continue_on_timeout": True}
            if case["wait"] == "template":
                wait["wait_template"] = "{{ is_state('switch.nha042_gate', 'on') }}"
            else:
                wait["wait_for_trigger"] = [{"trigger": "state", "entity_id": "switch.nha042_gate", "to": "on"}]
            instance = await prepare_helper(hass, [wait, switch_action("switch.nha042_after_wait")], case["id"])
            result = await instance.async_run(context=Context())
            return {"id": case["id"], "effects": list(effects), "wait": result.variables.get("wait"), "running": instance.is_running}

        if kind == "race":
            callbacks = []
            elapsed = [0.0]
            monotone_origin = hass.loop.time()
            wall_origin = dt_util.utcnow()
            timeout_handle = [None]
            trigger_registered = [False]
            timeout_future = [None]
            due_elapsed = [None]
            original_done = script._set_result_unless_done
            original_initialize = script.trigger_helper.async_initialize_triggers

            def timed_done(future):
                label = "timeout" if future is timeout_future[0] else "trigger_future_completion"
                callbacks.append({"callback": label, "elapsed": elapsed[0], "future_already_done": future.done()})
                return original_done(future)

            timed_done.__module__ = script.__name__
            timed_done.__name__ = "_set_result_unless_done"

            async def initialize(hass_, config_, action_, *args, **kwargs):
                async def observed_action(variables, context=None):
                    callbacks.append({"callback": "trigger_enter", "elapsed": elapsed[0]})
                    await action_(variables, context)
                    callbacks.append({"callback": "trigger_completed", "elapsed": elapsed[0]})
                    if case["order"] == "event-callback-first":
                        # Process the real trigger at t=59 before advancing both
                        # clocks to the actual t=60 deadline, without yielding
                        # back to the Script waiter between those completions.
                        elapsed[0] = due_elapsed[0]
                        timeout_handle[0]._run()
                        timeout_handle[0].cancel()
                remove = await original_initialize(hass_, config_, observed_action, *args, **kwargs)
                trigger_registered[0] = bool(remove)
                return remove

            # Advance the real loop's clock and HA wall-clock observations
            # together, then run the actual TimerHandle, as upstream tests do.
            patches.enter_context(patch.object(hass.loop, "time", lambda: monotone_origin + elapsed[0]))
            patches.enter_context(patch.object(dt_util, "utcnow", lambda: wall_origin + timedelta(seconds=elapsed[0])))
            patches.enter_context(patch.object(script, "utcnow", lambda: wall_origin + timedelta(seconds=elapsed[0])))
            patches.enter_context(patch.object(event_helper, "time_tracker_utcnow", lambda: wall_origin + timedelta(seconds=elapsed[0])))
            patches.enter_context(patch.object(event_helper, "time_tracker_timestamp", lambda: wall_origin.timestamp() + elapsed[0]))
            patches.enter_context(patch.object(script, "_set_result_unless_done", timed_done))
            patches.enter_context(patch.object(script.trigger_helper, "async_initialize_triggers", initialize))
            wait = {"wait_for_trigger": [{"trigger": "event", "event_type": "nha042_release"}],
                    "timeout": {"seconds": 60}, "continue_on_timeout": case["continue_on_timeout"]}
            instance = await prepare_helper(hass, [wait, switch_action("switch.nha042_after_race")], case["id"])
            task = hass.async_create_task(instance.async_run(context=Context()))
            await wait_until(lambda: task.done() or trigger_registered[0])
            if task.done():
                await task
                raise RuntimeError("Script ended before real trigger initialization completed")
            # This is the real callback created by script._async_futures_with_timeout.
            # Selecting it avoids advancing unrelated HA background timers.
            handles = [handle for handle in list(hass.loop._scheduled)
                       if isinstance(handle, asyncio.TimerHandle) and not handle.cancelled()
                       and getattr(handle._callback, "__module__", "") == script.__name__
                       and getattr(handle._callback, "__name__", "") == "_set_result_unless_done"]
            if len(handles) != 1 or not instance.is_running:
                raise RuntimeError(f"expected one real script timeout, got {len(handles)}")
            handle = handles[0]
            timeout_handle[0] = handle
            timeout_future[0] = handle._args[0]
            due_elapsed[0] = handle.when() - monotone_origin
            elapsed[0] = due_elapsed[0]
            if case["order"] == "event-callback-first":
                elapsed[0] -= 1.0

            def timeout():
                handle._run()
                handle.cancel()

            # Both causes are admitted without an intervening event-loop yield.
            if case["order"] == "event-callback-first":
                hass.bus.async_fire("nha042_release")
            elif case["order"] == "event-first":
                hass.bus.async_fire("nha042_release")
                timeout()
            else:
                timeout()
                hass.bus.async_fire("nha042_release")
            values = await asyncio.wait_for(asyncio.gather(task, return_exceptions=True), 5)
            result = values[0]
            # No error is deliberately injected into a race case. Preserve an
            # unexpected exception and its traceback instead of calling its
            # type an observed HA race outcome.
            if isinstance(result, BaseException):
                raise result
            if result is not None and not isinstance(result, script.ScriptRunResult):
                raise TypeError(f"unexpected race Script result: {type(result).__name__}")
            wait_result = None if result is None else result.variables.get("wait")
            if wait_result and wait_result.get("trigger"):
                trace_trigger = wait_result["trigger"]
                wait_result = {**wait_result, "trigger": {key: trace_trigger[key] for key in ("id", "idx", "platform") if key in trace_trigger}}
            return {"id": case["id"], "effects": list(effects), "wait": wait_result,
                    "result": type(result).__name__, "result_state": "absent" if result is None else "returned",
                    "running": instance.is_running,
                    "timeout_cancelled": handle.cancelled(), "callbacks": callbacks,
                    "elapsed": elapsed[0], "wall_utc": dt_util.utcnow().isoformat(),
                    "cause_order": case["order"], "trigger_registration_completed": trigger_registered[0],
                    "callback_first_case_trigger_elapsed": due_elapsed[0]-1.0 if case["order"] == "event-callback-first" else None,
                    "scope": "cause admission and instrumented real callback execution at coherent virtual deadline; labels alone do not establish callback order"}

        if kind == "parallel":
            sequence = [{"parallel": [
                {"sequence": [switch_action("switch.nha042_fail")]},
                {"sequence": [{"wait_template": "{{ is_state('switch.nha042_gate', 'on') }}"},
                              switch_action("switch.nha042_other_branch")]},
            ]}, switch_action("switch.nha042_after_parallel")]
            instance = await prepare_helper(hass, sequence, case["id"])
            task = hass.async_create_task(instance.async_run(context=Context()))
            await wait_until(lambda: task.done() or any("switch.nha042_gate" in item["template"] for item in template_registrations))
            before = {"effects": list(effects), "running": instance.is_running, "task_done": task.done()}
            hass.states.async_set("switch.nha042_gate", "on")
            values = await asyncio.wait_for(asyncio.gather(task, return_exceptions=True), 5)
            result = values[0]
            error = None
            if isinstance(result, BaseException):
                # Only record() deliberately raises this exact class/marker.
                # HA script._async_step_parallel gathers all branches, then
                # propagates their exception (pinned helpers/script.py:724-731).
                if type(result) is not HomeAssistantError or result.args != ("synthetic branch error",):
                    raise result
                error = {"type": type(result).__name__, "message": str(result)}
            elif result is not None and not isinstance(result, script.ScriptRunResult):
                raise TypeError(f"unexpected parallel Script result: {type(result).__name__}")
            return {"id": case["id"], "before": before, "effects": list(effects),
                    "result": type(result).__name__, "error": error,
                    "result_state": "synthetic_error" if error is not None else "absent" if result is None else "returned",
                    "running": instance.is_running,
                    "template_subscriptions": template_registrations}
        raise ValueError("unknown case kind")
    finally:
        # Cleanup must not hide the scenario's primary exception. Each real
        # entity's public removal joins Script.async_unload through
        # ScriptEntity.async_will_remove_from_hass (component lines 774-781).
        primary = sys.exc_info()[1]
        cleanup_errors = []

        async def clean(label, operation):
            try:
                await operation()
            except BaseException as error:
                cleanup_errors.append(error)
                print("NHA042_CLEANUP " + json.dumps({"case": case["id"], "step": label,
                      "error_type": type(error).__name__}), file=sys.stderr, flush=True)

        if instance is not None:
            await clean("parent_unload", instance.async_unload)
        if component is not None:
            for component_entity in list(component.entities):
                await clean("entity_remove", lambda component_entity=component_entity: component.async_remove_entity(component_entity.entity_id))
        if task is not None:
            if not task.done():
                task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        try:
            patches.close()
        except BaseException as error:
            cleanup_errors.append(error)
        await clean("hass_stop", lambda: hass.async_stop(force=True))
        # Core.async_stop marks this shared loop as closed to thread-safe
        # callbacks. The pinned tests/common.py:368-372 clears that marker so
        # the next independent HomeAssistant fixture can use the same loop.
        with suppress(AttributeError):
            delattr(hass.loop, _SHUTDOWN_RUN_CALLBACK_THREADSAFE)
        if primary is None and cleanup_errors:
            raise BaseExceptionGroup("scenario cleanup failed", cleanup_errors)


async def collect(fixtures):
    if homeassistant.const.__version__ != fixtures["homeassistant"] or version("homeassistant") != fixtures["homeassistant"]:
        raise RuntimeError("wrong HA package version")
    if sys.version_info < (3, 14, 2):
        raise RuntimeError("wrong Python version")
    module_hashes = {name: sha256(Path(inspect.getfile(module)).read_bytes()).hexdigest()
                     for name, module in (("script", script), ("component", script_component), ("variables", script_variables))}
    runtime = {"homeassistant": version("homeassistant"), "python": sys.version.split()[0],
               "async_interrupt": version("async-interrupt"), "source_sha256": module_hashes}
    print("NHA042_RUNTIME " + json.dumps(runtime, sort_keys=True), file=sys.stderr, flush=True)
    for name, digest in module_hashes.items():
        if digest != fixtures[f"source_{name}_sha256"]:
            raise RuntimeError(f"installed {name} module differs from pinned commit: {digest}")
    observations = []
    for case in fixtures["cases"]:
        observation = await observe_case(case)
        # Preserve each completed scenario even if a later scenario fails.
        print("NHA042_CASE " + json.dumps(observation, sort_keys=True, default=str), file=sys.stderr, flush=True)
        observations.append(observation)
    return {"runtime": runtime,
            "source_commit": fixtures["source_commit"], "cases": observations}


if __name__ == "__main__":
    print(json.dumps(asyncio.run(collect(json.load(sys.stdin))), sort_keys=True, default=str))
