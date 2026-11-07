"""Device-free regression tests of the exact native control dispatch boundary."""
import dataclasses
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock

import aioesphomeapi as esp

from drivers.esphome.config import Function, Kind, Operation, Transport, Settings, load_settings
from drivers.esphome.worker import (ProtocolFault, controls, description, dispatch,
                                    host_pb2, state_value, timestamp)


def function(kind, operations=(), code=None):
    return Function(kind, 123, "synthetic_control", "control", tuple(Operation(v) for v in operations), code)


def command(kind, op):
    frame = host_pb2.CommandsResponse()
    frame.request.command_id = "test-command"
    frame.request.capability_schema_id = "core." + kind.value + ".v1"
    frame.request.endpoint = "native"
    frame.request.expires_at.CopyFrom(timestamp())
    frame.request.expires_at.seconds += 30
    getattr(frame.request.arguments, kind.value).operation = op
    return frame


class ESPHomeControls(unittest.TestCase):
    def test_alarm_requires_secret_for_disarm_and_respects_features(self):
        f = function(Kind.ALARM, ("arm_home", "arm_away", "disarm"))
        info = esp.AlarmControlPanelInfo(key=123, object_id=f.object_id,
                                        supported_features=1, requires_code=True)
        d = description(f, info, None)
        self.assertEqual(list(d.alarm.operations), [controls.ALARM_OPERATION_ARM_HOME])
        d = description(f, info, "9876")
        self.assertEqual(list(d.alarm.operations), [controls.ALARM_OPERATION_ARM_HOME, controls.ALARM_OPERATION_DISARM])
        armed = dataclasses.replace(info, requires_code_to_arm=True)
        self.assertEqual(list(description(f, armed, None).alarm.operations), [])

    def test_night_is_advertised_only_when_supported_and_never_trigger(self):
        f = function(Kind.ALARM, ("arm_night",))
        info = esp.AlarmControlPanelInfo(key=123, supported_features=15)
        d = description(f, info, None)
        self.assertEqual(list(d.alarm.operations), [controls.ALARM_OPERATION_ARM_NIGHT])
        client = Mock()
        dispatch(client, f, info, d, None, command(Kind.ALARM, controls.ALARM_OPERATION_ARM_NIGHT))
        client.alarm_control_panel_command.assert_called_once_with(123, esp.AlarmControlPanelCommand.ARM_NIGHT, None, device_id=0)
        self.assertEqual(list(description(f, dataclasses.replace(info, supported_features=3), None).alarm.operations), [])

    def test_alarm_dispatch_passes_code_without_mutating_host_command(self):
        f = function(Kind.ALARM, ("disarm",), "alarm-code")
        info = esp.AlarmControlPanelInfo(key=123, requires_code=True)
        frame = command(Kind.ALARM, controls.ALARM_OPERATION_DISARM)
        before = frame.SerializeToString()
        client = Mock()
        dispatch(client, f, info, description(f, info, "9876"), "9876", frame)
        client.alarm_control_panel_command.assert_called_once_with(123, esp.AlarmControlPanelCommand.DISARM, "9876", device_id=0)
        self.assertEqual(before, frame.SerializeToString())
        self.assertNotIn(b"9876", before)

    def test_cover_close_is_absolute_and_never_toggle(self):
        f = function(Kind.COVER, ("close",))
        info = esp.CoverInfo(key=123, supports_stop=True, supports_position=True)
        d = description(f, info, None)
        client = Mock()
        dispatch(client, f, info, d, None, command(Kind.COVER, controls.COVER_OPERATION_CLOSE))
        client.cover_command.assert_called_once_with(123, position=0.0, device_id=0)
        for operation in (controls.COVER_OPERATION_OPEN, controls.COVER_OPERATION_STOP, 99):
            with self.assertRaises(ProtocolFault):
                dispatch(client, f, info, d, None, command(Kind.COVER, operation))
        self.assertEqual(client.cover_command.call_count, 1)

    def test_expired_or_mismatched_commands_never_dispatch(self):
        f = function(Kind.COVER, ("close",))
        info = esp.CoverInfo(key=123)
        d = description(f, info, None)
        for field in ("endpoint", "capability_schema_id", "expired"):
            frame = command(Kind.COVER, controls.COVER_OPERATION_CLOSE)
            if field == "expired":
                frame.request.expires_at.seconds = 1
            else:
                setattr(frame.request, field, "wrong")
            client = Mock()
            with self.assertRaises(ProtocolFault):
                dispatch(client, f, info, d, None, frame)
            self.assertEqual(client.mock_calls, [])

    def test_transport_failure_is_not_retried(self):
        f = function(Kind.COVER, ("close",))
        info = esp.CoverInfo(key=123)
        client = Mock()
        client.cover_command.side_effect = ConnectionError("test")
        with self.assertRaises(ConnectionError):
            dispatch(client, f, info, description(f, info, None), None,
                     command(Kind.COVER, controls.COVER_OPERATION_CLOSE))
        self.assertEqual(client.cover_command.call_count, 1)

    def test_lock_observes_all_states_without_commands_or_fake_unknown(self):
        f = function(Kind.LOCK)
        info = esp.LockInfo(key=123)
        self.assertEqual(list(description(f, info, None).lock.operations), [])
        self.assertIsNone(state_value(f, info, esp.LockEntityState(key=123, state=esp.LockState.NONE), esp.APIVersion(1, 10)))
        self.assertEqual(state_value(f, info, esp.LockEntityState(key=123, state=esp.LockState.UNLOCKED),
                                     esp.APIVersion(1, 10)).state, controls.LOCK_STATE_UNLOCKED)
        with self.assertRaises(ProtocolFault):
            dispatch(Mock(), f, info, description(f, info, None), None,
                     command(Kind.LOCK, controls.LOCK_OPERATION_LOCK))

    def test_cover_state_keeps_zero_position_and_real_motion(self):
        f = function(Kind.COVER)
        info = esp.CoverInfo(key=123, supports_position=True)
        value = state_value(f, info, esp.CoverState(key=123, position=0), esp.APIVersion(1, 10))
        self.assertEqual(value.state, controls.COVER_STATE_CLOSED)
        self.assertTrue(value.HasField("position_percent"))
        self.assertEqual(value.position_percent, 0)
        moving = state_value(f, info, esp.CoverState(key=123, position=.4, current_operation=esp.CoverOperation.IS_CLOSING), esp.APIVersion(1, 10))
        self.assertEqual(moving.state, controls.COVER_STATE_CLOSING)
        self.assertEqual(moving.position_percent, 40)
        with self.assertRaises(ProtocolFault):
            state_value(f, info, esp.CoverState(key=123, position=float("nan")), esp.APIVersion(1, 10))
        with self.assertRaises(ProtocolFault):
            state_value(f, info, esp.CoverState(key=124), esp.APIVersion(1, 10))

    def test_alarm_unknown_never_becomes_disarmed(self):
        f = function(Kind.ALARM)
        info = esp.AlarmControlPanelInfo(key=123)
        self.assertIsNone(state_value(f, info, esp.AlarmControlPanelEntityState(key=123, state=None), esp.APIVersion(1, 10)))
        for source, target in ((esp.AlarmControlPanelState.ARMING, controls.ALARM_STATE_ARMING),
                               (esp.AlarmControlPanelState.DISARMED, controls.ALARM_STATE_DISARMED)):
            self.assertEqual(state_value(f, info, esp.AlarmControlPanelEntityState(key=123, state=source), esp.APIVersion(1, 10)).state, target)

    def test_configuration_has_no_implicit_plaintext_fallback(self):
        args = dict(host="device.example", port=6053, expected_mac="aa:bb:cc:dd:ee:01",
                    source_device="synthetic", transport=Transport.NOISE, noise_secret="key", password_secret=None,
                    functions=(function(Kind.COVER),))
        Settings(**args)
        with self.assertRaises(ValueError):
            Settings(**{**args, "noise_secret": None})
        with self.assertRaises(ValueError):
            Settings(**{**args, "transport": Transport.PLAINTEXT})
        with self.assertRaises(ValueError):
            Function(Kind.LOCK, 1, "lock", "lock", ("unlock",))
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "config.json"
            path.write_text("{}")
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                load_settings(path)


class NativeSessionComposition(unittest.IsolatedAsyncioTestCase):
    async def test_publication_preserves_receipt_time_without_claiming_correlation(self):
        from unittest.mock import AsyncMock
        from drivers.esphome.worker import Host
        from google.protobuf.timestamp_pb2 import Timestamp
        host = object.__new__(Host)
        host.sequence = 0
        host.handles = {123: b"h" * 32}
        host.call = AsyncMock()
        receipt = Timestamp(seconds=123, nanos=456)
        await host.publish(function(Kind.LOCK), controls.LockValue(state=controls.LOCK_STATE_LOCKED), receipt)
        observation = host.call.call_args.args[1].observation
        self.assertEqual(observation.observed_at, receipt)
        self.assertFalse(observation.HasField("command_correlation"))

    async def test_supervised_session_discovers_observes_dispatches_once_and_disconnects(self):
        import asyncio
        import os
        from unittest.mock import patch
        import grpc
        from drivers.esphome import worker
        from home.integrations.v1 import host_pb2_grpc
        from testworker.schema_digest import HOST_MAJOR, HOST_MINOR, SCHEMA_DIGEST_HEX

        calls = []
        published = asyncio.Event()
        accepted = asyncio.Event()

        class Client:
            api_version = esp.APIVersion(1, 10)
            async def connect(self, on_stop, **kwargs):
                self.on_stop = on_stop
                calls.append("connect")
            async def device_info(self):
                return esp.DeviceInfo(mac_address="aa:bb:cc:dd:ee:01")
            async def list_entities_services(self):
                return [esp.CoverInfo(key=123, object_id="synthetic_control", supports_position=True)], []
            def subscribe_states(self, callback):
                calls.append("subscribe")
                callback(esp.CoverState(key=123, position=1))
            def cover_command(self, key, **kwargs):
                calls.append((key, kwargs))
            async def disconnect(self):
                calls.append("disconnect")

        client = Client()

        class Service(host_pb2_grpc.HostServiceServicer):
            async def Handshake(self, request, context):
                self_test.assertEqual(request.schema_sha256, bytes.fromhex(SCHEMA_DIGEST_HEX))
                return host_pb2.HandshakeResponse(host_major=HOST_MAJOR, host_minor=HOST_MINOR,
                    driver_major=HOST_MAJOR, driver_minor=HOST_MINOR, schema_sha256=request.schema_sha256,
                    capabilities=request.capabilities, max_frame_bytes=65536)
            async def Discover(self, request, context):
                calls.append("discover")
                self_test.assertEqual(request.candidates[0].source.device, "synthetic")
                self_test.assertEqual(list(request.candidates[0].descriptions[0].cover.operations),
                                      [controls.COVER_OPERATION_CLOSE])
                return host_pb2.DiscoverResponse(bindings=[host_pb2.Binding(handle=b"h" * 32)])
            async def Publish(self, request, context):
                calls.append("observe")
                self_test.assertFalse(request.observation.HasField("command_correlation"))
                self_test.assertEqual(request.observation.cover.state, controls.COVER_STATE_OPEN)
                published.set()
                return host_pb2.PublishResponse()
            async def ReportHealth(self, request, context):
                return host_pb2.ReportHealthResponse()
            async def Heartbeat(self, request, context):
                return host_pb2.HeartbeatResponse()
            async def Commands(self, request, context):
                await published.wait()
                frame = command(Kind.COVER, controls.COVER_OPERATION_CLOSE)
                frame.binding_handle = b"h" * 32
                yield frame
                await accepted.wait()
                await asyncio.Event().wait()
            async def CompleteCommand(self, request, context):
                self_test.assertEqual(request.kind, host_pb2.COMMAND_RESULT_KIND_ACCEPTED)
                accepted.set()
                await client.on_stop(False)
                return host_pb2.CompleteCommandResponse()

        self_test = self
        with tempfile.TemporaryDirectory() as folder:
            socket = str(Path(folder) / "host.sock")
            server = grpc.aio.server()
            host_pb2_grpc.add_HostServiceServicer_to_server(Service(), server)
            server.add_insecure_port("unix:" + socket)
            await server.start()
            read_fd, write_fd = os.pipe()
            os.write(write_fd, b"1" * 64)
            os.close(write_fd)
            settings = Settings("device.example", 6053, "aa:bb:cc:dd:ee:01", "synthetic", Transport.PLAINTEXT, None,
                                None, (function(Kind.COVER, ("close",)),))
            try:
                with patch.dict(os.environ, HOMEWARD_HOST_SOCKET=socket, HOMEWARD_HOST_TOKEN_FD=str(read_fd)), \
                        patch.object(worker.esp, "APIClient", return_value=client) as constructor, \
                        patch.object(asyncio.get_running_loop(), "add_signal_handler"):
                    with self.assertRaises(ProtocolFault):
                        await asyncio.wait_for(worker.run(settings), 5)
                    self.assertEqual(constructor.call_args.kwargs["expected_mac"], "aabbccddee01")
                    self.assertFalse(constructor.call_args.kwargs["provide_time"])
            finally:
                await server.stop(None)
        self.assertEqual(calls, ["connect", "discover", "subscribe", "observe",
                                 (123, {"position": 0.0, "device_id": 0}), "disconnect"])


if __name__ == "__main__":
    unittest.main()
