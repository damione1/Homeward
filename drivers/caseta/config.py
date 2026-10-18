"""Explicit operator mapping of the immutable manifest to open LEAP IDs."""

import dataclasses
import enum
import json
import os
import re
import stat
from pathlib import Path


WIRE_ID = re.compile(r"[1-9][0-9]{0,9}\Z")
SOURCE = re.compile(r"[A-Za-z0-9][A-Za-z0-9:._/-]{0,127}\Z")


class FunctionKind(str, enum.Enum):
    LIGHT = "light"
    SWITCH = "switch"
    FAN = "fan"
    SMART_AWAY = "smart_away"


@dataclasses.dataclass(frozen=True)
class Function:
    alias: str
    kind: FunctionKind
    device_id: str | None
    zone_id: str | None
    source_device: str
    source_entity: str

    def __post_init__(self):
        if type(self.kind) is not FunctionKind or self.alias not in _manifest_aliases() or \
                self.kind != (FunctionKind.SMART_AWAY if self.alias ==
                "switch.unassigned_smart_away" else FunctionKind(
                    self.alias.split(".", 1)[0])) or \
                not all(isinstance(v, str) and SOURCE.fullmatch(v)
                        for v in (self.source_device, self.source_entity)):
            raise ValueError("invalid function identity")
        if self.kind is FunctionKind.SMART_AWAY:
            if self.device_id is not None or self.zone_id is not None:
                raise ValueError("Smart Away has no zone")
        elif not all(isinstance(v, str) and WIRE_ID.fullmatch(v)
                     for v in (self.device_id, self.zone_id)):
            raise ValueError("invalid function wire ID")


@dataclasses.dataclass(frozen=True)
class Pico:
    button_id: str
    binding_alias: str

    def __post_init__(self):
        if not isinstance(self.button_id, str) or not WIRE_ID.fullmatch(self.button_id) or \
                self.binding_alias not in _manifest_aliases():
            raise ValueError("invalid Pico binding")


@dataclasses.dataclass(frozen=True)
class Settings:
    host: str
    port: int
    pin: bytes
    ca_secret: str
    cert_secret: str
    key_secret: str
    functions: tuple[Function, ...]
    picos: tuple[Pico, ...]
    reconcile_seconds: int

    def __post_init__(self):
        if not isinstance(self.host, str) or not 1 <= len(self.host) <= 253 or \
                any(c in self.host for c in "/\\\x00 \t\r\n") or \
                type(self.port) is not int or not 1 <= self.port <= 65535 or \
                not isinstance(self.pin, bytes) or len(self.pin) != 32 or \
                type(self.reconcile_seconds) is not int or \
                not 1 <= self.reconcile_seconds <= 3600 or \
                type(self.functions) is not tuple or len(self.functions) != 13 or \
                type(self.picos) is not tuple or len(self.picos) > 16:
            raise ValueError("invalid Caséta settings")
        if any(type(f) is not Function for f in self.functions) or \
                any(type(p) is not Pico for p in self.picos):
            raise ValueError("invalid Caséta mapping value")
        zone_functions = [f for f in self.functions
                          if f.kind is not FunctionKind.SMART_AWAY]
        if {f.alias for f in self.functions} != _manifest_aliases() or \
                len({(f.source_device, f.source_entity) for f in self.functions}) != 13 or \
                len({(f.device_id, f.zone_id) for f in zone_functions}) != 12 or \
                len({f.zone_id for f in zone_functions}) != 12 or \
                len({p.button_id for p in self.picos}) != len(self.picos):
            raise ValueError("incomplete or duplicate Caséta mapping")
        if any(not isinstance(name, str) or not SOURCE.fullmatch(name)
               for name in (self.ca_secret, self.cert_secret, self.key_secret)):
            raise ValueError("invalid scoped secret handle")


def _manifest_aliases() -> set[str]:
    manifest = json.loads((Path(__file__).resolve().parents[2] /
                           "scope/manifest.json").read_text())
    return {item["entity_id"] for item in manifest["entities"]
            if item.get("source") == "lutron_caseta" and
            item["entity_id"].split(".", 1)[0] in ("fan", "light", "switch")}


def load_settings(path: Path) -> Settings:
    if not path.is_absolute():
        raise ValueError("settings path must be absolute")
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size > 65536 or \
                info.st_mode & 0o077 or info.st_uid != os.getuid():
            raise ValueError("settings must be a private bounded owned regular file")
        with os.fdopen(fd, "rb", closefd=False) as source:
            contents = source.read(65537)
        if len(contents) > 65536:
            raise ValueError("settings exceeded size bound while reading")
    finally:
        os.close(fd)

    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate settings key")
            result[key] = value
        return result

    raw = json.loads(contents, object_pairs_hook=unique_object)
    if set(raw) != {"host", "port", "bridge_cert_sha256", "secrets", "functions",
                    "pico_events", "reconcile_seconds"}:
        raise ValueError("settings fields differ from the admitted schema")
    host = raw["host"]
    port = raw["port"]
    if not isinstance(host, str) or not 1 <= len(host) <= 253 or any(
            c in host for c in "/\\\x00 \t\r\n") or type(port) is not int or not 1 <= port <= 65535:
        raise ValueError("invalid bridge endpoint")
    pin_text = raw["bridge_cert_sha256"]
    if not isinstance(pin_text, str) or not re.fullmatch(r"[0-9a-f]{64}", pin_text):
        raise ValueError("bridge pin must be a pre-admitted SHA-256 leaf fingerprint")
    secrets = raw["secrets"]
    if not isinstance(secrets, dict) or set(secrets) != {"ca", "client_cert", "client_key"}:
        raise ValueError("CA, client cert and client key secret handles are required")
    if any(not isinstance(name, str) or not SOURCE.fullmatch(name) for name in secrets.values()):
        raise ValueError("invalid scoped secret name")
    entries = raw["functions"]
    manifest = _manifest_aliases()
    if len(manifest) != 13 or not isinstance(entries, dict) or set(entries) != manifest:
        raise ValueError("exactly the 13 manifest functions require explicit wire IDs")
    functions = []
    for alias in sorted(entries):
        item = entries[alias]
        kind = FunctionKind(alias.split(".", 1)[0])
        if not isinstance(item, dict):
            raise ValueError("invalid function mapping")
        expected = {"source_device", "source_entity", "device_id", "zone_id"}
        if alias == "switch.unassigned_smart_away":
            expected = {"source_device", "source_entity", "system_id"}
        if set(item) != expected:
            raise ValueError("missing or extra explicit function mapping")
        source_device, source_entity = item["source_device"], item["source_entity"]
        if not all(isinstance(v, str) and SOURCE.fullmatch(v)
                   for v in (source_device, source_entity)):
            raise ValueError("invalid source key")
        if alias == "switch.unassigned_smart_away":
            if item["system_id"] != "1":
                raise ValueError("pylutron Smart Away supports only system/away/1")
            device_id = zone_id = None
            kind = FunctionKind.SMART_AWAY
        else:
            device_id, zone_id = item["device_id"], item["zone_id"]
            if not all(isinstance(v, str) and WIRE_ID.fullmatch(v)
                       for v in (device_id, zone_id)):
                raise ValueError("invalid LEAP wire ID")
        functions.append(Function(alias, kind, device_id, zone_id,
                                  source_device, source_entity))
    picos = raw["pico_events"]
    if not isinstance(picos, list) or len(picos) > 16:
        raise ValueError("Pico event mapping limit")
    admitted_picos = []
    seen_buttons = set()
    for pico in picos:
        if not isinstance(pico, dict) or set(pico) != {"button_id", "binding_alias"} or \
                not isinstance(pico["button_id"], str) or not WIRE_ID.fullmatch(pico["button_id"]) or \
                pico["binding_alias"] not in manifest or pico["button_id"] in seen_buttons:
            raise ValueError("invalid Pico event mapping")
        seen_buttons.add(pico["button_id"])
        admitted_picos.append(Pico(pico["button_id"], pico["binding_alias"]))
    duration = raw["reconcile_seconds"]
    if type(duration) is not int or not 1 <= duration <= 3600:
        raise ValueError("invalid reconciliation interval")
    return Settings(host, port, bytes.fromhex(pin_text), secrets["ca"],
                    secrets["client_cert"], secrets["client_key"],
                    tuple(functions), tuple(admitted_picos), duration)
