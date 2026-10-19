"""Record/replay differential evidence against real, isolated HA 2026.10.1."""

from datetime import datetime
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time


ROOT = Path(__file__).resolve().parents[3]
HERE = Path(__file__).resolve().parent
CASES = HERE / "cases.json"
WORKER = HERE / "ha_worker.py"
OBSERVED = HERE / "observed.json"
EXPECTED_TZDATA = "go1.27.2/sha256:b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779"
EVIDENCE_ROOT = Path("/Users/damien/Library/Caches/codex-build/homeward/qualification/nha105")


def harness():
    path = ROOT / "compat/ha/nha006/test_nha_006.py"
    spec = importlib.util.spec_from_file_location("nha105_existing_local_harness", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def instant(text):
    value = datetime.fromisoformat(text)
    if value.utcoffset() is None or value.utcoffset().total_seconds() != 0:
        raise AssertionError("oracle timestamp is not UTC")
    return value


def compare_occurrence(actual, expected, label, metrics):
    if actual.get("kind") != expected.get("kind"):
        raise AssertionError(f"{label}: presence differs")
    if actual["kind"] == "present":
        if set(actual) != {"kind", "instant_utc"} or set(expected) != {"kind", "instant_utc"}:
            raise AssertionError(f"{label}: invalid present representation")
        delta = abs((instant(actual["instant_utc"]) - instant(expected["instant_utc"])).total_seconds())
        metrics["max_event_error_seconds"] = max(metrics["max_event_error_seconds"], delta)
        if delta > 1:
            raise AssertionError(f"{label}: event differs by {delta}s")
    elif actual["kind"] == "absent":
        if actual != expected or set(actual) != {"kind", "reason"}:
            raise AssertionError(f"{label}: absence reason differs or contains a forged instant")
    else:
        raise AssertionError(f"{label}: unknown occurrence branch")


def compare(fixtures, go, ha):
    if go.get("go_version") != "go1.27.2" or go.get("astral_version") != "2.2" or go.get("timezone_data_id") != EXPECTED_TZDATA:
        raise AssertionError("Go toolchain, port, or timezone identity drifted")
    if (ha.get("homeassistant"), ha.get("astral")) != ("2026.10.1", "2.2"):
        raise AssertionError("reference runtime version drifted")
    expected_ids = [case["id"] for case in fixtures["cases"]]
    if len(set(expected_ids)) != len(expected_ids):
        raise AssertionError("duplicate synthetic fixture identity")
    if [case["id"] for case in go["cases"]] != expected_ids or [case["id"] for case in ha["cases"]] != expected_ids:
        raise AssertionError("reference/probe case ordering differs")
    metrics = {"max_event_error_seconds": 0.0, "max_angle_error_degrees": 0.0, "max_phase_error_seconds": 0.0, "ha_entity_exhaustion_cases": []}
    for fixture, actual, expected in zip(fixtures["cases"], go["cases"], ha["cases"], strict=True):
        label = fixture["id"]
        if actual["date"] != fixture["date"] or expected["date"] != fixture["date"]:
            raise AssertionError(f"{label}: requested civil date was changed")
        if len(actual["events"]) != 6 or len(expected["events"]) != 6:
            raise AssertionError(f"{label}: six events required")
        for got, want in zip(actual["events"], expected["events"], strict=True):
            if got["event"] != want["event"]:
                raise AssertionError(f"{label}: event order differs")
            for mode in ("date", "next"):
                compare_occurrence(got[mode], want[mode], f"{label}/{got['event']}/{mode}", metrics)
                if mode == "next":
                    for outcome, after in ((got[mode], actual["after_utc"]), (want[mode], expected["after_utc"])):
                        if outcome["kind"] == "present" and instant(outcome["instant_utc"]) <= instant(after):
                            raise AssertionError(f"{label}: next event was not strictly future")
        if fixture.get("after_event"):
            for outcome in (actual, expected):
                anchor = instant(outcome["anchor_utc"])
                event = next(item for item in outcome["events"] if item["event"] == fixture["after_event"])
                following = instant(event["next"]["instant_utc"])
                if fixture.get("after_shift_us", 0) < 0:
                    if following != anchor:
                        raise AssertionError(f"{label}: event one microsecond ahead was skipped")
                elif following <= anchor:
                    raise AssertionError(f"{label}: equality returned the same occurrence")
        got, want = actual["sun"], expected["sun"]
        if not 1 <= got["date_evaluations"] <= 12 * 368:
            raise AssertionError(f"{label}: calculation exceeded date bound")
        primary_position = expected["position"]
        if got["state"] != primary_position["state"] or got["rising"] is not primary_position["rising"]:
            raise AssertionError(f"{label}: primary Astral/HA horizon or rising differs")
        for field in ("elevation", "azimuth"):
            error = abs(got[field] - primary_position[field])
            metrics["max_angle_error_degrees"] = max(metrics["max_angle_error_degrees"], error)
            if error > 0.010000001:
                raise AssertionError(f"{label}: primary Astral {field} differs by {error} degrees")
        if want.get("error"):
            if want != {"error": "no_event_within_search_horizon"}:
                raise AssertionError(f"{label}: unexpected HA entity error")
            # The helpers above still compare every typed absent/present result.
            # HA's entity raises; Homeward deliberately retains typed absences.
            metrics["ha_entity_exhaustion_cases"].append(label)
            continue
        if got["state"] != want["state"] or got["rising"] is not want["rising"] or got["phase"] != want["phase"]:
            raise AssertionError(f"{label}: horizon, rising or phase differs")
        for field in ("elevation", "azimuth"):
            error = abs(got[field] - want[field])
            metrics["max_angle_error_degrees"] = max(metrics["max_angle_error_degrees"], error)
            if error > 0.010000001:
                raise AssertionError(f"{label}: rounded {field} differs by {error} degrees")
        for field in ("next_change", "next_position"):
            if bool(got.get(field)) != bool(want.get(field)):
                raise AssertionError(f"{label}: {field} timer presence differs")
            if got.get(field):
                delta = abs((instant(got[field]) - instant(want[field])).total_seconds())
                metrics["max_phase_error_seconds"] = max(metrics["max_phase_error_seconds"], delta)
                if delta > 1:
                    raise AssertionError(f"{label}: {field} differs by {delta}s")
        for index, (actual_next, expected_next) in enumerate(zip(got["next"], want["next"], strict=True)):
            compare_occurrence(actual_next, expected_next, f"{label}/Sun.next[{index}]", metrics)
    return metrics


def input_hashes():
    return {
        "cases_sha256": hashlib.sha256(CASES.read_bytes()).hexdigest(),
        "ha_worker_sha256": hashlib.sha256(WORKER.read_bytes()).hexdigest(),
    }


def collect():
    local = harness()
    evidence = EVIDENCE_ROOT / time.strftime("p0-%Y%m%dT%H%M%SZ", time.gmtime())
    evidence.mkdir(parents=True, exist_ok=False)
    print(f"NHA105_RAW_EVIDENCE={evidence}", file=sys.stderr, flush=True)
    command_index = 0

    def captured_run(args, *, input_text=None, env=None, timeout=120):
        nonlocal command_index
        command_index += 1
        prefix = evidence / f"command-{command_index:02d}"
        started = time.monotonic()
        process = subprocess.Popen(
            args, text=True,
            stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            cwd=ROOT, env=env, start_new_session=True,
        )
        timed_out = False
        try:
            stdout, stderr = process.communicate(input=input_text, timeout=timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                stdout, stderr = process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                stdout, stderr = process.communicate()
        prefix.with_suffix(".stdout").write_text(stdout)
        prefix.with_suffix(".stderr").write_text(stderr)
        prefix.with_suffix(".json").write_text(json.dumps({
            "args": args, "exit_code": process.returncode, "timeout": timed_out,
            "elapsed_seconds": time.monotonic() - started,
        }, indent=2) + "\n")
        if timed_out or process.returncode:
            raise RuntimeError(f"P0 command failed; raw evidence: {prefix}")
        return stdout

    # The existing isolation/lifecycle helper calls its module-global runner.
    # Replacing only that runner preserves its checks while retaining complete
    # stdout/stderr and status for successes, failures, and timeouts.
    local.run = captured_run
    # Check image existence/digest before any Go build. Missing image fails;
    # neither this harness nor the reused one pulls an image.
    reference, arch, context, endpoint = local.local_image()
    env = dict(os.environ)
    env.update(
        GOTOOLCHAIN="local",
        PATH=str(ROOT / ".tools/bin") + os.pathsep + env["PATH"],
        GOCACHE="/Users/damien/Library/Caches/codex-build/homeward/go-build",
        GOTMPDIR="/Users/damien/Library/Caches/codex-build/homeward/go-tmp",
        TMPDIR="/Users/damien/Library/Caches/codex-build/homeward/go-tmp",
        CARGO_TARGET_DIR="/Users/damien/Library/Caches/codex-build/homeward/workspace/target",
        CARGO_INCREMENTAL="0",
        CARGO_PROFILE_DEV_DEBUG="line-tables-only",
        CARGO_PROFILE_TEST_DEBUG="line-tables-only",
        PYTHONDONTWRITEBYTECODE="1",
    )
    fixtures_text = CASES.read_text()
    started = time.monotonic()
    go_text = local.run(["go", "run", "./compat/ha/nha105/cmd/nha105-probe"], input_text=fixtures_text, env=env, timeout=60)
    (evidence / "go.json").write_text(go_text)
    ha_text = local.run_ha_worker(reference, (context, endpoint), WORKER.read_text(), fixtures_text, timeout=30)
    (evidence / "ha.json").write_text(ha_text)
    go, ha = json.loads(go_text), json.loads(ha_text)
    elapsed = time.monotonic() - started
    metrics = compare(json.loads(fixtures_text), go, ha)
    return {
        "inputs": input_hashes(),
        "image": {"reference": reference, "architecture": arch, "index": local.IMAGE_INDEX},
        "ha_profile_commit": "abe1fdd7cd6b4de1715c87b6fd87d92699514ef9",
        "elapsed_seconds": elapsed,
        "raw_evidence_directory": str(evidence),
        "comparison": metrics,
        "go": go,
        "ha": ha,
    }


def test_nha105_recorded_ha_profile():
    if not OBSERVED.is_file():
        raise AssertionError("NHA-105 P0 has not produced an observed HA archive")
    archive = json.loads(OBSERVED.read_text())
    if archive["inputs"] != input_hashes():
        raise AssertionError("HA archive uses different fixture or worker sources")
    if archive["image"]["reference"] not in {
        "ghcr.io/home-assistant/home-assistant@sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7",
        "ghcr.io/home-assistant/home-assistant@sha256:a7ecc966ffed643296af32d576ee081910a0362c01e695db24fbee6816175fbe",
    }:
        raise AssertionError("HA archive does not use a pinned platform manifest")
    actual = compare(json.loads(CASES.read_text()), archive["go"], archive["ha"])
    if archive["comparison"] != actual:
        raise AssertionError("recorded comparison does not match the observations")


if __name__ == "__main__":
    if sys.argv[1:] != ["--record"]:
        raise SystemExit("usage: test_nha_105.py --record (requires the root build lease)")
    captured = collect()
    OBSERVED.write_text(json.dumps(captured, indent=2, sort_keys=True, allow_nan=False) + "\n")
    print(json.dumps({"observed": str(OBSERVED.relative_to(ROOT)), "comparison": captured["comparison"], "elapsed_seconds": captured["elapsed_seconds"]}, sort_keys=True))
