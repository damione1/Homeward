"""Evidence checks never create/start Docker or silently collect missing data."""

from copy import deepcopy
from pathlib import Path

import pytest

from capture import IMAGE, MEMORY, QualificationFailure, docker_size, load_json, validate_envelope


def admitted_envelope():
    return {"Image": "image-id", "Config": {"Image": IMAGE, "Entrypoint": ["/nha045-probe"]},
            "State": {"Status": "created", "Running": False},
            "HostConfig": {"NetworkMode": "none", "PublishAllPorts": False, "PortBindings": {},
                           "Privileged": False, "ReadonlyRootfs": True, "CapDrop": ["ALL"], "CapAdd": [],
                           "SecurityOpt": ["no-new-privileges"], "PidsLimit": 128, "Memory": MEMORY,
                           "MemorySwap": MEMORY, "Devices": [], "DeviceRequests": [], "ExtraHosts": [],
                           "PidMode": "", "UTSMode": "", "IpcMode": "private",
                           "Tmpfs": {"/tmp": "rw,nosuid,nodev,size=67108864"}},
            "NetworkSettings": {"Networks": {"none": {"IPAddress": "", "Gateway": ""}}},
            "Mounts": [{"Type": "bind", "Source": "/tmp/probe", "Destination": "/nha045-probe", "RW": False}]}


def test_nha_045_effective_envelope_refuses_egress_and_host_access():
    original = admitted_envelope()
    validate_envelope(original, image_id="image-id", probe=Path("/tmp/probe"), python=False)
    mutations = [
        lambda value: value["HostConfig"].update(NetworkMode="host"),
        lambda value: value["HostConfig"].update(Privileged=True),
        lambda value: value["HostConfig"].update(ReadonlyRootfs=False),
        lambda value: value["HostConfig"].update(CapDrop=[]),
        lambda value: value["HostConfig"].update(SecurityOpt=[]),
        lambda value: value["HostConfig"].update(PidsLimit=0),
        lambda value: value["HostConfig"].update(Memory=0),
        lambda value: value["HostConfig"].update(PortBindings={"8123/tcp": [{"HostPort": "8123"}]}),
        lambda value: value["Mounts"][0].update(Source="/Users/damien"),
        lambda value: value["Mounts"][0].update(RW=True),
        lambda value: value["State"].update(Running=True),
        lambda value: value["NetworkSettings"]["Networks"].update(bridge={"IPAddress": "192.0.2.1"}),
    ]
    for mutate in mutations:
        value = deepcopy(original)
        mutate(value)
        with pytest.raises(QualificationFailure):
            validate_envelope(value, image_id="image-id", probe=Path("/tmp/probe"), python=False)
    with pytest.raises(QualificationFailure):
        validate_envelope(original, image_id="image-id", probe=Path("/tmp/probe"), python=True)


def test_nha_045_driver_rejects_ambiguous_measurements_and_json():
    assert docker_size("1.25GB") == 1250000000
    assert docker_size("1GiB") == 1 << 30
    for value in ("unknown", "-1GB", "1PB"):
        with pytest.raises(QualificationFailure):
            docker_size(value)
    with pytest.raises(QualificationFailure):
        load_json('{"schema":"a","schema":"b"}')


def test_nha_045_live_evidence_is_complete(request):
    directory = request.config.getoption("--capture-dir")
    if directory is None:
        pytest.fail("--capture-dir is required; no NHA-045 live run is implied or collected")
    result = load_json((directory / "result.json").read_bytes())
    assert result["status"] == "passed" and result["cases"] == 10
    assert result["mutant_failed"] is True and result["mutant_replay_equal"] is True
    assert result["containers_terminal"] is True
    corpus = load_json((directory / "corpus.json").read_bytes())
    assert len(corpus) == len({row["id"] for row in corpus}) == 10
    for row in corpus:
        comparison = load_json((directory / (row["id"] + "-comparison.json")).read_bytes())
        assert comparison["equivalent"] is True and comparison["differences"] == []
        ha = load_json((directory / (row["id"] + ".ha.json")).read_bytes())
        go = load_json((directory / (row["id"] + ".go.json")).read_bytes())
        assert ha["fixture_sha256"] == go["fixture_sha256"] == row["sha256"]
        for capture in (ha, go):
            assert capture["terminal"] is True and capture["evicted"] == 0
            assert capture["raw"]["network"]["loopback_only"] is True
            assert capture["raw"]["cleanup"] and all(capture["raw"]["cleanup"].values())
    mutant = load_json((directory / "mutant-comparison.json").read_bytes())
    replay = load_json((directory / "mutant-replay-comparison.json").read_bytes())
    assert mutant == replay and mutant["equivalent"] is False
    assert any(row["field"] == "fact" for row in mutant["differences"])
    containers = load_json((directory / "containers.json").read_bytes())
    assert containers and all(row["terminal"] is True for row in containers)
