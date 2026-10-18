"""One bounded, authenticated Caséta LEAP session; Supervisor owns retries."""

import asyncio
import datetime as dt
import hashlib
import ssl
from pathlib import Path

import orjson
from cryptography import x509
from pylutron_caseta import BridgeDisconnectedError, BridgeResponseError
from pylutron_caseta.leap import LeapProtocol
from pylutron_caseta.messages import Response
from pylutron_caseta.smartbridge import Smartbridge


MAX_FRAME = 65536
MAX_PAGES = 4
MAX_RESPONSE_BYTES = 262144
MAX_DEVICES = 128
MAX_BUTTONS = 64
MAX_IN_FLIGHT = 8
REQUEST_SECONDS = 5


class BridgeFault(Exception):
    """The sole LEAP attempt cannot safely continue."""


def _client_cert_valid(certfile: Path) -> None:
    chain = certfile.read_bytes()
    cert = x509.load_pem_x509_certificate(chain)
    now = dt.datetime.now(dt.timezone.utc)
    if not cert.not_valid_before_utc <= now <= cert.not_valid_after_utc:
        raise BridgeFault("client certificate outside validity window")


async def open_pinned_tls(host: str, port: int, ca: Path, cert: Path,
                          key: Path, expected_pin: bytes) -> LeapProtocol:
    """CA and validity checks plus an admitted leaf pin; no SNI or auto trust."""
    if len(expected_pin) != 32:
        raise BridgeFault("invalid bridge certificate pin")
    _client_cert_valid(cert)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.check_hostname = False  # Identity is the exact, pre-admitted leaf pin below.
    context.verify_mode = ssl.CERT_REQUIRED
    context.load_verify_locations(cafile=str(ca))
    context.load_cert_chain(str(cert), str(key))
    loop = asyncio.get_running_loop()
    reader = asyncio.StreamReader(limit=MAX_FRAME)
    protocol = asyncio.StreamReaderProtocol(reader)
    raw, _ = await asyncio.wait_for(
        loop.create_connection(lambda: protocol, host, port), REQUEST_SECONDS)
    writer = None
    try:
        wrapped = await asyncio.wait_for(
            loop.start_tls(raw, protocol, context, server_hostname=None,
                           ssl_handshake_timeout=REQUEST_SECONDS), REQUEST_SECONDS)
        writer = asyncio.StreamWriter(wrapped, protocol, reader, loop)
        ssl_object = writer.get_extra_info("ssl_object")
        leaf = ssl_object.getpeercert(binary_form=True) if ssl_object else None
        if not leaf or hashlib.sha256(leaf).digest() != expected_pin:
            raise BridgeFault("bridge certificate pin changed")
        return BoundedLeapProtocol(reader, writer)
    except BaseException:
        failed_writer = writer or asyncio.StreamWriter(raw, protocol, reader, loop)
        failed_writer.close()
        try:
            await asyncio.wait_for(failed_writer.wait_closed(), REQUEST_SECONDS)
        except (OSError, asyncio.TimeoutError):
            pass
        raise


class BoundedLeapProtocol(LeapProtocol):
    """The upstream parser is used with hard frame and callback bounds."""

    async def request(self, communique_type, url, body=None, tag=None, paging=None):
        if len(self._in_flight_requests) >= MAX_IN_FLIGHT or len(url) > 256:
            raise BridgeFault("LEAP in-flight or URL limit")
        if body is not None and len(orjson.dumps(body)) > MAX_FRAME // 2:
            raise BridgeFault("LEAP command too large")
        return await super().request(communique_type, url, body, tag, paging)

    async def subscribe(self, url, callback, body=None,
                        communique_type="SubscribeRequest", tag=None):
        if len(self._tagged_subscriptions) >= MAX_BUTTONS + 2:
            raise BridgeFault("LEAP subscription limit")

        def strict_callback(response):
            status = response.Header.StatusCode
            if status is None or not status.is_successful():
                raise BridgeFault("LEAP subscription expired or refused")
            callback(response)

        return await super().subscribe(url, strict_callback, body, communique_type, tag)

    def subscribe_unsolicited(self, callback):
        if len(self._unsolicited_subs) >= 2:
            raise BridgeFault("LEAP unsolicited callback limit")
        return super().subscribe_unsolicited(callback)

    async def run(self):
        try:
            while True:
                frame = await self._reader.readline()
                if not frame:
                    raise BridgeDisconnectedError()
                if len(frame) > MAX_FRAME:
                    raise BridgeFault("LEAP frame limit")
                raw = orjson.loads(frame)
                if not isinstance(raw, dict) or not isinstance(raw.get("Header"), dict):
                    raise BridgeFault("invalid LEAP frame")
                tag = raw["Header"].pop("ClientTag", None)
                response = Response.from_json(raw)
                if tag is not None:
                    pending = self._in_flight_requests.pop(tag, None)
                    if pending is not None and not pending.done():
                        pending.set_result(response)
                    else:
                        callback = self._tagged_subscriptions.get(tag)
                        if callback is None:
                            raise BridgeFault("unexpected LEAP tag")
                        callback(response)
                else:
                    for callback in self._unsolicited_subs:
                        callback(response)
        finally:
            self.close()


class StrictSmartbridge(Smartbridge):
    """Uses pylutron parsing/operations without its reconnect or silent retries."""

    def __init__(self, connector):
        super().__init__(connector)
        self.reader_task = None

    async def connect_once(self):
        if self._leap is not None:
            raise BridgeFault("duplicate bridge connection")
        self._leap = await self._connect()
        self._leap.subscribe_unsolicited(self._handle_unsolicited)
        self.reader_task = asyncio.create_task(self._leap.run())
        try:
            project = await self._request("ReadRequest", "/project")
            if not isinstance(project.Body, dict) or project.Body.get("Project", {}).get("ProductType") in (
                "Lutron RadioRA 3 Project", "Lutron HWQS Project"):
                raise BridgeFault("non-Caséta bridge")
            await self._load_devices()
            await self._load_buttons()
            if len(self.devices) > MAX_DEVICES or len(self.buttons) > MAX_BUTTONS:
                raise BridgeFault("bridge inventory limit")
        except BaseException:
            await self.close_joined()
            raise

    async def subscribe_required(self, button_ids):
        try:
            for button_id in button_ids:
                await self._subscribe(f"/button/{button_id}/status/event",
                                      self._handle_button_status)
            response, _ = await self._subscribe(
                "/system/away/1/status", self._handle_smart_away_status)
            self._handle_smart_away_status(response)
        except BridgeResponseError as error:
            raise BridgeFault("required LEAP subscription refused") from error

    async def _request(self, communique_type, url, body=None):
        if self._leap is None:
            raise BridgeDisconnectedError()
        responses = []
        paging = None
        seen = set()
        size = 0
        for _ in range(MAX_PAGES):
            response = await asyncio.wait_for(
                self._leap.request(communique_type, url, body, paging=paging),
                REQUEST_SECONDS)
            status = response.Header.StatusCode
            if status is None or not status.is_successful():
                raise BridgeFault("LEAP request refused")
            encoded = orjson.dumps(response.Body)
            size += len(encoded)
            if size > MAX_RESPONSE_BYTES:
                raise BridgeFault("LEAP paginated response limit")
            responses.append(response)
            paging = response.Header.Paging
            if communique_type != "ReadRequest" and paging is not None:
                raise BridgeFault("LEAP write response cannot request pagination")
            if not paging:
                break
            marker = orjson.dumps(paging)
            if marker in seen:
                raise BridgeFault("LEAP repeated pagination cursor")
            seen.add(marker)
        else:
            raise BridgeFault("LEAP pagination limit")
        merged = responses[0]
        if len(responses) > 1 and isinstance(merged.Body, dict):
            kind = next(iter(merged.Body), None)
            if kind is None or not isinstance(merged.Body[kind], list):
                raise BridgeFault("invalid LEAP pagination body")
            for page in responses[1:]:
                if not isinstance(page.Body, dict) or not isinstance(page.Body.get(kind), list):
                    raise BridgeFault("invalid LEAP pagination page")
                merged.Body[kind].extend(page.Body[kind])
                if len(merged.Body[kind]) > MAX_DEVICES:
                    raise BridgeFault("LEAP paginated item limit")
        return merged

    async def close_joined(self):
        leap = self._leap
        self._leap = None
        if leap is not None:
            leap.close()
        task = self.reader_task
        self.reader_task = None
        if task is not None:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        if leap is not None:
            await asyncio.wait_for(leap.wait_closed(), REQUEST_SECONDS)
