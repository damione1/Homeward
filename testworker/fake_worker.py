"""Synthetic, device-free Host v1 worker using generated Python gRPC stubs."""
import argparse
import os
import pathlib
import signal
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
from schema_digest import SCHEMA_DIGEST_HEX


def mark(path, text):
    if path:
        with open(path, "a", encoding="utf-8") as output:
            output.write(text + "\n")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("normal", "incompatible", "silent", "crash", "fail", "hold", "many_hold", "stubborn", "orphan_child", "small_frame", "large_frame", "no_command", "secret"), default="normal")
    parser.add_argument("--marker")
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
                host_major=1, host_minor=0, driver_major=1,
                driver_minor=1 if args.mode == "incompatible" else 0,
                schema_sha256=bytes.fromhex(SCHEMA_DIGEST_HEX),
                max_frame_bytes=1024 if args.mode == "small_frame" else 2 << 20 if args.mode == "large_frame" else 65536,
                capabilities=[host_pb2.CAPABILITY_DISCOVER, host_pb2.CAPABILITY_PUBLISH,
                              host_pb2.CAPABILITY_EMIT, host_pb2.CAPABILITY_HEALTH,
                              host_pb2.CAPABILITY_COMMAND, host_pb2.CAPABILITY_SECRET_READ]
                              if args.mode != "no_command" else [host_pb2.CAPABILITY_DISCOVER,
                                                                    host_pb2.CAPABILITY_PUBLISH]), metadata=metadata, timeout=5)
        except grpc.RpcError as error:
            mark(args.marker, "handshake_rejected:" + error.code().name)
            if args.mode == "incompatible" and error.code() == grpc.StatusCode.FAILED_PRECONDITION:
                return
            raise
        assert welcome.driver_major == 1 and welcome.driver_minor == 0
        assert welcome.schema_sha256 == bytes.fromhex(SCHEMA_DIGEST_HEX)
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
            candidates.append(host_pb2.Candidate(
                source=host_pb2.SourceKey(device=f"synthetic-device-{number}", entity=f"relay-{number}"),
                descriptions=[registry_pb2.CapabilityDescription(
                    schema_id="core.switch.v1", major=1, display_name=f"Synthetic relay {number}",
                    endpoint="relay", switch=registry_pb2.SwitchConstraints(set_on=True))]))
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
            stub.Publish(host_pb2.PublishRequest(observation=host_pb2.Observation(
                binding_handle=handle, endpoint="relay",
                quality=state_pb2.Quality(availability=state_pb2.AVAILABILITY_AVAILABLE,
                                          freshness=state_pb2.FRESHNESS_FRESH,
                                          provenance=state_pb2.PROVENANCE_OBSERVED),
                switch_on=False)), metadata=metadata, timeout=5)
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
        if args.mode in ("hold", "many_hold"):
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
