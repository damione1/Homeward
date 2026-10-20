"""A failed recovery inspect cannot certify that uncertain creation was absent."""

import pytest

from capture import CommandResult, QualificationFailure, load_json
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
