"""Supervised ESPHome Native API controls. No startup/reconnect writes or replay."""
import argparse
import asyncio
import base64
import logging
import math
import os
from pathlib import Path
import signal
import sys
import time

import aioesphomeapi as esp
import grpc
from google.protobuf.timestamp_pb2 import Timestamp

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "testworker/generated"))
from drivers.esphome.config import Kind, Operation, load_settings
from home.controls.v1 import controls_pb2 as controls
from home.integrations.v1 import host_pb2, host_pb2_grpc
from home.registry.v1 import registry_pb2
from home.state.v1 import state_pb2
from testworker.schema_digest import SCHEMA_DIGEST_HEX, HOST_MAJOR, HOST_MINOR


class ProtocolFault(Exception):
    """Safe error category, never includes device data or secrets."""


SCHEMAS = {Kind.ALARM: "core.alarm.v1", Kind.COVER: "core.cover.v1", Kind.LOCK: "core.lock.v1"}
INFO_TYPES = {Kind.ALARM: esp.AlarmControlPanelInfo, Kind.COVER: esp.CoverInfo, Kind.LOCK: esp.LockInfo}
STATE_TYPES = {Kind.ALARM: esp.AlarmControlPanelEntityState, Kind.COVER: esp.CoverState, Kind.LOCK: esp.LockEntityState}
ALARM_STATES = {getattr(esp.AlarmControlPanelState, name): getattr(controls, "ALARM_STATE_" + name)
                for name in ("DISARMED", "ARMED_HOME", "ARMED_AWAY", "ARMED_NIGHT", "ARMED_VACATION",
                             "ARMED_CUSTOM_BYPASS", "PENDING", "ARMING", "DISARMING", "TRIGGERED")}
LOCK_STATES = {getattr(esp.LockState, name): getattr(controls, "LOCK_STATE_" + name)
               for name in ("LOCKED", "UNLOCKED", "JAMMED", "LOCKING", "UNLOCKING", "OPENING", "OPEN")}
ALARM_COMMANDS = {Operation.ARM_HOME: (controls.ALARM_OPERATION_ARM_HOME, esp.AlarmControlPanelCommand.ARM_HOME),
                  Operation.ARM_AWAY: (controls.ALARM_OPERATION_ARM_AWAY, esp.AlarmControlPanelCommand.ARM_AWAY),
                  Operation.DISARM: (controls.ALARM_OPERATION_DISARM, esp.AlarmControlPanelCommand.DISARM),
                  Operation.ARM_NIGHT: (controls.ALARM_OPERATION_ARM_NIGHT, esp.AlarmControlPanelCommand.ARM_NIGHT)}
COVER_COMMANDS = {Operation.OPEN: controls.COVER_OPERATION_OPEN, Operation.CLOSE: controls.COVER_OPERATION_CLOSE,
                  Operation.STOP: controls.COVER_OPERATION_STOP}
LOCK_COMMANDS = {Operation.LOCK: (controls.LOCK_OPERATION_LOCK, esp.LockCommand.LOCK),
                 Operation.UNLOCK: (controls.LOCK_OPERATION_UNLOCK, esp.LockCommand.UNLOCK)}


def timestamp():
    value = Timestamp()
    value.GetCurrentTime()
    return value


def description(function, info, code):
    result = registry_pb2.CapabilityDescription(schema_id=SCHEMAS[function.kind], major=1,
        endpoint="native", display_name=info.name or function.object_id)
    if function.kind is Kind.ALARM:
        result.alarm.observed_states.extend(ALARM_STATES.values())
        for name in function.operations:
            if name == Operation.DISARM:
                allowed = not info.requires_code or code is not None
            else:
                feature = {Operation.ARM_HOME: esp.AlarmControlPanelEntityFeature.ARM_HOME,
                           Operation.ARM_AWAY: esp.AlarmControlPanelEntityFeature.ARM_AWAY,
                           Operation.ARM_NIGHT: esp.AlarmControlPanelEntityFeature.ARM_NIGHT}[name]
                allowed = bool(info.supported_features & feature) and (not info.requires_code_to_arm or code is not None)
            if allowed:
                result.alarm.operations.append(ALARM_COMMANDS[name][0])
    elif function.kind is Kind.COVER:
        if info.assumed_state:
            raise ProtocolFault("assumed cover state is unsupported")
        result.cover.observed_states.extend([controls.COVER_STATE_OPEN, controls.COVER_STATE_CLOSED,
                                            controls.COVER_STATE_OPENING, controls.COVER_STATE_CLOSING])
        result.cover.observes_position_percent = info.supports_position
        result.cover.operations.extend(COVER_COMMANDS[name] for name in function.operations
                                       if name != Operation.STOP or info.supports_stop)
    else:
        if info.assumed_state:
            raise ProtocolFault("assumed lock state is unsupported")
        result.lock.observed_states.extend(LOCK_STATES.values())
        # Lock credentials are not supported by this driver profile. Discovery
        # must not offer an action whose authentication cannot be satisfied.
        if not info.requires_code:
            result.lock.operations.extend(LOCK_COMMANDS[name][0] for name in function.operations)
    return result


def state_value(function, info, state, api_version):
    if type(state) is not STATE_TYPES[function.kind] or state.key != function.key or state.device_id != 0:
        raise ProtocolFault("state identity mismatch")
    if function.kind is Kind.ALARM:
        if state.state not in ALARM_STATES:
            return None
        return controls.AlarmValue(state=ALARM_STATES[state.state])
    if function.kind is Kind.LOCK:
        if state.state not in LOCK_STATES:
            return None
        return controls.LockValue(state=LOCK_STATES[state.state])
    if state.current_operation is esp.CoverOperation.IS_OPENING:
        value = controls.COVER_STATE_OPENING
    elif state.current_operation is esp.CoverOperation.IS_CLOSING:
        value = controls.COVER_STATE_CLOSING
    elif state.current_operation is esp.CoverOperation.IDLE:
        value = controls.COVER_STATE_CLOSED if state.is_closed(api_version) else controls.COVER_STATE_OPEN
    else:
        return None
    if not math.isfinite(state.position) or not 0 <= state.position <= 1:
        raise ProtocolFault("invalid cover position")
    result = controls.CoverValue(state=value)
    if info.supports_position:
        result.position_percent = state.position * 100
    return result


class Host:
    def __init__(self, channel, token):
        self.stub = host_pb2_grpc.HostServiceStub(channel)
        self.metadata = (("authorization", "Bearer " + token),)
        self.sequence = 0
        self.handles = {}

    async def call(self, method, message, timeout=5):
        return await getattr(self.stub, method)(message, metadata=self.metadata, timeout=timeout)

    async def handshake(self, secrets):
        capabilities = [host_pb2.CAPABILITY_DISCOVER, host_pb2.CAPABILITY_PUBLISH,
                        host_pb2.CAPABILITY_HEALTH, host_pb2.CAPABILITY_COMMAND]
        if secrets:
            capabilities.append(host_pb2.CAPABILITY_SECRET_READ)
        digest = bytes.fromhex(SCHEMA_DIGEST_HEX)
        response = await self.call("Handshake", host_pb2.HandshakeRequest(
            host_major=HOST_MAJOR, host_minor=HOST_MINOR, driver_major=HOST_MAJOR, driver_minor=HOST_MINOR,
            schema_sha256=digest, max_frame_bytes=65536, capabilities=capabilities))
        if (response.host_major, response.host_minor, response.driver_major, response.driver_minor) != \
                (HOST_MAJOR, HOST_MINOR, HOST_MAJOR, HOST_MINOR) or response.schema_sha256 != digest or \
                set(response.capabilities) != set(capabilities) or not 1024 <= response.max_frame_bytes <= 65536:
            raise ProtocolFault("Host profile mismatch")

    async def secret(self, name):
        if name is None:
            return None
        response = await self.call("ReadSecret", host_pb2.ReadSecretRequest(name=name))
        if not 1 <= len(response.value) <= 256:
            raise ProtocolFault("invalid scoped credential")
        return response.value.decode("utf-8")

    async def publish(self, function, value, observed_at):
        self.sequence += 1
        observation = host_pb2.Observation(binding_handle=self.handles[function.key], endpoint="native",
            observed_at=observed_at, source_seq=self.sequence,
            quality=state_pb2.Quality(availability=state_pb2.AVAILABILITY_AVAILABLE if value is not None
                                     else state_pb2.AVAILABILITY_UNAVAILABLE,
                                     freshness=state_pb2.FRESHNESS_FRESH,
                                     provenance=state_pb2.PROVENANCE_OBSERVED))
        if value is not None:
            getattr(observation, function.kind.value).CopyFrom(value)
        # Native API offers no command correlation. A coincident observation is
        # never promoted to physical confirmation of a particular ledger action.
        await self.call("Publish", host_pb2.PublishRequest(observation=observation))

    async def complete(self, frame, kind):
        await self.call("CompleteCommand", host_pb2.CompleteCommandRequest(
            command_id=frame.request.command_id, kind=kind))


def dispatch(client, function, info, descriptor, code, frame):
    request = frame.request
    if request.capability_schema_id != SCHEMAS[function.kind] or request.endpoint != "native" or \
            request.arguments.WhichOneof("arguments") != function.kind.value or \
            request.expires_at.ToNanoseconds() <= time.time_ns():
        raise ProtocolFault("unsupported or expired command")
    if function.kind is Kind.ALARM:
        operation = request.arguments.alarm.operation
        if operation not in descriptor.alarm.operations:
            raise ProtocolFault("unadvertised alarm operation")
        action = next(pair[1] for pair in ALARM_COMMANDS.values() if pair[0] == operation)
        client.alarm_control_panel_command(function.key, action, code, device_id=0)
    elif function.kind is Kind.COVER:
        arg = request.arguments.cover
        if arg.WhichOneof("command") != "operation" or arg.operation not in descriptor.cover.operations:
            raise ProtocolFault("unadvertised cover operation")
        if arg.operation == controls.COVER_OPERATION_STOP:
            client.cover_command(function.key, stop=True, device_id=0)
        else:
            client.cover_command(function.key, position=0.0 if arg.operation == controls.COVER_OPERATION_CLOSE else 1.0,
                                 device_id=0)
    else:
        operation = request.arguments.lock.operation
        if operation not in descriptor.lock.operations:
            raise ProtocolFault("unadvertised lock operation")
        action = next(pair[1] for pair in LOCK_COMMANDS.values() if pair[0] == operation)
        client.lock_command(function.key, action, device_id=0)


async def heartbeat(host):
    while True:
        await asyncio.sleep(1)
        await host.call("Heartbeat", host_pb2.HeartbeatRequest(), timeout=2)


async def commands(host, client, settings, infos, descriptions, codes):
    by_handle = {host.handles[f.key]: f for f in settings.functions}
    stream = host.stub.Commands(host_pb2.CommandsRequest(), metadata=host.metadata)
    async for frame in stream:
        function = by_handle.get(frame.binding_handle)
        if function is None:
            raise ProtocolFault("foreign command binding")
        try:
            dispatch(client, function, infos[function.key], descriptions[function.key], codes[function.key], frame)
        except ProtocolFault:
            await host.complete(frame, host_pb2.COMMAND_RESULT_KIND_FAILED)
            continue
        # Socket submission is neither device ACK nor physical confirmation.
        # Any exception after dispatch terminates this attempt, without replay.
        await host.complete(frame, host_pb2.COMMAND_RESULT_KIND_ACCEPTED)
    raise ProtocolFault("Host command stream closed")


async def run(settings):
    fd = int(os.environ["HOMEWARD_HOST_TOKEN_FD"])
    try:
        token = os.read(fd, 128).decode("ascii")
    finally:
        os.close(fd)
    if len(token) != 64 or any(c not in "0123456789abcdef" for c in token):
        raise ProtocolFault("invalid Host token")
    loop = asyncio.get_running_loop()
    owner = asyncio.current_task()
    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, owner.cancel)
    channel = grpc.aio.insecure_channel("unix:" + os.environ["HOMEWARD_HOST_SOCKET"],
        options=[("grpc.max_receive_message_length", 1 << 20), ("grpc.max_send_message_length", 1 << 20)])
    host = Host(channel, token)
    client = None
    tasks = []
    try:
        await asyncio.wait_for(channel.channel_ready(), 5)
        await host.handshake(bool(settings.noise_secret or settings.password_secret or
                                  any(f.code_secret for f in settings.functions)))
        tasks.append(asyncio.create_task(heartbeat(host)))
        noise = await host.secret(settings.noise_secret)
        password = await host.secret(settings.password_secret)
        if noise is not None and len(base64.b64decode(noise, validate=True)) != 32:
            raise ProtocolFault("invalid Noise key")
        codes = {f.key: await host.secret(f.code_secret) for f in settings.functions}
        if any(code is not None and (not code.isascii() or not code.isdigit() or len(code) > 32)
               for code in codes.values()):
            raise ProtocolFault("invalid alarm credential")
        stopped = asyncio.Event()

        async def on_stop(_expected):
            stopped.set()

        client = esp.APIClient(settings.host, settings.port, password, noise_psk=noise,
                               expected_mac=settings.expected_mac.replace(":", ""), client_info="Homeward",
                               provide_time=False)
        await asyncio.wait_for(client.connect(on_stop=on_stop, login=True, log_errors=False), 15)
        device = await asyncio.wait_for(client.device_info(), 10)
        if device.mac_address.lower() != settings.expected_mac or client.api_version < esp.APIVersion(1, 8):
            raise ProtocolFault("device identity or API mismatch")
        inventory, _services = await asyncio.wait_for(client.list_entities_services(), 10)
        infos = {}
        descriptions = {}
        for function in settings.functions:
            matches = [info for info in inventory if type(info) is INFO_TYPES[function.kind]
                       and info.key == function.key and info.device_id == 0 and info.object_id == function.object_id]
            if len(matches) != 1:
                raise ProtocolFault("configured entity not uniquely discovered")
            infos[function.key] = matches[0]
            descriptions[function.key] = description(function, matches[0], codes[function.key])
        response = await host.call("Discover", host_pb2.DiscoverRequest(candidates=[host_pb2.Candidate(
            source=host_pb2.SourceKey(device=settings.source_device, entity=f.source_entity),
            descriptions=[descriptions[f.key]]) for f in settings.functions]))
        if len(response.bindings) != len(settings.functions) or any(len(b.handle) != 32 for b in response.bindings):
            raise ProtocolFault("Host discovery mismatch")
        host.handles = {f.key: b.handle for f, b in zip(settings.functions, response.bindings)}
        queue = asyncio.Queue(maxsize=128)
        by_key = {f.key: f for f in settings.functions}

        def on_state(state):
            if state.key not in by_key or state.device_id != 0:
                return
            try:
                queue.put_nowait((state, timestamp()))
            except asyncio.QueueFull:
                stopped.set()

        async def observations():
            while True:
                state, observed_at = await queue.get()
                f = by_key[state.key]
                await host.publish(f, state_value(f, infos[f.key], state, client.api_version), observed_at)

        client.subscribe_states(on_state)
        tasks.extend([asyncio.create_task(observations()), asyncio.create_task(stopped.wait()),
                      asyncio.create_task(commands(host, client, settings, infos, descriptions, codes))])
        await host.call("ReportHealth", host_pb2.ReportHealthRequest(status=host_pb2.HEALTH_STATUS_READY,
                                                                    detail="ESPHome Native API session"))
        done, _pending = await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
        for task in done:
            task.result()
        raise ProtocolFault("Native API session stopped")
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        if client is not None:
            try:
                await asyncio.wait_for(client.disconnect(), 3)
            except Exception:
                pass
        await channel.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", type=Path, required=True)
    args = parser.parse_args()
    # Upstream exception logs may contain addresses or authentication material.
    logging.disable(logging.CRITICAL)
    try:
        asyncio.run(run(load_settings(args.config)))
    except KeyboardInterrupt:
        return
    except BaseException:
        print("ESPHome worker stopped; session requires supervision", file=sys.stderr)
        raise SystemExit(1) from None


if __name__ == "__main__":
    main()
