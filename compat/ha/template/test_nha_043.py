"""Differential proof for the admitted NHA-043 subset against real HA Core."""

import hashlib
import json
import os
from pathlib import Path
import sys

import pytest

ROOT = Path(__file__).resolve().parents[3]
HERE = Path(__file__).resolve().parent
NHA006 = HERE.parent / "nha006"
sys.path.insert(0, str(NHA006))
import test_nha_006 as harness  # noqa: E402


def combined_input():
    baseline_bytes = (NHA006 / "cases.json").read_bytes()
    extra_bytes = (HERE / "cases_extra.json").read_bytes()
    worker_bytes = (HERE / "ha_worker.py").read_bytes()
    baseline = json.loads(baseline_bytes)
    extra = json.loads(extra_bytes)
    if len(baseline["cases"]) != 10 or len(extra["cases"]) != 43:
        raise ValueError("NHA-043 fixture count drift")
    baseline["cases"].extend(extra["cases"])
    baseline["variables"] = extra["variables"]
    ids = [case["id"] for case in baseline["cases"]]
    if len(ids) != len(set(ids)):
        raise ValueError("duplicate fixture ID")
    return baseline, worker_bytes, {
        "nha006_cases_sha256": hashlib.sha256(baseline_bytes).hexdigest(),
        "nha043_extra_sha256": hashlib.sha256(extra_bytes).hexdigest(),
        "nha043_worker_sha256": hashlib.sha256(worker_bytes).hexdigest(),
    }


def collect():
    fixtures, worker_bytes, hashes = combined_input()
    payload = json.dumps(fixtures, sort_keys=True, separators=(",", ":"))
    env = dict(os.environ)
    env["PATH"] = str(ROOT / ".tools/bin") + os.pathsep + env["PATH"]
    env["GOTOOLCHAIN"] = "local"
    go = json.loads(harness.run(
        ["go", "run", "./compat/ha/template/cmd/nha043-probe"],
        input_text=payload,
        env=env,
    ))
    module = json.loads(harness.run(
        ["go", "list", "-m", "-json", "github.com/nikolalohinski/gonja/v2"],
        env=env,
    ))
    go_version = harness.run(["go", "env", "GOVERSION"], env=env).strip()
    if (module["Path"], module["Version"], module["Sum"], go_version) != (
        "github.com/nikolalohinski/gonja/v2", "v2.9.1",
        "h1:ZDG0zYs5oR3fsqQFAlkaWiWYxPOBrCUK9k2IsRZhMa8=", "go1.25.2",
    ):
        raise RuntimeError("Gonja or Go pin drifted")
    reference, arch, context, endpoint = harness.local_image()
    ha = json.loads(harness.run_ha_worker(
        reference, (context, endpoint), worker_bytes.decode("utf-8"), payload,
    ))
    if ha["runtime"] != {
        "python": "3.14.6", "homeassistant": "2026.10.1", "jinja2": "3.1.6",
    }:
        raise RuntimeError("unexpected HA runtime")
    ids = [case["id"] for case in fixtures["cases"]]
    if [case["id"] for case in ha["cases"]] != ids or [case["id"] for case in go["cases"]] != ids:
        raise AssertionError("case order mismatch")
    return {
        "inputs": hashes,
        "reference": {
            "image_index": harness.IMAGE_INDEX,
            "image_manifest": harness.MANIFESTS[arch],
            "platform": f"linux/{arch}",
            "runtime": ha["runtime"],
        },
        "go_runtime": {
            "go": go_version,
            "gonja_module": module["Path"],
            "gonja_version": module["Version"],
            "gonja_sum": module["Sum"],
        },
        "ha": [{"id": row["id"], "raw": row["raw"]} for row in ha["cases"]],
        "go": go["cases"],
    }


def normalize_native(native):
    if native["type"] == "Wrapper" and native.get("category") == "list":
        return {"type": "list", "value": [normalize_native(item) for item in native["value"]]}
    if native["type"] == "list":
        assert native.get("category") == "list"
        return {"type": "list", "value": [normalize_native(item) for item in native["value"]]}
    kind = native["type"]
    value = native["value"]
    if kind == "none":
        assert value is None
    elif kind == "bool":
        assert type(value) is bool
    elif kind == "int":
        assert type(value) is int
    elif kind == "float":
        # Go's JSON encoder may spell 0.0 as 0. The tagged float is the
        # authority; accept either numeric lexical form, never a bool.
        assert type(value) in (int, float)
        value = float(value)
    elif kind == "str":
        assert type(value) is str
    else:
        raise AssertionError(f"unadmitted native fixture type {kind!r}")
    return {"type": kind, "value": value}


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


def check_reference(reference):
    platform = reference["platform"]
    assert platform.startswith("linux/")
    arch = platform.removeprefix("linux/")
    assert arch in harness.MANIFESTS
    assert reference["image_index"] == harness.IMAGE_INDEX
    assert reference["image_manifest"] == harness.MANIFESTS[arch]
    assert reference["runtime"] == {
        "python": "3.14.6", "homeassistant": "2026.10.1", "jinja2": "3.1.6",
    }


def assert_parity(observation):
    check_reference(observation["reference"])
    for ha_case, go_case in zip(observation["ha"], observation["go"]):
        assert ha_case["id"] == go_case["id"]
        assert ha_case["raw"]["text"] == go_case["raw"]["text"], ha_case["id"]
        for case in (ha_case, go_case):
            native = case["raw"]["native"]
            if native["type"] in ("Wrapper", "list"):
                assert native["render_result"] == case["raw"]["text"]
        ha_native = normalize_native(ha_case["raw"]["native"])
        go_native = normalize_native(go_case["raw"]["native"])
        assert canonical(ha_native) == canonical(go_native), (ha_case["id"], ha_native, go_native)


def test_nha_043_live_reference():
    observation = collect()
    assert_parity(observation)
    recorded = json.loads((HERE / "observed.json").read_text())
    check_reference(recorded["reference"])
    assert canonical(observation["inputs"]) == canonical(recorded["inputs"])
    assert canonical(observation["go_runtime"]) == canonical(recorded["go_runtime"])
    assert canonical(observation["reference"]["runtime"]) == canonical(recorded["reference"]["runtime"])
    for key in ("ha", "go"):
        live_cases = observation[key]
        recorded_cases = recorded[key]
        assert [row["id"] for row in live_cases] == [row["id"] for row in recorded_cases]
        for live, saved in zip(live_cases, recorded_cases):
            assert live["raw"]["text"] == saved["raw"]["text"]
            assert canonical(normalize_native(live["raw"]["native"])) == canonical(normalize_native(saved["raw"]["native"]))


def test_nha_043_type_comparator_rejects_corruption():
    integer = {"type": "int", "value": 0}
    floating = {"type": "float", "value": 0.0}
    boolean = {"type": "bool", "value": True}
    one = {"type": "int", "value": 1}
    assert canonical(normalize_native(integer)) != canonical(normalize_native(floating))
    assert canonical(normalize_native(boolean)) != canonical(normalize_native(one))
    assert canonical(normalize_native({"type": "float", "value": 0})) == canonical(normalize_native(floating))
    with pytest.raises(AssertionError):
        normalize_native({"type": "int", "value": True})
    with pytest.raises(AssertionError):
        normalize_native({"type": "int", "value": 0.0})
    with pytest.raises(AssertionError):
        normalize_native({"type": "bool", "value": 1})


if __name__ == "__main__":
    if sys.argv[1:] != ["--capture"]:
        raise SystemExit("usage: python test_nha_043.py --capture")
    observed = collect()
    assert_parity(observed)
    (HERE / "observed.json").write_text(json.dumps(observed, indent=2, sort_keys=True) + "\n")
