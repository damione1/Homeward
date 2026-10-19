"""Capture three missing-source legacy formulas from local, pinned HA Core.

Run explicitly with --capture via the existing NHA-006 local-image harness.
This script never downloads an image and never connects to Home Assistant.
"""

import argparse
import hashlib
import json
from pathlib import Path
import sys

HERE = Path(__file__).resolve().parent
NHA006 = HERE.parent / "nha006"
sys.path.insert(0, str(NHA006))
import test_nha_006 as harness  # noqa: E402

EXPECTED_IDS = ["ups-legacy", "floor-legacy", "hvac-legacy"]
EXPECTED_RUNTIME = {
    "python": "3.14.6",
    "homeassistant": "2026.10.1",
    "jinja2": "3.1.6",
}


def collect(container_name):
    cases_bytes = (HERE / "cases.json").read_bytes()
    worker_bytes = (HERE / "ha_worker.py").read_bytes()
    fixture = json.loads(cases_bytes)
    if fixture["states"] != [] or [case["id"] for case in fixture["cases"]] != EXPECTED_IDS:
        raise ValueError("NHA-107 legacy corpus drift")
    reference, arch, context, endpoint = harness.local_image()
    result = json.loads(
        harness.run_ha_worker(
            reference,
            (context, endpoint),
            worker_bytes.decode("utf-8"),
            cases_bytes.decode("utf-8"),
            name=container_name,
        )
    )
    if result["runtime"] != EXPECTED_RUNTIME:
        raise RuntimeError("unexpected HA runtime")
    if [row["id"] for row in result["cases"]] != EXPECTED_IDS:
        raise RuntimeError("HA dropped or reordered a case")
    for row in result["cases"]:
        raw = row["raw"]
        if raw["text"].strip() not in ("0", "0.0"):
            raise AssertionError(f"legacy fallback changed for {row['id']}: {raw!r}")
        native = raw["native"]
        if native["type"] not in ("int", "float") or type(native["value"]) not in (int, float) or native["value"] != 0:
            raise AssertionError(f"legacy native zero changed for {row['id']}: {native!r}")
    return {
        "inputs": {
            "cases_sha256": hashlib.sha256(cases_bytes).hexdigest(),
            "ha_worker_sha256": hashlib.sha256(worker_bytes).hexdigest(),
        },
        "reference": {
            "image_index": harness.IMAGE_INDEX,
            "image_manifest": harness.MANIFESTS[arch],
            "platform": f"linux/{arch}",
            "runtime": result["runtime"],
        },
        "ha": result["cases"],
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--name", required=True, choices=("nha107-legacy-20261019",))
    args = parser.parse_args()
    observed = collect(args.name)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(observed, indent=2, sort_keys=True) + "\n")
