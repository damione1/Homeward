"""Fixed profile oracles. No daemon, network, Go build or HA worker is invoked."""

from dataclasses import FrozenInstanceError
from pathlib import Path

import pytest

import capture


def test_nha047_profile_keeps_045_default_and_closed_worker_paths():
    default = capture.admit_profile("nha045")
    profile = capture.admit_profile("nha047")
    assert default.name == "nha045" and default.schema == "nha045/v1"
    assert default.worker == capture.HERE / "ha_worker.py"
    assert default.expected_count == 10
    assert profile.name == "nha047" and profile.schema == "nha047/v1"
    assert profile.worker == capture.HERE.parent / "nha047" / "ha_worker.py"
    assert profile.expected_count is None
    assert profile.source_roots == (capture.HERE, capture.HERE.parent / "nha047",
                                    capture.HERE.parent / "report")
    with pytest.raises(FrozenInstanceError):
        profile.worker = Path("/tmp/arbitrary-worker.py")


@pytest.mark.parametrize("name", ["", "NHA047", "nha048", "../nha047", None,
                                  {"name": "nha047", "network": "host"}])
def test_nha047_unknown_profile_is_rejected_before_probe_or_filesystem(name):
    with pytest.raises(capture.QualificationFailure, match="profile"):
        capture.admit_profile(name)
    runner = capture.Runner.__new__(capture.Runner)
    with pytest.raises(capture.QualificationFailure, match="profile"):
        runner.__init__(object(), object(), profile=name)


def contract():
    return {"schema": "nha047/v1", "image": capture.IMAGE,
            "ha_version": "2026.10.1", "go_version": "go1.27.2",
            "constructions": [{"value": 1, "name": "automation_single"}],
            "cases": [{"id": "time-18-true", "sha256": "a" * 64}],
            "demands": [{"name": "ClimateTemperature", "token": "climate_temperature"}],
            "python": "3.14.6", "jinja": "3.1.6",
            "sources": [{"path": "homeassistant.core", "sha256": "a" * 64}]}


def test_nha047_go_manifest_is_the_only_case_authority():
    profile = capture.admit_profile("nha047")
    result = capture.admit_profile_contract(profile, contract())
    assert result == {"time-18-true": "a" * 64}


@pytest.mark.parametrize("mutation", [
    lambda value: value.update(schema="nha045/v1"),
    lambda value: value.update(image="arbitrary-image"),
    lambda value: value.update(ha_version="latest"),
    lambda value: value.update(go_version="go1.25.2"),
    lambda value: value.update(cases=[]),
    lambda value: value.update(cases=value["cases"] * 2),
    lambda value: value["cases"][0].update(sha256="not-a-digest"),
    lambda value: value["cases"][0].update(id="../escape"),
    lambda value: value.update(worker="/tmp/arbitrary-worker.py"),
    lambda value: value.update(network="host"),
])
def test_nha047_manifest_cannot_open_an_execution_boundary(mutation):
    value = contract()
    mutation(value)
    with pytest.raises(capture.QualificationFailure):
        capture.admit_profile_contract(capture.admit_profile("nha047"), value)
