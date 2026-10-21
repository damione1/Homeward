"""A failed recovery inspect cannot certify that uncertain creation was absent."""

import pytest

from dataclasses import replace

from capture import CommandResult, QualificationFailure, admit_profile, load_json
from test_review_cleanup import SimulatedDaemonRunner


def test_nha_045_uncertain_recovery_inspect_stays_nonterminal(tmp_path):
    class UnavailableRecovery(SimulatedDaemonRunner):
        def docker(self, arguments, **kwargs):
            if list(arguments[:2]) == ["container", "inspect"] and self.created is not None:
                self.calls.append([str(value) for value in arguments])
                return CommandResult(b"", 1)
            return super().docker(arguments, **kwargs)

    runner = UnavailableRecovery(tmp_path, create_fault="timeout")
    with pytest.raises(QualificationFailure, match="create timeout"):
        runner.isolated("unavailable-recovery")
    audit = load_json((tmp_path / "containers.json").read_bytes())[-1]
    assert audit["terminal"] is False and audit["recovery_unknown"] is True
    assert "recovery" in audit["cleanup_error"]
    assert "create timeout" in audit["primary_error"]
    assert runner.created is not None and not any(call[0] == "rm" for call in runner.calls)


@pytest.mark.parametrize("name", ["nha045", "nha047"])
def test_nha047_closed_profile_preserves_uncertain_create_ownership(tmp_path, name):
    runner = SimulatedDaemonRunner(tmp_path, create_fault="timeout")
    runner.profile = admit_profile(name)
    with pytest.raises(QualificationFailure, match="create timeout"):
        runner.isolated("profile-owned")
    assert runner.created is None
    assert ["rm", "-f", "-v", runner.identity] in runner.calls
    assert not any(call[0] == "start" for call in runner.calls)
    audit = load_json((tmp_path / "containers.json").read_bytes())[-1]
    assert audit["terminal"] is True


def test_nha047_profile_cannot_replace_worker_with_a_caller_path(tmp_path):
    runner = SimulatedDaemonRunner(tmp_path)
    runner.profile = replace(admit_profile("nha047"), worker=tmp_path / "arbitrary.py")
    with pytest.raises(QualificationFailure, match="profile"):
        runner.isolated("forbidden-worker", python=True)
    assert runner.calls == []
