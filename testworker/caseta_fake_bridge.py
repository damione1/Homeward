"""Loopback-only mTLS LEAP simulator for NHA-077; no home-network defaults."""

import argparse
import asyncio
import json
import os
import signal
import ssl
from pathlib import Path


def device(identifier, zone=None, kind="WallDimmer", button_group=None):
    item = {"href": f"/device/{identifier}", "Name": f"Synthetic {identifier}",
            "FullyQualifiedName": ["Synthetic", str(identifier)],
            "SerialNumber": int(identifier), "ModelNumber": "FAKE-DEVICE",
            "DeviceType": kind, "Parent": {"href": "/project"}}
    if zone is not None:
        item["LocalZones"] = [{"href": f"/zone/{zone}"}]
    if button_group is not None:
        item["ButtonGroups"] = [{"href": f"/buttongroup/{button_group}"}]
    return item


class FakeBridge:
    def __init__(self, cert, key, ca, deny_away=False):
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(cert), str(key))
        context.load_verify_locations(cafile=str(ca))
        context.verify_mode = ssl.CERT_REQUIRED
        self.context = context
        self.deny_away = deny_away
        self.server = None
        self.writer = None
        self.write_lock = asyncio.Lock()
        self.subscriptions = {}
        self.requests = []
        self.commands = {}
        self.levels = {str(i): 0 for i in range(2, 12)}
        self.levels["30"] = 0
        self.fan = "Off"
        self.away = "Disabled"
        self.stalled = {}
        self.entered = {}
        self.arm_after = {}
        self.command_response_gates = {}
        self.page_next_write = False

    async def start(self):
        self.server = await asyncio.start_server(self.handle, "127.0.0.1", 0,
                                                 ssl=self.context, limit=65536)
        return self.server.sockets[0].getsockname()[1]

    async def close(self):
        for gate in self.stalled.values():
            gate.set()
        for gate in self.command_response_gates.values():
            gate.set()
        self.server.close()
        await self.server.wait_closed()
        if self.writer is not None:
            self.writer.close()
            await self.writer.wait_closed()

    async def send(self, response, writer=None):
        target = writer or self.writer
        if target is None or target.is_closing():
            return
        async with self.write_lock:
            target.write(json.dumps(response, separators=(",", ":")).encode() + b"\r\n")
            await target.drain()

    def response(self, url, body, tag=None, code=200, communique="ReadResponse",
                 body_type="Synthetic"):
        header = {"StatusCode": f"{code} {'OK' if code == 200 else 'Denied'}",
                  "Url": url, "MessageBodyType": body_type}
        if tag is not None:
            header["ClientTag"] = tag
        return {"CommuniqueType": communique, "Header": header, "Body": body}

    async def handle(self, reader, writer):
        self.writer = writer
        try:
            while True:
                frame = await reader.readline()
                if not frame:
                    break
                request = json.loads(frame)
                header = request["Header"]
                url = header["Url"]
                tag = header.get("ClientTag")
                operation = request["CommuniqueType"]
                self.requests.append((operation, url, request.get("Body")))
                if operation == "CreateRequest" and url.endswith("/commandprocessor"):
                    self.commands[url] = self.commands.get(url, 0) + 1
                elif operation == "UpdateRequest" and url == "/system/away/1/status":
                    self.commands[url] = self.commands.get(url, 0) + 1
                gate_key = f"{operation} {url}"
                if gate_key not in self.stalled:
                    gate_key = url
                if gate_key in self.stalled:
                    self.entered[gate_key].set()
                    await self.stalled[gate_key].wait()
                if operation == "SubscribeRequest":
                    if url == "/system/away/1/status" and self.deny_away:
                        response = self.response(url, {}, tag, 403, "SubscribeResponse")
                    elif url == "/system/away/1/status":
                        self.subscriptions[url] = tag
                        response = self.response(url, {"AwayStatus": {"EnabledState": self.away}},
                                                 tag, communique="SubscribeResponse",
                                                 body_type="OneAwayStatus")
                    elif url == "/button/101/status/event":
                        self.subscriptions[url] = tag
                        response = self.response(url, {}, tag, communique="SubscribeResponse")
                    else:
                        response = self.response(url, {}, tag, 404, "SubscribeResponse")
                elif operation == "ReadRequest":
                    response = self.read(url, tag)
                elif operation == "CreateRequest" and url.endswith("/commandprocessor"):
                    response = self.response(url, {}, tag, communique="CreateResponse")
                elif operation == "UpdateRequest" and url == "/system/away/1/status":
                    response = self.response(url, {}, tag, communique="UpdateResponse")
                else:
                    response = self.response(url, {}, tag, 404)
                if self.page_next_write and operation in ("CreateRequest", "UpdateRequest"):
                    response["Header"]["Paging"] = {"Next": "/synthetic/replay"}
                    self.page_next_write = False
                if operation in ("CreateRequest", "UpdateRequest") and url in self.arm_after:
                    read_url = self.arm_after.pop(url)
                    key = f"ReadRequest {read_url}"
                    self.stalled[key] = asyncio.Event()
                    self.entered[key] = asyncio.Event()
                    await self.command_response_gates[url].wait()
                    self.command_response_gates.pop(url, None)
                await self.send(response, writer)
        except (ConnectionResetError, BrokenPipeError, asyncio.CancelledError):
            pass
        finally:
            writer.close()
            try:
                await writer.wait_closed()
            except (ConnectionResetError, BrokenPipeError, ssl.SSLError):
                pass
            if self.writer is writer:
                self.writer = None

    def read(self, url, tag):
        if url == "/project":
            return self.response(url, {"Project": {"ProductType": "Lutron Smart Bridge Project"}},
                                 tag, body_type="OneProjectDefinition")
        if url == "/device":
            bridge = device("1", kind="SmartBridge")
            bridge["ModelNumber"] = "L-BDG2-WH"
            devices = [bridge]
            devices.extend(device(str(i), str(i)) for i in range(2, 12))
            devices.append(device("20", "20", kind="CasetaFanSpeedController"))
            devices.append(device("30", "30", kind="OutdoorPlugInSwitch"))
            devices.append(device("40", kind="Pico3Button", button_group="40"))
            return self.response(url, {"Devices": devices}, tag,
                                 body_type="MultipleDeviceDefinition")
        if url == "/button":
            return self.response(url, {"Buttons": [{"href": "/button/101",
                "ButtonNumber": 1, "Parent": {"href": "/buttongroup/40"},
                "Name": "Synthetic Pico"}]}, tag, body_type="MultipleButtonDefinition")
        if url == "/system/away/1/status":
            return self.response(url, {"AwayStatus": {"EnabledState": self.away}},
                                 tag, body_type="OneAwayStatus")
        if url.startswith("/zone/") and url.endswith("/status"):
            zone = url.split("/")[2]
            if zone == "20":
                status = {"Zone": {"href": "/zone/20"}, "FanSpeed": self.fan}
            elif zone in self.levels:
                status = {"Zone": {"href": f"/zone/{zone}"},
                          "Level": self.levels[zone]}
            else:
                return self.response(url, {}, tag, 404)
            return self.response(url, {"ZoneStatus": status}, tag,
                                 body_type="OneZoneStatus")
        if url == "/server/1/status/ping":
            return self.response(url, {}, tag, body_type="OneServerStatus")
        return self.response(url, {}, tag, 404)

    async def control(self, message):
        op = message["op"]
        if op == "snapshot":
            return {"requests": self.requests, "connected": self.writer is not None,
                    "commands": dict(self.commands),
                    "entered": {url: gate.is_set() for url, gate in self.entered.items()},
                    "subscriptions": sorted(self.subscriptions)}
        if op == "level":
            zone = message["zone"]
            self.levels[zone] = message["value"]
            if message.get("notify", False):
                await self.send(self.response(f"/zone/{zone}/status",
                    {"ZoneStatus": {"Zone": {"href": f"/zone/{zone}"},
                                    "Level": self.levels[zone]}},
                    body_type="OneZoneStatus"))
        elif op == "fan":
            self.fan = message["speed"]
            if message.get("notify", False):
                await self.send(self.response("/zone/20/status",
                    {"ZoneStatus": {"Zone": {"href": "/zone/20"},
                                    "FanSpeed": self.fan}}, body_type="OneZoneStatus"))
        elif op == "away":
            self.away = "Enabled" if message["enabled"] else "Disabled"
            if message.get("notify", False):
                await self.send(self.response("/system/away/1/status",
                    {"AwayStatus": {"EnabledState": self.away}},
                    self.subscriptions.get("/system/away/1/status"),
                    communique="UpdateResponse", body_type="OneAwayStatus"))
        elif op == "pico":
            await self.send(self.response("/button/101/status/event",
                {"ButtonStatus": {"Button": {"href": "/button/101"},
                                  "ButtonEvent": {"EventType": message["event"]}}},
                self.subscriptions.get("/button/101/status/event"),
                body_type="OneButtonStatusEvent"))
        elif op == "expire":
            url = message["url"]
            await self.send(self.response(url, {}, self.subscriptions[url], 410,
                                          "UpdateResponse"))
        elif op == "stall":
            url = message["url"]
            key = f"{message['operation']} {url}" if "operation" in message else url
            self.stalled[key] = asyncio.Event()
            self.entered[key] = asyncio.Event()
        elif op == "arm_after_command":
            self.arm_after[message["command_url"]] = message["read_url"]
            self.command_response_gates[message["command_url"]] = asyncio.Event()
        elif op == "release_command_response":
            self.command_response_gates[message["command_url"]].set()
        elif op == "page_next_write":
            self.page_next_write = True
        elif op == "release":
            key = f"{message['operation']} {message['url']}" if "operation" in message else message["url"]
            self.stalled[key].set()
        elif op == "close":
            if self.writer is not None:
                self.writer.close()
        elif op == "entered":
            key = f"{message['operation']} {message['url']}" if "operation" in message else message["url"]
            return {"entered": {key: self.entered[key].is_set()}}
        else:
            raise ValueError("unknown fake control operation")
        return {"ok": True}


async def main():
    parser = argparse.ArgumentParser()
    for name in ("cert", "key", "ca", "ready", "control"):
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--deny-away", action="store_true")
    args = parser.parse_args()
    bridge = FakeBridge(args.cert, args.key, args.ca, args.deny_away)
    port = await bridge.start()
    if args.control.exists():
        args.control.unlink()

    async def handle_control(reader, writer):
        try:
            async for line in reader:
                response = await bridge.control(json.loads(line))
                writer.write(json.dumps(response).encode() + b"\n")
                await writer.drain()
        finally:
            writer.close()
            await writer.wait_closed()

    control = await asyncio.start_unix_server(handle_control, path=str(args.control))
    os.chmod(args.control, 0o600)
    args.ready.write_text(str(port))
    os.chmod(args.ready, 0o600)
    stop = asyncio.Event()
    for sig in (signal.SIGTERM, signal.SIGINT):
        asyncio.get_running_loop().add_signal_handler(sig, stop.set)
    await stop.wait()
    control.close()
    await control.wait_closed()
    await bridge.close()


if __name__ == "__main__":
    asyncio.run(main())
