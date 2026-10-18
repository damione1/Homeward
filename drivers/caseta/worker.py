"""Supervised, device-free-by-default Caséta LEAP Host 1.1 worker."""

import argparse
import asyncio
import dataclasses
import enum
import logging
import math
import os
import signal
import sys
import tempfile
from pathlib import Path

import grpc
from google.protobuf.timestamp_pb2 import Timestamp

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "testworker/generated"))

from drivers.caseta.bridge import BridgeFault, StrictSmartbridge, open_pinned_tls
from drivers.caseta.config import FunctionKind, load_settings
from home.integrations.v1 import host_pb2, host_pb2_grpc
from home.registry.v1 import registry_pb2
from home.state.v1 import state_pb2
from testworker.schema_digest import SCHEMA_DIGEST_HEX


class FanSpeed(str, enum.Enum):
    OFF = "Off"
    LOW = "Low"
    MEDIUM = "Medium"
    MEDIUM_HIGH = "MediumHigh"
    HIGH = "High"


class PicoEvent(str, enum.Enum):
    PRESS = "Press"
    RELEASE = "Release"


class NoticeKind(enum.Enum):
    REFRESH = enum.auto()
    PICO = enum.auto()


@dataclasses.dataclass(frozen=True)
class LightLevel:
    value: float

    def __post_init__(self):
        if type(self.value) not in (int, float) or not math.isfinite(self.value) or \
                not 0 <= self.value <= 100:
            raise ValueError("invalid light level")


@dataclasses.dataclass(frozen=True)
class SwitchState:
    on: bool

    def __post_init__(self):
        if type(self.on) is not bool:
            raise ValueError("invalid switch state")


@dataclasses.dataclass(frozen=True)
class FanState:
    speed: FanSpeed

    def __post_init__(self):
        if type(self.speed) is not FanSpeed:
            raise ValueError("invalid fan speed")


@dataclasses.dataclass(frozen=True)
class Notice:
    kind: NoticeKind
    alias: str
    event: PicoEvent | None = None

    def __post_init__(self):
        if type(self.kind) is not NoticeKind or \
                (self.kind is NoticeKind.PICO) != (type(self.event) is PicoEvent):
            raise ValueError("invalid LEAP notice")


class UnsupportedCommand(Exception):
    pass


SPEEDS = {
    FanSpeed.OFF: state_pb2.FAN_SPEED_OFF,
    FanSpeed.LOW: state_pb2.FAN_SPEED_LOW,
    FanSpeed.MEDIUM: state_pb2.FAN_SPEED_MEDIUM,
    FanSpeed.MEDIUM_HIGH: state_pb2.FAN_SPEED_MEDIUM_HIGH,
    FanSpeed.HIGH: state_pb2.FAN_SPEED_HIGH,
}
SCHEMAS = {FunctionKind.LIGHT: "core.light.v1",
           FunctionKind.SWITCH: "core.switch.v1",
           FunctionKind.SMART_AWAY: "core.switch.v1",
           FunctionKind.FAN: "core.fan.v1"}
ENDPOINTS = {FunctionKind.LIGHT: "level", FunctionKind.SWITCH: "relay",
             FunctionKind.SMART_AWAY: "away", FunctionKind.FAN: "speed"}
QUALITY = state_pb2.Quality(
    availability=state_pb2.AVAILABILITY_AVAILABLE,
    freshness=state_pb2.FRESHNESS_FRESH,
    provenance=state_pb2.PROVENANCE_OBSERVED)


def timestamp():
    value = Timestamp()
    value.GetCurrentTime()
    return value


def description(function):
    base = dict(schema_id=SCHEMAS[function.kind], major=1,
                endpoint=ENDPOINTS[function.kind], display_name=function.alias)
    if function.kind is FunctionKind.LIGHT:
        return registry_pb2.CapabilityDescription(**base,
            light=registry_pb2.LightConstraints(
                set_on=True,
                brightness_percent=registry_pb2.NumberBounds(minimum=0, maximum=100)))
    if function.kind is FunctionKind.FAN:
        return registry_pb2.CapabilityDescription(**base,
            fan=registry_pb2.FanConstraints(set_on=False,
                speed_options=[speed.value for speed in SPEEDS]))
    return registry_pb2.CapabilityDescription(**base,
        switch=registry_pb2.SwitchConstraints(set_on=True))


class Host:
    def __init__(self, channel, token):
        self.stub = host_pb2_grpc.HostServiceStub(channel)
        self.metadata = (("authorization", "Bearer " + token),)
        self.handles = {}
        self.sequence = 0

    async def call(self, method, message, timeout=5):
        return await getattr(self.stub, method)(message, metadata=self.metadata,
                                               timeout=timeout)

    async def handshake(self):
        capabilities = [host_pb2.CAPABILITY_DISCOVER, host_pb2.CAPABILITY_PUBLISH,
                        host_pb2.CAPABILITY_EMIT, host_pb2.CAPABILITY_HEALTH,
                        host_pb2.CAPABILITY_COMMAND, host_pb2.CAPABILITY_SECRET_READ]
        response = await self.call("Handshake", host_pb2.HandshakeRequest(
            host_major=1, host_minor=1, driver_major=1, driver_minor=1,
            schema_sha256=bytes.fromhex(SCHEMA_DIGEST_HEX),
            max_frame_bytes=65536, capabilities=capabilities))
        if (response.host_major, response.host_minor,
            response.driver_major, response.driver_minor) != (1, 1, 1, 1) or \
                response.schema_sha256 != bytes.fromhex(SCHEMA_DIGEST_HEX) or \
                set(response.capabilities) != set(capabilities) or \
                not 1024 <= response.max_frame_bytes <= 65536:
            raise BridgeFault("Host profile mismatch")

    async def discover(self, functions):
        candidates = [host_pb2.Candidate(
            source=host_pb2.SourceKey(device=f.source_device,
                                      entity=f.source_entity),
            descriptions=[description(f)]) for f in functions]
        response = await self.call("Discover",
                                   host_pb2.DiscoverRequest(candidates=candidates))
        if len(response.bindings) != len(functions) or any(
                len(binding.handle) != 32 for binding in response.bindings):
            raise BridgeFault("Host discovery mismatch")
        self.handles = {f.alias: b.handle for f, b in zip(functions, response.bindings)}

    async def health(self, status):
        await self.call("ReportHealth", host_pb2.ReportHealthRequest(
            status=status, detail="caseta LEAP session"), timeout=2)

    async def publish(self, function, value, correlation=None):
        self.sequence += 1
        observation = host_pb2.Observation(
            binding_handle=self.handles[function.alias],
            endpoint=ENDPOINTS[function.kind], quality=QUALITY,
            observed_at=timestamp(), source_seq=self.sequence)
        if function.kind is FunctionKind.LIGHT and type(value) is LightLevel:
            observation.light.on = value.value > 0
            observation.light.brightness_percent = value.value
        elif function.kind is FunctionKind.FAN and type(value) is FanState:
            observation.fan.speed = SPEEDS[value.speed]
        elif function.kind in (FunctionKind.SWITCH, FunctionKind.SMART_AWAY) and type(value) is SwitchState:
            observation.switch_on = value.on
        else:
            raise BridgeFault("observation type does not match capability")
        if correlation is not None:
            observation.command_correlation = correlation
        await self.call("Publish", host_pb2.PublishRequest(observation=observation))

    async def emit(self, alias, event: PicoEvent):
        if type(event) is not PicoEvent:
            raise BridgeFault("unknown Pico event")
        self.sequence += 1
        await self.call("Emit", host_pb2.EmitRequest(event=host_pb2.Event(
            binding_handle=self.handles[alias], code="pico." + event.value.lower(),
            observed_at=timestamp(), source_seq=self.sequence)))


def validate_inventory(bridge, settings):
    bridges = [d for d in bridge.devices.values() if d.get("model") == "L-BDG2-WH"]
    if len(bridges) != 1:
        raise BridgeFault("exactly one L-BDG2-WH is required")
    lights = {d["device_id"] for d in bridge.get_devices_by_domain("light")}
    fans = {d["device_id"] for d in bridge.get_devices_by_domain("fan")}
    switches = {d["device_id"] for d in bridge.get_devices_by_domain("switch")}
    for f in settings.functions:
        if f.kind is FunctionKind.SMART_AWAY:
            continue
        device = bridge.devices.get(f.device_id)
        if device is None or device.get("zone") != f.zone_id:
            raise BridgeFault("mapped LEAP device/zone not discovered")
        if f.kind is FunctionKind.LIGHT and f.device_id not in lights or \
                f.kind is FunctionKind.FAN and f.device_id not in fans or \
                f.kind is FunctionKind.SWITCH and f.device_id not in lights | switches:
            raise BridgeFault("mapped LEAP device type mismatch")
    if any(p.button_id not in bridge.buttons for p in settings.picos):
        raise BridgeFault("mapped Pico button not discovered")


async def read_value(bridge, function):
    if function.kind is FunctionKind.SMART_AWAY:
        status = await bridge.get_smart_away_status()
        if status not in ("Enabled", "Disabled"):
            raise BridgeFault("Smart Away observation missing")
        return SwitchState(status == "Enabled")
    response = await bridge._request("ReadRequest",
                                     f"/zone/{function.zone_id}/status")
    body = response.Body
    if not isinstance(body, dict) or not isinstance(body.get("ZoneStatus"), dict) or \
            body["ZoneStatus"].get("Zone", {}).get("href") != f"/zone/{function.zone_id}":
        raise BridgeFault("zone observation mismatch")
    status = body["ZoneStatus"]
    if function.kind is FunctionKind.FAN:
        speed = status.get("FanSpeed")
        try:
            admitted = FanSpeed(speed)
        except (ValueError, TypeError):
            raise BridgeFault("fan speed missing or outside canonical set")
        return FanState(admitted)
    level = status.get("Level")
    if type(level) not in (int, float) or not math.isfinite(level) or not 0 <= level <= 100:
        raise BridgeFault("light/switch level missing or invalid")
    return LightLevel(float(level)) if function.kind is FunctionKind.LIGHT else SwitchState(level > 0)


def requested_effect(frame, function):
    request = frame.request
    if request.capability_schema_id != SCHEMAS[function.kind] or \
            request.endpoint != ENDPOINTS[function.kind]:
        raise UnsupportedCommand("unmatched command schema or endpoint")
    which = request.arguments.WhichOneof("arguments")
    if function.kind is FunctionKind.LIGHT:
        if which != "set_light" or request.arguments.set_light.HasField("color_temp_mireds"):
            raise UnsupportedCommand("unsupported dimmer command")
        arg = request.arguments.set_light
        if arg.HasField("brightness_percent"):
            level = arg.brightness_percent
            if not math.isfinite(level) or not 0 <= level <= 100 or \
                    arg.HasField("on") and arg.on != (level > 0):
                raise UnsupportedCommand("inconsistent light command")
            return LightLevel(float(level))
        if arg.HasField("on"):
            return LightLevel(100.0 if arg.on else 0.0)
    elif function.kind is FunctionKind.FAN:
        arg = request.arguments.set_fan
        if which == "set_fan" and not arg.HasField("on") and \
                arg.HasField("speed"):
            try:
                return FanState(FanSpeed(arg.speed))
            except ValueError:
                pass
    else:
        if which == "set_switch":
            return SwitchState(request.arguments.set_switch.on)
    raise UnsupportedCommand("unsupported command arguments")


async def apply(bridge, function, effect):
    if function.kind is FunctionKind.FAN and type(effect) is FanState:
        await bridge.set_fan(function.device_id, effect.speed.value)
    elif function.kind is FunctionKind.SMART_AWAY and type(effect) is SwitchState:
        if effect.on:
            await bridge.activate_smart_away()
        else:
            await bridge.deactivate_smart_away()
    elif function.kind is FunctionKind.LIGHT and type(effect) is LightLevel:
        await bridge.set_value(function.device_id, effect.value)
    elif function.kind is FunctionKind.SWITCH and type(effect) is SwitchState:
        await bridge.set_value(function.device_id, 100 if effect.on else 0)
    else:
        raise UnsupportedCommand("effect type does not match capability")


async def heartbeat(host):
    while True:
        await asyncio.sleep(1)
        await host.call("Heartbeat", host_pb2.HeartbeatRequest(), timeout=2)


async def commands(host, bridge_box, settings, ready):
    stream = host.stub.Commands(host_pb2.CommandsRequest(), metadata=host.metadata)
    async for frame in stream:
        if not ready.is_set():
            await host.call("CompleteCommand", host_pb2.CompleteCommandRequest(
                command_id=frame.request.command_id,
                kind=host_pb2.COMMAND_RESULT_KIND_FAILED))
            continue
        by_handle = {host.handles[f.alias]: f for f in settings.functions
                     if f.alias in host.handles}
        function = by_handle.get(frame.binding_handle)
        if function is None:
            raise BridgeFault("foreign Host command binding")
        bridge = bridge_box.get("bridge")
        if bridge is None:
            raise BridgeFault("LEAP session is not ready")
        try:
            effect = requested_effect(frame, function)
        except UnsupportedCommand:
            await host.call("CompleteCommand", host_pb2.CompleteCommandRequest(
                command_id=frame.request.command_id,
                kind=host_pb2.COMMAND_RESULT_KIND_FAILED))
            continue
        if frame.request.expires_at.ToDatetime().timestamp() <= timestamp().ToDatetime().timestamp():
            raise BridgeFault("expired Host command")
        await apply(bridge, function, effect)
        await host.call("CompleteCommand", host_pb2.CompleteCommandRequest(
            command_id=frame.request.command_id,
            kind=host_pb2.COMMAND_RESULT_KIND_ACKNOWLEDGED))
        # Only this explicit post-ACK read carries the Host correlation. A
        # coincident LEAP notification is published separately without it.
        observed = await read_value(bridge, function)
        await host.publish(function, observed, frame.correlation)
    raise BridgeFault("Host command stream closed")


async def _write_secret(path, contents):
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    fd = os.open(path, flags, 0o600)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(contents)
    except BaseException:
        os.unlink(path)
        raise


async def run(settings):
    socket = os.environ["HOMEWARD_HOST_SOCKET"]
    fd = int(os.environ["HOMEWARD_HOST_TOKEN_FD"])
    try:
        token = os.read(fd, 128).decode("ascii")
    finally:
        os.close(fd)
    if len(token) != 64 or any(c not in "0123456789abcdef" for c in token):
        raise BridgeFault("invalid private Host token")
    loop = asyncio.get_running_loop()
    main_task = asyncio.current_task()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, main_task.cancel)
    channel = grpc.aio.insecure_channel("unix:" + socket,
        options=[("grpc.max_receive_message_length", 1 << 20),
                 ("grpc.max_send_message_length", 1 << 20)])
    bridge = None
    bridge_box = {}
    tasks = []
    ready = asyncio.Event()
    host = Host(channel, token)
    try:
        await asyncio.wait_for(channel.channel_ready(), 5)
        await host.handshake()
        tasks.append(asyncio.create_task(heartbeat(host)))
        tasks.append(asyncio.create_task(commands(host, bridge_box, settings, ready)))
        with tempfile.TemporaryDirectory(prefix="hw-caseta-", dir="/tmp") as folder:
            paths = [Path(folder) / name for name in ("ca.pem", "client.pem", "client.key")]
            for path, name in zip(paths, (settings.ca_secret, settings.cert_secret,
                                          settings.key_secret)):
                response = await host.call("ReadSecret", host_pb2.ReadSecretRequest(name=name))
                if not response.value or len(response.value) > 65536:
                    raise BridgeFault("invalid scoped TLS material")
                await _write_secret(path, response.value)

            async def connector():
                return await open_pinned_tls(settings.host, settings.port,
                                             *paths, settings.pin)

            bridge = StrictSmartbridge(connector)
            bridge_box["bridge"] = bridge
            await bridge.connect_once()
            validate_inventory(bridge, settings)
            await bridge.subscribe_required([p.button_id for p in settings.picos])
            await host.discover(settings.functions)
            by_alias = {f.alias: f for f in settings.functions}
            events = asyncio.Queue(maxsize=64)

            def enqueue(item):
                try:
                    events.put_nowait(item)
                except asyncio.QueueFull:
                    bridge._leap.close()

            for function in settings.functions:
                if function.device_id is not None:
                    bridge.add_subscriber(function.device_id,
                        lambda alias=function.alias: enqueue(Notice(NoticeKind.REFRESH, alias)))
            bridge.add_smart_away_subscriber(
                lambda _state: enqueue(Notice(
                    NoticeKind.REFRESH, "switch.unassigned_smart_away")))
            for pico in settings.picos:
                bridge.add_button_subscriber(pico.button_id,
                    lambda event, p=pico: enqueue(Notice(
                        NoticeKind.PICO, p.binding_alias, PicoEvent(event)))
                    if event in PicoEvent._value2member_map_ else None)
            # All 13 sources are read after subscription, before command gate.
            for function in settings.functions:
                await host.publish(function, await read_value(bridge, function))

            async def notifications():
                while True:
                    event = await events.get()
                    if event.kind is NoticeKind.REFRESH:
                        function = by_alias[event.alias]
                        await host.publish(function, await read_value(bridge, function))
                    else:
                        await host.emit(event.alias, event.event)
                    events.task_done()

            tasks.append(asyncio.create_task(notifications()))
            await asyncio.wait_for(events.join(), 5)
            ready.set()
            await host.health(host_pb2.HEALTH_STATUS_READY)
            async def ping():
                while True:
                    await asyncio.sleep(20)
                    await bridge._request("ReadRequest", "/server/1/status/ping")

            tasks.append(asyncio.create_task(ping()))

            async def reconcile():
                # LEAP does not advertise a lease TTL. A healthy session keeps
                # its subscriptions; explicit 4xx tagged expiry is fatal.
                # Fresh bounded reads prevent old snapshots from persisting.
                while True:
                    await asyncio.sleep(settings.reconcile_seconds)
                    for function in settings.functions:
                        await host.publish(function, await read_value(bridge, function))

            tasks.append(asyncio.create_task(reconcile()))
            done, _ = await asyncio.wait(
                [*tasks, bridge.reader_task],
                return_when=asyncio.FIRST_COMPLETED)
            for task in done:
                await task
            raise BridgeFault("worker task ended unexpectedly")
    finally:
        ready.clear()
        try:
            await host.health(host_pb2.HEALTH_STATUS_UNAVAILABLE)
        except (grpc.RpcError, asyncio.TimeoutError):
            pass
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if bridge is not None:
            await bridge.close_joined()
        await channel.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True, type=Path)
    args = parser.parse_args()
    logging.disable(logging.CRITICAL)
    try:
        asyncio.run(run(load_settings(args.config)))
    except asyncio.CancelledError:
        return 0
    except (Exception, KeyboardInterrupt) as error:
        print("caseta worker unavailable: " + type(error).__name__, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
