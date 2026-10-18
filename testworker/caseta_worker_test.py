"""NHA-077 loopback TLS, LEAP and cancellation barriers."""

import asyncio
import dataclasses
import datetime as dt
import hashlib
import json
import logging
import os
import ssl
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch
from pathlib import Path

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

from drivers.caseta.bridge import BridgeFault, StrictSmartbridge, open_pinned_tls
from drivers.caseta.config import Settings, _manifest_aliases, load_settings
from drivers.caseta.worker import (FanSpeed, FanState, FadingLightLevel, HostProfile,
                                   LightLevel, SwitchState, UnsupportedCommand, apply,
                                   description, read_value, requested_effect,
                                   validate_inventory)
from home.actions.v1 import actions_pb2
from testworker.caseta_fake_bridge import FakeBridge


def issue(root, name, ca=None, expired=False):
    key = ec.generate_private_key(ec.SECP256R1())
    now = dt.datetime.now(dt.timezone.utc)
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, name)])
    builder = (x509.CertificateBuilder().subject_name(subject)
               .issuer_name(ca[1].subject if ca else subject)
               .public_key(key.public_key()).serial_number(x509.random_serial_number())
               .not_valid_before(now - dt.timedelta(days=2))
               .not_valid_after(now - dt.timedelta(days=1) if expired else
                                now + dt.timedelta(days=3))
               .add_extension(x509.BasicConstraints(ca=ca is None,
                                                   path_length=None), True))
    cert = builder.sign(ca[0] if ca else key, hashes.SHA256())
    cert_path, key_path = root / (name + ".pem"), root / (name + ".key")
    cert_path.write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    key_path.write_bytes(key.private_bytes(serialization.Encoding.PEM,
        serialization.PrivateFormat.TraditionalOpenSSL,
        serialization.NoEncryption()))
    os.chmod(cert_path, 0o600)
    os.chmod(key_path, 0o600)
    return key, cert, cert_path, key_path


def settings_file(root, pin, port, reconcile_seconds=30):
    functions = {}
    light_id = 2
    for alias in sorted(_manifest_aliases()):
        if alias.startswith("light."):
            device_id = zone_id = str(light_id)
            light_id += 1
        elif alias.startswith("fan."):
            device_id = zone_id = "20"
        elif alias == "switch.gazebo_lights":
            device_id = zone_id = "30"
        else:
            functions[alias] = {"system_id": "1", "source_device": "fake-system-1",
                                "source_entity": "fake-away-1"}
            continue
        functions[alias] = {"device_id": device_id, "zone_id": zone_id,
                            "source_device": "fake-device-" + device_id,
                            "source_entity": "fake-zone-" + zone_id}
    path = root / "caseta-settings.json"
    path.write_text(json.dumps({"host": "127.0.0.1", "port": port,
        "bridge_cert_sha256": pin.hex(),
        "secrets": {"ca": "fake-ca", "client_cert": "fake-cert",
                    "client_key": "fake-key"}, "functions": functions,
        "pico_events": [{"button_id": "101",
                         "binding_alias": "light.living_room_lights"}],
        "reconcile_seconds": reconcile_seconds}))
    os.chmod(path, 0o600)
    return path


class CasetaTLS(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        logging.getLogger("asyncio").setLevel(logging.CRITICAL)
        loop = asyncio.get_running_loop()
        default_handler = loop.get_exception_handler()

        def expected_tls_rejection(loop, context):
            if not isinstance(context.get("exception"), (ConnectionResetError, ssl.SSLError)):
                if default_handler is not None:
                    default_handler(loop, context)
                else:
                    loop.default_exception_handler(context)

        loop.set_exception_handler(expected_tls_rejection)
        self.temp = tempfile.TemporaryDirectory(prefix="nha077-test-", dir="/tmp")
        self.root = Path(self.temp.name)
        self.ca = issue(self.root, "ca")
        self.client = issue(self.root, "client", self.ca)
        self.server = issue(self.root, "server", self.ca)
        self.pin = hashlib.sha256(self.server[1].public_bytes(
            serialization.Encoding.DER)).digest()
        self.fake = FakeBridge(self.server[2], self.server[3], self.ca[2])
        self.port = await self.fake.start()
        self.settings = load_settings(settings_file(self.root, self.pin, self.port))

    async def asyncTearDown(self):
        await self.fake.close()
        self.temp.cleanup()

    async def connect(self):
        async def connector():
            return await open_pinned_tls("127.0.0.1", self.port, self.ca[2],
                                         self.client[2], self.client[3], self.pin)
        bridge = StrictSmartbridge(connector)
        await bridge.connect_once()
        return bridge

    async def test_ca_pin_validity_and_changed_identity(self):
        leap = await open_pinned_tls("127.0.0.1", self.port, self.ca[2],
                                     self.client[2], self.client[3], self.pin)
        leap.close()
        await leap.wait_closed()
        with self.assertRaises(BridgeFault):
            await open_pinned_tls("127.0.0.1", self.port, self.ca[2],
                                  self.client[2], self.client[3], bytes(32))
        foreign_ca = issue(self.root, "foreign-ca")
        with self.assertRaises(ssl.SSLError):
            await open_pinned_tls("127.0.0.1", self.port, foreign_ca[2],
                                  self.client[2], self.client[3], self.pin)
        old_client = issue(self.root, "expired-client", self.ca, expired=True)
        with self.assertRaises(BridgeFault):
            await open_pinned_tls("127.0.0.1", self.port, self.ca[2],
                                  old_client[2], old_client[3], self.pin)
        await self.fake.close()
        old_server = issue(self.root, "expired-server", self.ca, expired=True)
        self.fake = FakeBridge(old_server[2], old_server[3], self.ca[2])
        self.port = await self.fake.start()
        with self.assertRaises(ssl.SSLError):
            await open_pinned_tls("127.0.0.1", self.port, self.ca[2],
                                  self.client[2], self.client[3],
                                  hashlib.sha256(old_server[1].public_bytes(
                                      serialization.Encoding.DER)).digest())

    async def test_inventory_and_ack_observation_are_distinct(self):
        bridge = await self.connect()
        try:
            validate_inventory(bridge, self.settings)
            await bridge.subscribe_required(["101"])
            values = {f.alias: await read_value(bridge, f)
                      for f in self.settings.functions}
            self.assertEqual(len(values), 13)
            self.assertEqual(values["switch.unassigned_smart_away"], SwitchState(False))
            self.assertEqual(values["fan.living_room_ceiling_fan"], FanState(FanSpeed.OFF))
            light = next(f for f in self.settings.functions if f.kind == "light")
            await bridge.set_value(light.device_id, 42)
            self.assertEqual(await read_value(bridge, light), LightLevel(0))
            await self.fake.control({"op": "level", "zone": light.zone_id,
                                     "value": 42, "notify": True})
            self.assertEqual(await read_value(bridge, light), LightLevel(42))
            fan = next(f for f in self.settings.functions if f.kind == "fan")
            await bridge.set_fan(fan.device_id, "High")
            self.assertEqual(await read_value(bridge, fan), FanState(FanSpeed.OFF))
            await self.fake.control({"op": "fan", "speed": "High", "notify": True})
            self.assertEqual(await read_value(bridge, fan), FanState(FanSpeed.HIGH))
            away = next(f for f in self.settings.functions if f.kind == "smart_away")
            await bridge.activate_smart_away()
            self.assertEqual(await read_value(bridge, away), SwitchState(False))
            await self.fake.control({"op": "away", "enabled": True,
                                     "notify": True})
            self.assertEqual(await read_value(bridge, away), SwitchState(True))
            event = asyncio.Event()
            bridge.add_button_subscriber("101", lambda value: event.set()
                                         if value == "Press" else None)
            await self.fake.control({"op": "pico", "event": "Press"})
            await asyncio.wait_for(event.wait(), 2)
        finally:
            await bridge.close_joined()

    async def test_nha178_opt_in_exact_fade_and_legacy_wire(self):
        light = next(f for f in self.settings.functions if f.kind == "light")
        self.assertFalse(description(light).light.HasField("transition_seconds"))
        self.assertEqual(description(light, HostProfile.V13).light.transition_seconds.step, 1)

        def frame(**fields):
            request = actions_pb2.CommandRequest(
                capability_schema_id="core.light.v1", endpoint="level",
                arguments=actions_pb2.ActionArgs(
                    set_light=actions_pb2.SetLight(**fields)))
            return SimpleNamespace(request=request)

        old = requested_effect(frame(brightness_percent=42), light)
        self.assertEqual(old, LightLevel(42))
        with self.assertRaises(UnsupportedCommand):
            requested_effect(frame(brightness_percent=42, transition_seconds=2), light)
        fade = requested_effect(frame(brightness_percent=42, transition_seconds=2),
                                light, HostProfile.V13)
        self.assertEqual(fade, FadingLightLevel(LightLevel(42), 2))
        zero = requested_effect(frame(on=True, transition_seconds=0),
                                light, HostProfile.V13)
        one = requested_effect(frame(on=True, transition_seconds=1),
                               light, HostProfile.V13)
        self.assertEqual(zero, FadingLightLevel(LightLevel(100), 0))
        self.assertEqual(one, FadingLightLevel(LightLevel(100), 1))
        off = requested_effect(frame(on=False, transition_seconds=10),
                               light, HostProfile.V13)
        self.assertEqual(off, FadingLightLevel(LightLevel(0), 10))
        for invalid in (dict(on=True, transition_seconds=.5),
                        dict(on=True, transition_seconds=float("nan")),
                        dict(on=True, transition_seconds=6554),
                        dict(transition_seconds=0),
                        dict(on=False, brightness_percent=0, transition_seconds=1),
                        dict(on=True, color_temp_mireds=300, transition_seconds=1)):
            with self.subTest(invalid=invalid), self.assertRaises(UnsupportedCommand):
                requested_effect(frame(**invalid), light, HostProfile.V13)

        bridge = await self.connect()
        try:
            validate_inventory(bridge, self.settings)
            before = len(self.fake.requests)
            await apply(bridge, light, old)
            await apply(bridge, light, zero)
            await apply(bridge, light, one)
            await apply(bridge, light, fade)
            await apply(bridge, light, off)
            writes = [body for kind, url, body in self.fake.requests[before:]
                      if kind == "CreateRequest" and url.endswith("/commandprocessor")]
            self.assertEqual([body["Command"]["CommandType"] for body in writes],
                             ["GoToLevel", "GoToDimmedLevel", "GoToDimmedLevel",
                              "GoToDimmedLevel", "GoToDimmedLevel"])
            self.assertEqual([body["Command"]["DimmedLevelParameters"] for body in writes[1:]],
                             [{"Level": 100, "FadeTime": "00:00:00"},
                              {"Level": 100, "FadeTime": "00:00:01"},
                              {"Level": 42, "FadeTime": "00:00:02"},
                              {"Level": 0, "FadeTime": "00:00:10"}])
        finally:
            await bridge.close_joined()

    async def test_subscription_refusal_and_expiration(self):
        await self.fake.close()
        self.fake = FakeBridge(self.server[2], self.server[3], self.ca[2],
                               deny_away=True)
        self.port = await self.fake.start()
        bridge = await self.connect()
        try:
            with self.assertRaises(BridgeFault):
                await bridge.subscribe_required(["101"])
        finally:
            await bridge.close_joined()
        await self.fake.close()
        self.fake = FakeBridge(self.server[2], self.server[3], self.ca[2])
        self.port = await self.fake.start()
        bridge = await self.connect()
        await bridge.subscribe_required(["101"])
        try:
            await self.fake.control({"op": "expire",
                "url": "/system/away/1/status"})
            with self.assertRaises(BridgeFault):
                await asyncio.wait_for(bridge.reader_task, 2)
        finally:
            await bridge.close_joined()

    async def test_io_barriers_cancel_login_and_ping_join_transport(self):
        async def connector():
            return await open_pinned_tls("127.0.0.1", self.port, self.ca[2],
                                         self.client[2], self.client[3], self.pin)
        await self.fake.control({"op": "stall", "url": "/device"})
        bridge = StrictSmartbridge(connector)
        login = asyncio.create_task(bridge.connect_once())
        await asyncio.wait_for(self.fake.entered["/device"].wait(), 2)
        login.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await login
        self.assertIsNone(bridge.reader_task)
        await self.fake.control({"op": "release", "url": "/device"})
        bridge = await self.connect()
        await self.fake.control({"op": "stall", "url": "/server/1/status/ping"})
        ping = asyncio.create_task(bridge._request(
            "ReadRequest", "/server/1/status/ping"))
        await asyncio.wait_for(self.fake.entered["/server/1/status/ping"].wait(), 2)
        ping.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await ping
        await bridge.close_joined()
        bridge = await self.connect()
        light = next(f for f in self.settings.functions if f.kind == "light")
        read_url = f"/zone/{light.zone_id}/status"
        await self.fake.control({"op": "stall", "operation": "ReadRequest",
                                 "url": read_url})
        read = asyncio.create_task(read_value(bridge, light))
        await asyncio.wait_for(self.fake.entered["ReadRequest " + read_url].wait(), 2)
        read.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await read
        await bridge.close_joined()
        self.assertIsNone(bridge.reader_task)
        self.assertTrue(self.fake.writer is None or self.fake.writer.is_closing())

    async def test_settings_fd_duplicate_keys_and_closed_types(self):
        path = self.root / "caseta-settings.json"
        original = path.read_text()
        for changed in (original.replace('"port": ' + str(self.port),
                                         '"port": true', 1),
                        original[:-1] + ', "host": "127.0.0.1"}',
                        original.replace('"reconcile_seconds": 30',
                                         '"reconcile_seconds": false', 1)):
            path.write_text(changed)
            with self.assertRaises(ValueError):
                load_settings(path)
        path.write_text(original)
        with patch("os.open", side_effect=OSError("open denied")):
            with self.assertRaises(OSError):
                load_settings(path)
        functions = list(self.settings.functions)
        functions[1] = dataclasses.replace(functions[1],
                                           device_id=functions[0].device_id,
                                           zone_id=functions[0].zone_id)
        with self.assertRaises(ValueError):
            dataclasses.replace(self.settings, functions=tuple(functions))
        functions[1] = dataclasses.replace(functions[1], device_id="99")
        with self.assertRaises(ValueError):
            dataclasses.replace(self.settings, functions=tuple(functions))
        with self.assertRaises(ValueError):
            dataclasses.replace(self.settings, functions=(None,) +
                                self.settings.functions[1:])

    async def test_paged_write_response_never_replays_command(self):
        bridge = await self.connect()
        try:
            await self.fake.control({"op": "page_next_write"})
            with self.assertRaises(BridgeFault):
                await bridge.set_value("2", 42)
            commands = (await self.fake.control({"op": "snapshot"}))["commands"]
            self.assertEqual(commands, {"/zone/2/commandprocessor": 1})
        finally:
            await bridge.close_joined()

    async def test_multiple_healthy_reconciliations_keep_one_connection(self):
        bridge = await self.connect()
        try:
            await bridge.subscribe_required(["101"])
            for _ in range(3):
                values = [await read_value(bridge, f) for f in self.settings.functions]
                self.assertEqual(len(values), 13)
            self.assertIsNotNone(self.fake.writer)
            self.assertFalse(self.fake.writer.is_closing())
            self.assertFalse(any(op in ("CreateRequest", "UpdateRequest")
                                 for op, _, _ in self.fake.requests))
            self.assertEqual(sum(op == "SubscribeRequest" for op, _, _ in
                                 self.fake.requests), 2)
        finally:
            await bridge.close_joined()


if __name__ == "__main__":
    unittest.main()
