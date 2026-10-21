"""Bounded047 producer. Import has no HA, socket, process or filesystem effect."""

from dataclasses import dataclass
from hashlib import sha256
import json


def load_json(text):
    def unique(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError("duplicate JSON member")
            result[key] = value
        return result
    return json.loads(text, object_pairs_hook=unique,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError(value)))


@dataclass(frozen=True)
class ConsumedConfiguration:
    document: object
    input_sha256: str


def consume_yaml(buffer, loader):
    """Hash the actual buffer at the one loader call, not a case/runtime lookup."""
    if type(buffer) is not bytes or not 0 < len(buffer) <= 1 << 20:
        raise ValueError("loaded source outside bound")
    digest = sha256(buffer).hexdigest()
    document = loader(buffer)
    if not isinstance(document, dict):
        raise ValueError("automation document must be a mapping")
    return ConsumedConfiguration(document, digest)


def patch_qualification_clock(patches, loop, dt_util, script, event_helper, origin, elapsed):
    """Keep HA's datetime, wall-clock and timer clock on one virtual timeline."""
    from datetime import timedelta, timezone
    from unittest.mock import patch

    def utcnow():
        return (origin + timedelta(seconds=elapsed())).astimezone(timezone.utc)

    def timestamp():
        # datetime rounds to whole microseconds; HA's wall timer must retain
        # the same fractional deadline as loop.time and its early-fire guard.
        return origin.timestamp() + elapsed()

    # _TrackPointUTCTime.async_attach converts a wall timestamp with time.time;
    # its early-fire guard uses the separately bound time_tracker_timestamp.
    # Both must move with UTC and loop.time. The virtual monotonic origin is
    # zero, so that conversion retains the registered deadline's precision.
    patches.enter_context(patch.object(loop, "time", elapsed))
    patches.enter_context(patch.object(event_helper.time, "time", timestamp))
    patches.enter_context(patch.object(event_helper, "time_tracker_timestamp", timestamp))
    patches.enter_context(patch.object(dt_util, "utcnow", utcnow))
    patches.enter_context(patch.object(dt_util, "now", lambda time_zone=None:
                                      utcnow().astimezone(time_zone or dt_util.get_default_time_zone())))
    patches.enter_context(patch.object(script, "utcnow", utcnow))
    patches.enter_context(patch.object(event_helper, "time_tracker_utcnow", utcnow))


def trigger_deadline(points, requested_timestamp):
    # HA's time-pattern tracker retains its native 50..500ms jitter. Observe
    # that registered deadline instead of changing its random choice or firing
    # the trigger ourselves. Sun timers already have their exact Astral point.
    matches = [point for point in points
               if requested_timestamp <= point < requested_timestamp + 1]
    if len(matches) != 1:
        raise ValueError("native trigger deadline missing or ambiguous")
    return matches[0]


async def run_worker(envelope):
    import asyncio
    from contextlib import ExitStack, suppress
    from datetime import datetime, timedelta
    import importlib
    from importlib.metadata import version
    import inspect
    from pathlib import Path
    import socket
    import sys
    import time
    from unittest.mock import patch

    from homeassistant import config_entries, loader
    from homeassistant.core import CoreState, HomeAssistant, callback
    from homeassistant.helpers import (
        area_registry, category_registry, condition, device_registry,
        entity as entity_helper, entity_registry, floor_registry, frame,
        issue_registry, label_registry, restore_state, translation,
        trigger as trigger_helper,
    )
    from homeassistant.helpers import event as event_helper
    from homeassistant.helpers import script
    from homeassistant.components import automation as automation_component
    from homeassistant.setup import async_setup_component
    from homeassistant.util import dt as dt_util
    from homeassistant.util import yaml as ha_yaml
    from homeassistant.util.async_ import _SHUTDOWN_RUN_CALLBACK_THREADSAFE

    if set(envelope) != {"contract", "fixture", "fixture_sha256"}:
        raise ValueError("unknown producer envelope")
    contract, text = envelope["contract"], envelope["fixture"]
    fixture = load_json(text)
    digest = sha256(text.encode()).hexdigest()
    if (digest != envelope["fixture_sha256"] or fixture["schema"] != contract["schema"]
            or not any(row["id"] == fixture["id"] and row["sha256"] == digest
                       for row in contract["cases"])):
        raise ValueError("unregistered case provenance")
    if fixture["scenario"] not in {"legacy_action", "time_climate", "sun_switch", "scene_projection"}:
        raise ValueError("qualification scenario not yet implemented")
    if (version("homeassistant") != contract["ha_version"]
            or sys.version.split()[0] != contract["python"]
            or version("jinja2") != contract["jinja"]):
        raise ValueError("pinned runtime drift")
    sources = []
    for pin in contract["sources"]:
        module = importlib.import_module(pin["path"])
        actual = sha256(Path(inspect.getfile(module)).read_bytes()).hexdigest()
        if actual != pin["sha256"]:
            raise ValueError("loaded module drift")
        sources.append({"path": pin["path"], "sha256": actual})
    if not any(row["path"] == "homeassistant.core" for row in sources):
        raise ValueError("core provenance absent")
    if [name for _, name in socket.if_nameindex()] != ["lo"]:
        raise ValueError("loopback-only namespace required")
    for path, column in (("/proc/net/route", 0), ("/proc/net/ipv6_route", -1)):
        routes = Path(path).read_text()
        if len(routes) > 65536:
            raise ValueError("route evidence too large")
        for line in routes.splitlines():
            fields = line.split()
            if fields and fields[0] != "Iface" and fields[column] != "lo":
                raise ValueError("non-loopback route")

    # The parser sees this exact buffer once. No expected/demand fields feed
    # execution, observation or provenance.
    buffer = fixture["yaml"].encode("utf-8")
    consumed = consume_yaml(buffer, lambda value: ha_yaml.parse_yaml(value.decode("utf-8")))
    tokens = {row["name"]: row["token"] for row in contract["demands"]}
    origin = datetime.fromisoformat("2024-03-11T00:00:00-04:00")
    elapsed = 0.0
    hass = HomeAssistant("/tmp/nha047/" + fixture["id"])
    patches = ExitStack()
    handles, components, demands, errors = [], [], [], []
    tracked = None
    terminal = 0
    trigger_points = []
    original_call_at = hass.loop.call_at

    async def turn():
        future = hass.loop.create_future()
        hass.loop.call_soon(future.set_result, None)
        await future

    async def until(predicate):
        deadline = time.monotonic() + 5
        while not predicate():
            if time.monotonic() >= deadline:
                raise TimeoutError("native HA phase barrier absent")
            await turn()

    def call_at(when, function, *args, context=None):
        if len(handles) >= 1024:
            raise ValueError("timer capture full")
        handle = original_call_at(when, function, *args, context=context)
        handles.append(handle)
        if isinstance(function, event_helper._TrackPointUTCTime):
            trigger_points.append(function.expected_fire_timestamp)
        return handle

    original_trigger = automation_component.AutomationEntity.async_trigger

    async def observed_trigger(entity, *args, **kwargs):
        nonlocal terminal
        if entity is not tracked:
            return await original_trigger(entity, *args, **kwargs)
        try:
            return await original_trigger(entity, *args, **kwargs)
        except BaseException as error:
            errors.append(error)
            raise
        finally:
            terminal += 1

    @callback
    def observe_service(call):
        if len(demands) >= 1024:
            raise ValueError("demand capture full")
        targets = call.data.get("entity_id", [])
        if isinstance(targets, str):
            targets = [targets]
        if len(targets) != 1 or targets[0] not in {
                "light.nha047_value", "switch.nha047_value",
                "fan.nha047_value", "climate.nha047_value"}:
            raise ValueError("non-synthetic or ambiguous command")
        value, secondary, code = 0, 0, ""
        if call.domain == "light" and call.service == "turn_on":
            kind, value = tokens["LightOn"], call.data["brightness"]
            if "color_temp_kelvin" in call.data:
                secondary = int(1000000 / call.data["color_temp_kelvin"])
        elif call.domain == "light" and call.service == "turn_off":
            kind = tokens["LightOff"]
        elif call.domain == "switch" and call.service in {"turn_on", "turn_off"}:
            kind = tokens["SwitchOn" if call.service == "turn_on" else "SwitchOff"]
        elif call.domain == "fan" and call.service in {"turn_on", "turn_off"}:
            kind = tokens["FanOn" if call.service == "turn_on" else "FanOff"]
            if call.service == "turn_on":
                value = call.data["percentage"]
        elif call.domain == "climate" and call.service == "set_temperature":
            kind, value = tokens["ClimateTemperature"], call.data["temperature"]
        elif call.domain == "climate" and call.service == "set_hvac_mode":
            kind, code = tokens["ClimateMode"], call.data["hvac_mode"]
        elif call.domain == "climate" and call.service == "set_humidity":
            kind, value = tokens["ClimateHumidity"], call.data["humidity"]
        elif call.domain == "climate" and call.service == "set_fan_mode":
            kind, code = tokens["ClimateFanMode"], call.data["fan_mode"]
        elif call.domain == "climate" and call.service == "set_preset_mode":
            kind, code = tokens["ClimatePresetMode"], call.data["preset_mode"]
        else:
            raise ValueError("service outside qualification profile")
        demands.append({"kind": kind, "target": targets[0], "value": value,
                        "secondary": secondary, "code": code})

    cleanup = False
    try:
        patches.enter_context(patch.object(hass.loop, "call_at", call_at))
        patch_qualification_clock(patches, hass.loop, dt_util, script, event_helper,
                                  origin, lambda: elapsed)
        patches.enter_context(patch.object(automation_component.AutomationEntity, "async_trigger", observed_trigger))
        Path(hass.config.config_dir).mkdir(parents=True, exist_ok=True)
        hass.config.skip_pip = True
        hass.config.skip_pip_packages = []
        hass.config_entries = config_entries.ConfigEntries(hass, {"_": "nha047"})
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
        for domain, services in {
                "light": ("turn_on", "turn_off"),
                "switch": ("turn_on", "turn_off"),
                "fan": ("turn_on", "turn_off"),
                "climate": ("set_temperature", "set_hvac_mode", "set_humidity",
                            "set_fan_mode", "set_preset_mode")}.items():
            for service in services:
                hass.services.async_register(domain, service, observe_service)
        if fixture["scenario"] == "scene_projection":
            from homeassistant.components.homeassistant.scene import (
                HomeAssistantScene, PLATFORM_SCHEMA, SceneConfig,
            )
            parsed = PLATFORM_SCHEMA({"platform": "homeassistant", "states": [consumed.document]})["states"][0]
            for entity_id, state in parsed["entities"].items():
                # Force reproduction rather than an already-satisfied shortcut.
                initial = "off" if state.state != "off" else "on"
                hass.states.async_set(entity_id, initial)
            native_scene = HomeAssistantScene(hass, SceneConfig(
                parsed.get("id"), parsed["name"], parsed.get("icon"), parsed["entities"]))
            await native_scene.async_activate()
            await hass.async_block_till_done()
            terminal = 1
        else:
            for entity, on in (("switch.nha047_season", fixture["season"]),
                               ("switch.nha047_workday", fixture["workday"]),
                               ("switch.nha047_value", False)):
                hass.states.async_set(entity, "on" if on else "off")
            hass.config.latitude = 0
            hass.config.longitude = 0
            hass.config.elevation = 0
            if not await async_setup_component(hass, "automation", {"automation": [consumed.document]}):
                raise RuntimeError("real automation setup failed")
            component = hass.data["automation"]
            components.append(component)
            entities = list(component.entities)
            if len(entities) != 1:
                raise ValueError("automation cardinality")
            tracked = entities[0]
            await hass.async_block_till_done()
            if not handles or terminal or demands:
                raise ValueError("trigger missing or executed before its deadline")
            if fixture["scenario"] == "sun_switch":
                from homeassistant.helpers.sun import get_astral_event_next
                target = get_astral_event_next(hass, "sunset", origin, timedelta(hours=-1))
            else:
                hours, minutes, seconds = (int(part) for part in fixture["at"].split(":"))
                target = origin + timedelta(hours=hours, minutes=minutes, seconds=seconds)
            deadline = trigger_deadline(trigger_points, target.timestamp()) - origin.timestamp()
            elapsed = deadline - 0.001
            await hass.async_block_till_done()
            if terminal or demands:
                raise ValueError("native trigger executed before its registered deadline")
            elapsed = deadline
            await until(lambda: terminal != 0)
            if errors or terminal != 1:
                raise RuntimeError("native automation did not finish exactly once")

    finally:
        for component in reversed(components):
            for entity in list(component.entities):
                await component.async_remove_entity(entity.entity_id)
        for handle in handles:
            handle.cancel()
        patches.close()
        await hass.async_stop(force=True)
        with suppress(AttributeError):
            delattr(hass.loop, _SHUTDOWN_RUN_CALLBACK_THREADSAFE)
        cleanup = (all(not list(component.entities) for component in components)
                   and all(handle.cancelled() for handle in handles)
                   and hass.state is CoreState.stopped)
        if not cleanup:
            raise RuntimeError("HA cleanup not terminal")
    return {"schema": contract["schema"], "scenario": fixture["id"],
            "fixture_sha256": digest, "input_sha256": consumed.input_sha256,
            "runtime": "ha", "version": contract["ha_version"], "image": contract["image"],
            "sources": sources, "terminal": cleanup and terminal == 1,
            "evicted": 0, "loopback_only": True, "demands": demands}


if __name__ == "__main__":
    import asyncio
    import sys
    text = sys.stdin.read((5 << 20) + 1)
    if len(text.encode()) > 5 << 20:
        raise ValueError("worker input too large")
    output = json.dumps(asyncio.run(run_worker(load_json(text))), sort_keys=True, allow_nan=False)
    if len(output.encode()) > 8 << 20:
        raise ValueError("worker capture too large")
    print(output)
