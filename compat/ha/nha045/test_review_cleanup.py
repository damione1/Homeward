"""Pure daemon simulation: no subprocess, socket, Docker or HA is invoked."""

from copy import deepcopy
from hashlib import sha256
from types import SimpleNamespace

import pytest

from capture import CommandResult, QualificationFailure, Runner, encode, load_json
from test_nha_045 import admitted_envelope


class SimulatedDaemonRunner(Runner):
    def __init__(self, directory, *, create_fault=None, cleanup_fails=False, foreign_lease=False):
        self.evidence = directory
        self.probe = directory / "probe"
        self.probe.write_bytes(b"synthetic-probe-only")
        self.probe_hash = sha256(self.probe.read_bytes()).hexdigest()
        self.image_id = "image-id"
        self.lease = "ROOT045PUREOWNERSHIP"
        self.containers = []
        self.calls = []
        self.created = None
        self.identity = "a" * 64
        self.create_fault = create_fault
        self.cleanup_fails = cleanup_fails
        self.foreign_lease = foreign_lease

    def budget(self):
        return None

    def docker(self, arguments, **kwargs):
        arguments = [str(value) for value in arguments]
        self.calls.append(arguments)
        if arguments[0] == "create":
            name = arguments[arguments.index("--name") + 1]
            value = deepcopy(admitted_envelope())
            value["Id"] = self.identity
            value["Name"] = "/" + name
            value["Mounts"][0]["Source"] = str(self.probe)
            value["Config"]["Labels"] = {"homeward.nha045.lease": "foreign-lease" if self.foreign_lease else self.lease}
            self.created = value
            if self.create_fault == "timeout":
                raise QualificationFailure("synthetic create timeout after daemon creation")
            if self.create_fault == "invalid_identity":
                return CommandResult(b"not-a-container-id\n", 0)
            return CommandResult((self.identity + "\n").encode(), 0)
        if arguments[:2] == ["container", "inspect"]:
            if self.created is None or arguments[2] not in (self.identity, self.created["Name"].removeprefix("/")):
                return CommandResult(b"[]\n", 1,
                                     ("Error response from daemon: No such container: " + arguments[2] + "\n").encode())
            return CommandResult(encode([self.created]), 0)
        if arguments[0] == "start":
            assert arguments[-1] == self.identity
            self.created["State"] = {"Status": "exited", "Running": False, "ExitCode": 0}
            return CommandResult(b"{}", 0)
        if arguments[:3] == ["rm", "-f", "-v"]:
            assert arguments[-1] == self.identity, "cleanup targeted an unadmitted identity"
            if self.cleanup_fails:
                raise QualificationFailure("synthetic cleanup failure")
            self.created = None
            return CommandResult(b"", 0)
        raise AssertionError("unexpected simulated daemon command: " + repr(arguments))


@pytest.mark.parametrize("fault", ["timeout", "invalid_identity"])
def test_nha_045_uncertain_create_recovers_owned_name_before_cleanup(tmp_path, monkeypatch, fault):
    monkeypatch.setattr("capture.uuid.uuid4", lambda: SimpleNamespace(hex="1" * 32))
    runner = SimulatedDaemonRunner(tmp_path, create_fault=fault)
    with pytest.raises(QualificationFailure):
        runner.isolated("uncertain-create")
    assert runner.created is None, "daemon-created owned container survived the uncertain CLI result"
    assert not any(call[0] == "start" for call in runner.calls)
    assert ["rm", "-f", "-v", runner.identity] in runner.calls
    audit = load_json((tmp_path / "containers.json").read_bytes())
    assert audit[-1]["id"] == runner.identity and audit[-1]["terminal"] is True


def test_nha_045_uncertain_create_never_removes_foreign_lease(tmp_path, monkeypatch):
    monkeypatch.setattr("capture.uuid.uuid4", lambda: SimpleNamespace(hex="2" * 32))
    runner = SimulatedDaemonRunner(tmp_path, create_fault="timeout", foreign_lease=True)
    with pytest.raises(QualificationFailure, match="create timeout"):
        runner.isolated("foreign-lease")
    assert runner.created is not None
    assert not any(call[0] in ("start", "rm") for call in runner.calls)
    name = runner.created["Name"].removeprefix("/")
    assert sum(call == ["container", "inspect", name] for call in runner.calls) >= 2
    audit = load_json((tmp_path / "containers.json").read_bytes())
    assert audit[-1]["terminal"] is False and audit[-1].get("cleanup_error")


def test_nha_045_cleanup_failure_audit_persists_when_primary(tmp_path):
    runner = SimulatedDaemonRunner(tmp_path, cleanup_fails=True)
    with pytest.raises(QualificationFailure, match="cleanup failure"):
        runner.isolated("cleanup-primary")
    audit_path = tmp_path / "containers.json"
    assert audit_path.is_file(), "primary cleanup failure skipped the owned-container audit"
    audit = load_json(audit_path.read_bytes())
    assert audit[-1]["id"] == runner.identity and audit[-1]["terminal"] is False
    assert "cleanup failure" in audit[-1]["cleanup_error"]


def test_nha_045_uncertain_create_preserves_primary_and_cleanup_failure(tmp_path):
    runner = SimulatedDaemonRunner(tmp_path, create_fault="timeout", cleanup_fails=True)
    with pytest.raises(QualificationFailure, match="create timeout"):
        runner.isolated("create-and-cleanup-failure")
    audit = load_json((tmp_path / "containers.json").read_bytes())
    assert audit[-1]["id"] == runner.identity and audit[-1]["terminal"] is False
    assert "cleanup failure" in audit[-1]["cleanup_error"]
