"""Verify the NHA-176 observation against the pinned, isolated HA helper."""

import hashlib
import json
from pathlib import Path
import sys

HERE = Path(__file__).resolve().parent
NHA006 = HERE.parent / "nha006"
sys.path.insert(0, str(NHA006))
import test_nha_006 as harness  # noqa: E402


def test_nha_176_live_reference():
    fixtures = (HERE / "cases_defined.json").read_bytes()
    worker = (HERE / "ha_defined_worker.py").read_bytes()
    recorded = json.loads((HERE / "observed_defined.json").read_bytes())
    reference = recorded["reference"]
    assert len(json.loads(fixtures)["cases"]) == 31
    assert hashlib.sha256(fixtures).hexdigest() == reference["fixtures_sha256"]
    assert hashlib.sha256(worker).hexdigest() == reference["worker_sha256"]
    image, arch, context, endpoint = harness.local_image()
    assert reference["image_index"] == harness.IMAGE_INDEX
    assert reference["image_manifest"] == harness.MANIFESTS[arch]
    assert reference["platform"] == f"linux/{arch}"
    observed = json.loads(harness.run_ha_worker(
        image, (context, endpoint), worker.decode(), fixtures.decode(),
    ))
    assert observed["runtime"] == reference["runtime"] == {
        "python": "3.14.6", "homeassistant": "2026.10.1", "jinja2": "3.1.6",
    }
    assert observed["cases"] == recorded["cases"]
    assert sum(row["ha_text"]["status"] == "error" for row in observed["cases"]) == 7
    for row in observed["cases"]:
        text, native, jinja = row["ha_text"], row["ha_native"], row["jinja_text"]
        assert text["status"] == native["status"] == jinja["status"]
        if text["status"] == "ok":
            assert text["value"] == jinja["value"]
        else:
            assert text["cause"]["error"] == jinja["error"]
            assert text["cause"]["message"] == jinja["message"]


def test_nha_176_attribute_profile_edges():
    """Keep unsupported Python attributes distinct from a false test result."""
    worker = (HERE / "ha_defined_worker.py").read_text()
    assert worker.count("import jinja2\n") == 1
    assert worker.count('variables = case["variables"]') == 1
    worker = worker.replace(
        "import jinja2\n", "import jinja2\nfrom datetime import timedelta\n",
    ).replace(
        'variables = case["variables"]',
        'variables = dict(case["variables"])\n'
        '        if case.get("timedelta"):\n'
        '            variables["duration"] = timedelta(days=2)',
    )
    cases = [
        {"id": "duration-days", "template": "{{ duration.days is defined }}", "variables": {}, "timedelta": True},
        {"id": "mapping-items", "template": "{{ obj.items is defined }}", "variables": {"obj": {}}},
        {"id": "mapping-items-key", "template": "{{ obj.items is defined }}", "variables": {"obj": {"items": 0}}},
        {"id": "mapping-items-dot-chain", "template": "{{ obj.items.idx is defined }}", "variables": {"obj": {"items": {"idx": 1}}}},
        {"id": "mapping-items-item-chain", "template": "{{ obj['items'].idx is defined }}", "variables": {"obj": {"items": {"idx": 1}}}},
        {"id": "scalar-real", "template": "{{ scalar.real is defined }}", "variables": {"scalar": 0}},
        {"id": "scalar-absent", "template": "{{ scalar.missing is defined }}", "variables": {"scalar": 0}},
        {"id": "string-upper", "template": "{{ scalar.upper is defined }}", "variables": {"scalar": "text"}},
    ]
    image, arch, context, endpoint = harness.local_image()
    assert arch == "arm64"  # The recorded profile observation uses this pinned manifest.
    assert harness.MANIFESTS[arch] == "sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7"
    observed = json.loads(harness.run_ha_worker(
        image, (context, endpoint), worker,
        json.dumps({"cases": cases}, sort_keys=True, separators=(",", ":")),
    ))
    assert observed["runtime"] == {
        "python": "3.14.6", "homeassistant": "2026.10.1", "jinja2": "3.1.6",
    }
    assert [row["id"] for row in observed["cases"]] == [case["id"] for case in cases]
    assert [row["ha_native"]["value"] for row in observed["cases"]] == [
        True, True, True, False, True, True, False, True,
    ]
    for row in observed["cases"]:
        assert row["ha_text"]["status"] == row["ha_native"]["status"] == row["jinja_text"]["status"] == "ok"
        assert row["ha_text"]["value"] == row["jinja_text"]["value"]
