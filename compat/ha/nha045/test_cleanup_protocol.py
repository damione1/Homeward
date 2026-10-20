"""Pure checks of the observed pinned Docker absence protocol and ownership."""

import subprocess
import time

import pytest

from capture import CommandResult, QualificationFailure, Runner, confirms_container_absence, load_json
from test_review_cleanup import SimulatedDaemonRunner


def absent_reply(identity):
    return CommandResult(b"[]\n", 1,
                         ("Error response from daemon: No such container: " + identity + "\n").encode("ascii"))


def test_nha_045_exact_absence_protocol_requires_separate_streams_and_exact_object():
    identities = ("homeward-absence-protocol-188-20261019-2252", "nha045-" + "1" * 32, "a" * 64)
    for identity in identities:
        valid = absent_reply(identity)
        assert confirms_container_absence(valid, identity) is True
        for invalid in (
            CommandResult(b"", 1),
            CommandResult(b"[]\n", 1),
            CommandResult(b"[]", 1, valid.stderr),
            CommandResult(b"[]\n{}\n", 1, valid.stderr),
            CommandResult(valid.stderr, 1),
            CommandResult(b"[]\n", 1, b"Cannot connect to the Docker daemon\n"),
            CommandResult(b"[]\n", 1, valid.stderr + b"additional error\n"),
            CommandResult(b"[]\n", 1, valid.stderr.rstrip(b"\n")),
            CommandResult(b"[]\n", 1, absent_reply("another-object").stderr),
            CommandResult(b"[]\n", 0, valid.stderr),
            CommandResult(b"[]\n", 2, valid.stderr),
            CommandResult(b"[]\n", -9, valid.stderr),
            CommandResult(b"[]\n", True, valid.stderr),
            CommandResult("[]\n", 1, valid.stderr),
            CommandResult(b"[]\n", 1, valid.stderr.decode()),
        ):
            assert confirms_container_absence(invalid, identity) is False
    for invalid_identity in (None, "", "x\n", "x/foreign", "é", "x" * 129):
        assert confirms_container_absence(absent_reply("safe"), invalid_identity) is False


def test_nha_045_normal_cleanup_proves_both_owned_id_and_name(tmp_path):
    runner = SimulatedDaemonRunner(tmp_path)
    runner.isolated("normal-protocol")
    record = load_json((tmp_path / "containers.json").read_bytes())[-1]
    assert record["terminal"] is True
    assert record["id_absence_confirmed"] is True and record["name_absence_confirmed"] is True
    assert runner.created is None


def test_nha_045_initial_inspect_error_refuses_before_create(tmp_path):
    class InitialInspectUnavailable(SimulatedDaemonRunner):
        def docker(self, arguments, **kwargs):
            if list(arguments[:2]) == ["container", "inspect"]:
                self.calls.append(list(arguments))
                return CommandResult(b"[]\n", 1, b"Cannot connect to the Docker daemon\n")
            return super().docker(arguments, **kwargs)

    runner = InitialInspectUnavailable(tmp_path)
    with pytest.raises(QualificationFailure, match="candidate container name absence not proven"):
        runner.isolated("initial-inspect-unavailable")
    assert runner.containers == [] and runner.created is None
    assert not any(call[0] in ("create", "start", "rm") for call in runner.calls)


@pytest.mark.parametrize("fault", ["wrong_id", "wrong_name", "daemon_error", "stdout_only"])
def test_nha_045_cleanup_rejects_wrong_object_or_ambiguous_protocol(tmp_path, fault):
    class ProtocolFault(SimulatedDaemonRunner):
        removed = False

        def docker(self, arguments, **kwargs):
            if self.removed and list(arguments[:2]) == ["container", "inspect"]:
                target = arguments[2]
                applies = (fault in ("daemon_error", "stdout_only")
                           or fault == "wrong_id" and target == self.identity
                           or fault == "wrong_name" and target != self.identity)
                if applies:
                    self.calls.append(list(arguments))
                    if fault == "daemon_error":
                        return CommandResult(b"[]\n", 1, b"Cannot connect to the Docker daemon\n")
                    if fault == "stdout_only":
                        reply = absent_reply(target)
                        return CommandResult(reply.stdout + reply.stderr, 1)
                    return absent_reply("another-object")
            result = super().docker(arguments, **kwargs)
            if list(arguments[:3]) == ["rm", "-f", "-v"]:
                self.removed = True
            return result

    runner = ProtocolFault(tmp_path)
    with pytest.raises(QualificationFailure, match="absence not proven"):
        runner.isolated("bad-absence-protocol")
    record = load_json((tmp_path / "containers.json").read_bytes())[-1]
    assert record["terminal"] is False and record.get("cleanup_error")
    assert sum(call[0] == "rm" for call in runner.calls) == 1
    assert [call for call in runner.calls if call[0] == "rm"][-1][-1] == runner.identity
    if fault == "wrong_name":
        assert record["id_absence_confirmed"] is True and record["name_absence_confirmed"] is False


def test_nha_045_uncertain_create_without_effect_requires_exact_owned_name_absence(tmp_path):
    class NoCreation(SimulatedDaemonRunner):
        def docker(self, arguments, **kwargs):
            if arguments[0] == "create":
                self.calls.append(list(arguments))
                raise QualificationFailure("synthetic create timeout without daemon creation")
            return super().docker(arguments, **kwargs)

    runner = NoCreation(tmp_path)
    with pytest.raises(QualificationFailure, match="create timeout"):
        runner.isolated("no-creation")
    record = load_json((tmp_path / "containers.json").read_bytes())[-1]
    assert record["terminal"] is True and record["creation_absent"] is True
    assert record["name_absence_confirmed"] is True and record["id_absence_confirmed"] is False
    assert not any(call[0] == "rm" for call in runner.calls)


def test_nha_045_primary_create_error_survives_unproven_post_rm_absence(tmp_path):
    class PrimaryAndUncertainCleanup(SimulatedDaemonRunner):
        removed = False

        def docker(self, arguments, **kwargs):
            if self.removed and list(arguments[:2]) == ["container", "inspect"]:
                self.calls.append(list(arguments))
                return CommandResult(b"[]\n", 1, b"Cannot connect to the Docker daemon\n")
            result = super().docker(arguments, **kwargs)
            if list(arguments[:3]) == ["rm", "-f", "-v"]:
                self.removed = True
            return result

    runner = PrimaryAndUncertainCleanup(tmp_path, create_fault="timeout")
    with pytest.raises(QualificationFailure, match="create timeout"):
        runner.isolated("primary-and-cleanup")
    record = load_json((tmp_path / "containers.json").read_bytes())[-1]
    assert runner.removed and record["terminal"] is False
    assert "create timeout" in record["primary_error"] and "absence not proven" in record["cleanup_error"]


@pytest.mark.parametrize("stdout,stderr,poll_overflow", [(b"abcde", b"xyz", False),
                                                       (b"abcdef", b"xyz", False),
                                                       (b"abcde", b"wxyz", True)])
def test_nha_045_command_streams_share_one_output_budget(tmp_path, monkeypatch, stdout, stderr, poll_overflow):
    processes = []

    class PureProcess:
        pid = 987654321
        returncode = 0
        terminated = False
        written = False

        def __init__(self, argv, **kwargs):
            assert kwargs["start_new_session"] is True
            self.stdout_file, self.stderr_file = kwargs["stdout"], kwargs["stderr"]
            processes.append(self)

        def communicate(self, input=None, timeout=None):
            if not self.written:
                self.stdout_file.write(stdout)
                self.stderr_file.write(stderr)
                self.written = True
                if poll_overflow:
                    raise subprocess.TimeoutExpired("synthetic-only", timeout)
            return None, None

        def terminate(self):
            self.terminated = True

        def wait(self, timeout=None):
            return self.returncode

    def absent_group(pid, signal):
        assert pid == PureProcess.pid and signal == 0
        raise ProcessLookupError

    monkeypatch.setattr("capture.subprocess.Popen", PureProcess)
    monkeypatch.setattr("capture.os.killpg", absent_group)
    monkeypatch.setattr("capture.MAX_OUTPUT", 8)
    runner = Runner.__new__(Runner)
    runner.evidence = tmp_path
    runner.lease = "ROOT045PUREBUDGET"
    runner.deadline = time.monotonic() + 120
    runner.counter, runner.commands = 0, []
    runner.socket_check = lambda: None
    if len(stdout) + len(stderr) > 8:
        with pytest.raises(QualificationFailure, match="capture_overflow"):
            runner.command(["synthetic-only"])
    else:
        result = runner.command(["synthetic-only"])
        assert result.stdout == stdout and result.stderr == stderr and result.code == 0
    assert len(processes) == 1 and runner.commands[-1]["leader_joined"] is True
    assert runner.commands[-1]["process_group_absent"] is True
    if poll_overflow:
        assert processes[0].terminated is True and runner.commands[-1]["overflow"] is True
