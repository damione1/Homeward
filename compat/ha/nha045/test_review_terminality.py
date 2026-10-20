"""A daemon error after rm is not proof of absence; no real Docker calls."""

from capture import CommandResult, load_json
from test_review_cleanup import SimulatedDaemonRunner


def test_nha_045_cleanup_inspect_daemon_error_stays_nonterminal(tmp_path):
    class UnavailableAfterRemoval(SimulatedDaemonRunner):
        removed = False

        def docker(self, arguments, **kwargs):
            if self.removed and arguments[:2] == ["container", "inspect"]:
                self.calls.append(list(arguments))
                return CommandResult(b"Cannot connect to the Docker daemon", 1)
            result = super().docker(arguments, **kwargs)
            if arguments[:3] == ["rm", "-f", "-v"]:
                self.removed = True
            return result

    runner = UnavailableAfterRemoval(tmp_path)
    try:
        runner.isolated("uncertain-cleanup")
    except Exception:
        # The audit must retain uncertainty whether cleanup propagates its
        # error or attaches it to a primary worker failure.
        pass
    assert runner.removed, "fixture did not reach successful removal"
    assert runner.calls[-1][:2] == ["container", "inspect"]
    records = load_json((tmp_path / "containers.json").read_bytes())
    assert len(records) == 1
    assert records[0]["terminal"] is False, "daemon failure was certified as absence"
