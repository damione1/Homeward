"""Synthetic, device-free Host v1 worker using generated Python gRPC stubs."""
import argparse
import json
import os
import pathlib
import signal
import socket
import subprocess
import sys
import threading
import time
from google.protobuf.timestamp_pb2 import Timestamp

sys.path.insert(0, str(pathlib.Path(__file__).parent / "generated"))

import grpc
from home.integrations.v1 import host_pb2, host_pb2_grpc
from home.registry.v1 import registry_pb2
from home.state.v1 import state_pb2
from schema_digest import LEGACY_SCHEMA_DIGEST_HEX, SCHEMA_DIGEST_HEX


def mark(path, text):
    if path:
        with open(path, "a", encoding="utf-8") as output:
            output.write(text + "\n")


def run_conformance(args, stub, metadata, handles, stop_heartbeat):
    """Local simulator control only; every effect still crosses the real Host RPC."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as control:
        control.connect(args.control_socket)
        reader = control.makefile("r", encoding="utf-8")
        writer = control.makefile("w", encoding="utf-8", buffering=1)
        write_lock = threading.Lock()
        frame_lock = threading.Lock()
        frames = {}

        def send(message):
            with write_lock:
                writer.write(json.dumps(message, separators=(",", ":")) + "\n")
                writer.flush()

        def receive_commands():
            try:
                stream = stub.Commands(host_pb2.CommandsRequest(), metadata=metadata)
                # The first stream is admitted before this probe can receive
                # ALREADY_EXISTS. A stream with no queued command has no
                # response headers yet, so initial_metadata() is not a barrier.
                try:
                    next(stub.Commands(host_pb2.CommandsRequest(), metadata=metadata, timeout=5))
                    raise AssertionError("second command stream was admitted")
                except grpc.RpcError as error:
                    assert error.code() == grpc.StatusCode.ALREADY_EXISTS
                send({"event": "ready"})
                for frame in stream:
                    with frame_lock:
                        frames[frame.request.command_id] = frame
                    send({"event": "delivered", "id": frame.request.command_id})
            except grpc.RpcError as error:
                try:
                    send({"event": "stream_error", "code": error.code().name})
                except OSError:
                    pass

        threading.Thread(target=receive_commands, daemon=True).start()
        try:
            for line in reader:
                instruction = json.loads(line)
                if instruction.get("op") == "crash":
                    mark(args.marker, "conformance_crash")
                    os._exit(17)
                command_id = instruction.get("id")
                stimulus = instruction.get("stimulus")
                with frame_lock:
                    frame = frames.get(command_id)
                if frame is None:
                    send({"event": "error", "id": command_id, "reason": "unknown_command"})
                    continue
                if stimulus == "ack":
                    stub.CompleteCommand(host_pb2.CompleteCommandRequest(
                        command_id=command_id, kind=host_pb2.COMMAND_RESULT_KIND_ACCEPTED),
                        metadata=metadata, timeout=5)
                elif stimulus in ("uncorrelated", "wrong_correlation", "stale", "partial_light", "match"):
                    quality = state_pb2.Quality(
                        availability=state_pb2.AVAILABILITY_AVAILABLE,
                        freshness=state_pb2.FRESHNESS_STALE if stimulus == "stale" else state_pb2.FRESHNESS_FRESH,
                        provenance=state_pb2.PROVENANCE_OBSERVED)
                    observation = host_pb2.Observation(
                        binding_handle=frame.binding_handle,
                        endpoint=frame.request.endpoint,
                        quality=quality)
                    if stimulus != "uncorrelated":
                        observation.command_correlation = (
                            "nha095-foreign-correlation" if stimulus == "wrong_correlation" else frame.correlation)
                    if frame.request.capability_schema_id == "core.switch.v1":
                        observation.switch_on = frame.request.arguments.set_switch.on
                    elif frame.request.capability_schema_id == "core.light.v1":
                        light = frame.request.arguments.set_light
                        observation.light.on = light.on
                        if stimulus != "partial_light":
                            observation.light.brightness_percent = light.brightness_percent
                        observation.light.color_temp_mireds = light.color_temp_mireds
                    else:
                        raise AssertionError("unexpected synthetic schema")
                    try:
                        stub.Publish(host_pb2.PublishRequest(observation=observation),
                                     metadata=metadata, timeout=5)
                    except grpc.RpcError as error:
                        if stimulus == "wrong_correlation" and error.code() == grpc.StatusCode.PERMISSION_DENIED:
                            send({"event": "applied", "id": command_id, "stimulus": stimulus,
                                  "rejected": error.code().name})
                            continue
                        raise
                else:
                    send({"event": "error", "id": command_id, "reason": "invalid_stimulus"})
                    continue
                send({"event": "applied", "id": command_id, "stimulus": stimulus})
        finally:
            stop_heartbeat.set()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("normal", "new", "new_hold", "incompatible", "incompatible_host_major", "incompatible_host_minor", "incompatible_driver_major", "incompatible_digest", "incompatible_version_pair", "incompatible_new_digest", "silent", "crash", "fail", "hold", "many_hold", "stubborn", "orphan_child", "small_frame", "large_frame", "no_command", "secret", "conformance"), default="normal")
    parser.add_argument("--marker")
    parser.add_argument("--control-socket")
    parser.add_argument("--child-stubborn", action="store_true")
    args = parser.parse_args()
    if args.child_stubborn:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        while True:
            time.sleep(1)
    if args.mode in ("stubborn", "orphan_child"):
        if args.mode == "stubborn":
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
        child = subprocess.Popen([sys.executable, __file__, "--child-stubborn"])
        mark(args.marker, f"child:{child.pid}")
    socket = os.environ["HOMEWARD_HOST_SOCKET"]
    fd = int(os.environ["HOMEWARD_HOST_TOKEN_FD"])
    token = os.read(fd, 128).decode("ascii")
    os.close(fd)
    metadata = (("authorization", "Bearer " + token),)
    with grpc.insecure_channel("unix:" + socket, options=[("grpc.max_receive_message_length", 1 << 20), ("grpc.max_send_message_length", 1 << 20)]) as channel:
        grpc.channel_ready_future(channel).result(timeout=5)
        stub = host_pb2_grpc.HostServiceStub(channel)
        try:
            welcome = stub.Handshake(host_pb2.HandshakeRequest(
                host_major=2 if args.mode == "incompatible_host_major" else 1,
                host_minor=1 if args.mode in ("new", "new_hold", "incompatible_host_minor", "incompatible_version_pair", "incompatible_new_digest") else 0,
                driver_major=2 if args.mode == "incompatible_driver_major" else 1,
                driver_minor=1 if args.mode in ("new", "new_hold", "incompatible", "incompatible_new_digest") else 0,
                schema_sha256=bytes(32) if args.mode in ("incompatible_digest", "incompatible_new_digest") else bytes.fromhex(SCHEMA_DIGEST_HEX if args.mode in ("new", "new_hold") else LEGACY_SCHEMA_DIGEST_HEX),
                max_frame_bytes=1024 if args.mode == "small_frame" else 2 << 20 if args.mode == "large_frame" else 65536,
                capabilities=[host_pb2.CAPABILITY_DISCOVER, host_pb2.CAPABILITY_PUBLISH,
                              host_pb2.CAPABILITY_EMIT, host_pb2.CAPABILITY_HEALTH,
                              host_pb2.CAPABILITY_COMMAND, host_pb2.CAPABILITY_SECRET_READ]
                              if args.mode != "no_command" else [host_pb2.CAPABILITY_DISCOVER,
                                                                    host_pb2.CAPABILITY_PUBLISH]), metadata=metadata, timeout=5)
        except grpc.RpcError as error:
            mark(args.marker, "handshake_rejected:" + error.code().name)
            if args.mode.startswith("incompatible") and error.code() == grpc.StatusCode.FAILED_PRECONDITION:
                return
            raise
        assert welcome.driver_major == 1 and welcome.driver_minor == (1 if args.mode in ("new", "new_hold") else 0)
        assert welcome.schema_sha256 == bytes.fromhex(SCHEMA_DIGEST_HEX if args.mode in ("new", "new_hold") else LEGACY_SCHEMA_DIGEST_HEX)
        assert (host_pb2.CAPABILITY_COMMAND in welcome.capabilities) == (args.mode != "no_command")
        assert (host_pb2.CAPABILITY_SECRET_READ in welcome.capabilities) == (args.mode == "secret")
        if args.mode == "small_frame":
            assert welcome.max_frame_bytes == 1024
        elif args.mode == "large_frame":
            assert welcome.max_frame_bytes == 1 << 20
        mark(args.marker, "handshake_ok")
        mark(args.marker, f"frame_limit:{welcome.max_frame_bytes}")
        if args.mode == "secret":
            value = stub.ReadSecret(host_pb2.ReadSecretRequest(name="nha094-token"), metadata=metadata, timeout=5)
            assert value.value
            mark(args.marker, "secret_read")
        try:
            stub.ReadSecret(host_pb2.ReadSecretRequest(name="foreign"), metadata=metadata, timeout=5)
            raise AssertionError("foreign or ungranted secret read accepted")
        except grpc.RpcError as error:
            assert error.code() == grpc.StatusCode.PERMISSION_DENIED
            mark(args.marker, "secret_denied")
        candidates = []
        numbers = range(1, 34) if args.mode == "many_hold" else range(1, 3)
        for number in numbers:
            if args.mode == "conformance" and number == 2:
                description = registry_pb2.CapabilityDescription(
                    schema_id="core.light.v1", major=1, display_name="Synthetic light",
                    endpoint="light", light=registry_pb2.LightConstraints(
                        set_on=True,
                        brightness_percent=registry_pb2.NumberBounds(minimum=0, maximum=100),
                        color_temp_mireds=registry_pb2.NumberBounds(minimum=250, maximum=454)))
            else:
                description = registry_pb2.CapabilityDescription(
                    schema_id="core.switch.v1", major=1, display_name=f"Synthetic relay {number}",
                    endpoint="relay", switch=registry_pb2.SwitchConstraints(set_on=True))
            candidates.append(host_pb2.Candidate(
                source=host_pb2.SourceKey(device=f"synthetic-device-{number}",
                                          entity="light-2" if args.mode == "conformance" and number == 2 else f"relay-{number}"),
                descriptions=[description]))
        discovered = stub.Discover(host_pb2.DiscoverRequest(candidates=candidates), metadata=metadata, timeout=5)
        assert len(discovered.bindings) == len(candidates)
        handles = [binding.handle for binding in discovered.bindings]
        again = stub.Discover(host_pb2.DiscoverRequest(candidates=candidates), metadata=metadata, timeout=5)
        assert [binding.handle for binding in again.bindings] == handles
        mark(args.marker, "rediscovered")
        if args.mode == "small_frame":
            oversized = host_pb2.DiscoverRequest(candidates=[host_pb2.Candidate(
                source=host_pb2.SourceKey(device="synthetic-device", entity="x" * 900),
                descriptions=candidates[0].descriptions) for _ in range(2)])
            try:
                stub.Discover(oversized, metadata=metadata, timeout=5)
                raise AssertionError("oversized negotiated frame admitted")
            except grpc.RpcError as error:
                assert error.code() == grpc.StatusCode.RESOURCE_EXHAUSTED
                mark(args.marker, "frame_rejected")
        for number, handle in enumerate(handles, 1):
            observation = host_pb2.Observation(
                binding_handle=handle, endpoint="light" if args.mode == "conformance" and number == 2 else "relay",
                quality=state_pb2.Quality(availability=state_pb2.AVAILABILITY_AVAILABLE,
                                          freshness=state_pb2.FRESHNESS_FRESH,
                                          provenance=state_pb2.PROVENANCE_OBSERVED))
            if args.mode == "conformance" and number == 2:
                observation.light.on = True
                observation.light.brightness_percent = 50.0
                observation.light.color_temp_mireds = 300
            else:
                observation.switch_on = False
            stub.Publish(host_pb2.PublishRequest(observation=observation), metadata=metadata, timeout=5)
            mark(args.marker, f"initial:{number}")
        if args.mode not in ("small_frame", "large_frame", "no_command"):
            stamp = Timestamp()
            stamp.GetCurrentTime()
            stub.Emit(host_pb2.EmitRequest(event=host_pb2.Event(
                binding_handle=handles[0], code="synthetic.observed", observed_at=stamp)), metadata=metadata, timeout=5)
            mark(args.marker, "event_emitted")
        stop_heartbeat = threading.Event()

        def heartbeat():
            while not stop_heartbeat.wait(1):
                try:
                    stub.Heartbeat(host_pb2.HeartbeatRequest(), metadata=metadata, timeout=2)
                except grpc.RpcError:
                    return

        if args.mode not in ("silent", "incompatible"):
            threading.Thread(target=heartbeat, daemon=True).start()
        if args.mode == "conformance":
            assert args.control_socket
            run_conformance(args, stub, metadata, handles, stop_heartbeat)
            return
        if args.mode == "silent":
            time.sleep(9)
            try:
                stub.Publish(host_pb2.PublishRequest(observation=host_pb2.Observation(
                    binding_handle=handles[0], endpoint="relay",
                    quality=state_pb2.Quality(availability=state_pb2.AVAILABILITY_AVAILABLE,
                                              freshness=state_pb2.FRESHNESS_FRESH,
                                              provenance=state_pb2.PROVENANCE_OBSERVED),
                    switch_on=True)), metadata=metadata, timeout=2)
                mark(args.marker, "late_accepted")
            except grpc.RpcError as error:
                mark(args.marker, "late_rejected:" + error.code().name)
            return
        if args.mode == "no_command":
            while True:
                time.sleep(1)
        commands = stub.Commands(host_pb2.CommandsRequest(), metadata=metadata)
        try:
            next(stub.Commands(host_pb2.CommandsRequest(), metadata=metadata, timeout=2))
            raise AssertionError("second command stream was admitted")
        except grpc.RpcError as error:
            assert error.code() == grpc.StatusCode.ALREADY_EXISTS
            mark(args.marker, "second_stream_rejected")
        if args.mode == "new_hold":
            stub.ReportHealth(host_pb2.ReportHealthRequest(
                status=host_pb2.HEALTH_STATUS_READY,
                detail="synthetic ready after initial snapshots"),
                metadata=metadata, timeout=5)
        if args.mode in ("hold", "new_hold", "many_hold"):
            while True:
                time.sleep(1)
        try:
            for frame in commands:
                if args.mode == "crash":
                    mark(args.marker, "crash_on_command")
                    os._exit(17)
                assert frame.request.command_id
                assert frame.binding_handle in handles
                number = handles.index(frame.binding_handle) + 1
                mark(args.marker, f"command:{number}:{frame.request.command_id}")
                if args.mode == "fail":
                    stub.CompleteCommand(host_pb2.CompleteCommandRequest(
                        command_id=frame.request.command_id,
                        kind=host_pb2.COMMAND_RESULT_KIND_FAILED), metadata=metadata, timeout=5)
                    mark(args.marker, f"failed:{frame.request.command_id}")
                    continue
                stub.CompleteCommand(host_pb2.CompleteCommandRequest(
                    command_id=frame.request.command_id,
                    kind=host_pb2.COMMAND_RESULT_KIND_ACCEPTED), metadata=metadata, timeout=5)
                on = frame.request.arguments.set_switch.on
                stub.Publish(host_pb2.PublishRequest(observation=host_pb2.Observation(
                    binding_handle=frame.binding_handle, endpoint="relay",
                    quality=state_pb2.Quality(availability=state_pb2.AVAILABILITY_AVAILABLE,
                                              freshness=state_pb2.FRESHNESS_FRESH,
                                              provenance=state_pb2.PROVENANCE_OBSERVED),
                    switch_on=on, command_correlation=frame.correlation)), metadata=metadata, timeout=5)
                mark(args.marker, f"state:{number}:{frame.request.command_id}")
        except grpc.RpcError:
            pass
        finally:
            stop_heartbeat.set()


if __name__ == "__main__":
    main()
