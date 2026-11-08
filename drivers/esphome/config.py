"""Explicit, private per-device Native API configuration; no house catalogue."""
import dataclasses
import enum
import json
import os
import re
import stat
from pathlib import Path

ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9:._/-]{0,127}\Z")
MAC = re.compile(r"(?:[0-9a-f]{2}:){5}[0-9a-f]{2}\Z")


class Kind(str, enum.Enum):
    ALARM = "alarm"
    COVER = "cover"
    LOCK = "lock"


class Transport(str, enum.Enum):
    NOISE = "noise"
    PLAINTEXT = "plaintext"


class CodeSource(str, enum.Enum):
    NONE = "none"
    SERVER = "server"
    USER = "user"


class Operation(str, enum.Enum):
    ARM_HOME = "arm_home"
    ARM_AWAY = "arm_away"
    ARM_NIGHT = "arm_night"
    DISARM = "disarm"
    OPEN = "open"
    CLOSE = "close"
    STOP = "stop"
    SET_POSITION = "set_position"
    LOCK = "lock"
    UNLOCK = "unlock"


OPERATIONS = {Kind.ALARM: frozenset((Operation.ARM_HOME, Operation.ARM_AWAY, Operation.ARM_NIGHT, Operation.DISARM)),
              Kind.COVER: frozenset((Operation.OPEN, Operation.CLOSE, Operation.STOP, Operation.SET_POSITION)),
              Kind.LOCK: frozenset((Operation.LOCK, Operation.UNLOCK))}


@dataclasses.dataclass(frozen=True)
class Function:
    kind: Kind
    key: int
    object_id: str
    source_entity: str
    operations: tuple[Operation, ...]
    code_secret: str | None = None
    code_source: CodeSource = CodeSource.NONE

    def __post_init__(self):
        if type(self.kind) is not Kind or type(self.code_source) is not CodeSource or \
                (self.code_source is CodeSource.SERVER) != (self.code_secret is not None) or \
                self.kind is not Kind.ALARM and self.code_source is not CodeSource.NONE or type(self.key) is not int or not 0 < self.key < 2**32 or \
                not all(isinstance(v, str) and ID.fullmatch(v) for v in (self.object_id, self.source_entity)) or \
                type(self.operations) is not tuple or any(type(op) is not Operation for op in self.operations) or len(set(self.operations)) != len(self.operations) or \
                not set(self.operations) <= OPERATIONS[self.kind] or \
                self.code_secret is not None and (self.kind is not Kind.ALARM or not isinstance(self.code_secret, str) or not ID.fullmatch(self.code_secret)):
            raise ValueError("invalid ESPHome function mapping")


@dataclasses.dataclass(frozen=True)
class Settings:
    host: str
    port: int
    expected_mac: str
    source_device: str
    transport: Transport
    noise_secret: str | None
    password_secret: str | None
    functions: tuple[Function, ...]

    def __post_init__(self):
        if not isinstance(self.host, str) or not 1 <= len(self.host) <= 253 or \
                any(c in self.host for c in "/\\\x00 \t\r\n") or \
                type(self.port) is not int or not 1 <= self.port <= 65535 or \
                not isinstance(self.expected_mac, str) or not MAC.fullmatch(self.expected_mac) or not isinstance(self.source_device, str) or not ID.fullmatch(self.source_device) or \
                type(self.transport) is not Transport or \
                (self.transport is Transport.NOISE) != (self.noise_secret is not None) or \
                any(v is not None and (not isinstance(v, str) or not ID.fullmatch(v))
                    for v in (self.noise_secret, self.password_secret)) or \
                type(self.functions) is not tuple or not 1 <= len(self.functions) <= 32 or \
                any(type(f) is not Function for f in self.functions) or \
                len({f.key for f in self.functions}) != len(self.functions) or \
                len({f.source_entity for f in self.functions}) != len(self.functions):
            raise ValueError("invalid ESPHome device settings")


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
            raise ValueError("settings exceeded size bound")
    finally:
        os.close(fd)

    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate settings field")
            result[key] = value
        return result

    raw = json.loads(contents, object_pairs_hook=unique)
    if set(raw) != {"host", "port", "expected_mac", "source_device", "transport", "noise_secret", "password_secret", "functions"}:
        raise ValueError("unexpected settings fields")
    functions = []
    for item in raw.pop("functions"):
        if set(item) != {"kind", "key", "object_id", "source_entity", "operations", "code_secret", "code_source"} or type(item["operations"]) is not list:
            raise ValueError("unexpected function fields")
        functions.append(Function(**{**item, "kind": Kind(item["kind"]), "code_source": CodeSource(item["code_source"]), "operations": tuple(Operation(v) for v in item["operations"])}))
    return Settings(**{**raw, "transport": Transport(raw["transport"])}, functions=tuple(functions))
