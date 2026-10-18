"""Differential fixture check: real HA helper in an isolated image vs Gonja."""

from copy import deepcopy
import difflib
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import stat
import subprocess
import sys
import time
import uuid

import pytest


ROOT = Path(__file__).resolve().parents[3]
HERE = Path(__file__).resolve().parent
CASES = HERE / "cases.json"
OBSERVED = HERE / "observed.json"
HA_WORKER = HERE / "ha_worker.py"
IMAGE = "ghcr.io/home-assistant/home-assistant"
IMAGE_INDEX = "sha256:130241f28d01fa80dfa3b44f7d89781c188d8642ecab9024f079f19f9f82baa0"
MANIFESTS = {
    "arm64": "sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7",
    "amd64": "sha256:a7ecc966ffed643296af32d576ee081910a0362c01e695db24fbee6816175fbe",
}


def run(args, *, input_text=None, env=None, timeout=120):
    process = subprocess.Popen(
        args,
        text=True,
        stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        cwd=ROOT,
        env=env,
        start_new_session=True,
    )
    try:
        stdout, stderr = process.communicate(input=input_text, timeout=timeout)
    except subprocess.TimeoutExpired as err:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate()
        raise TimeoutError(f"{args[:3]} exceeded {timeout}s; process group stopped") from err
    if process.returncode:
        raise RuntimeError(
            f"{args[:3]} exited {process.returncode}:\n{stderr[-4000:]}"
        )
    return stdout


def check_local_docker(expected=None):
    """Resolve and verify a local Unix socket before each Docker operation."""
    context = os.environ.get("NHA006_DOCKER_CONTEXT") or run(
        ["docker", "context", "show"]
    ).strip()
    if not context:
        raise RuntimeError("NHA-006 has no selected Docker context")
    endpoint = run(
        [
            "docker",
            "context",
            "inspect",
            context,
            "--format",
            '{{(index .Endpoints "docker").Host}}',
        ]
    ).strip()
    if not endpoint.startswith("unix:///"):
        raise RuntimeError(f"NHA-006 refuses non-local Docker endpoint {endpoint!r}")
    socket_path = Path(endpoint.removeprefix("unix://"))
    if not stat.S_ISSOCK(socket_path.stat().st_mode):
        raise RuntimeError(f"Docker endpoint is not a Unix socket: {socket_path}")
    host_override = os.environ.get("DOCKER_HOST")
    if host_override and host_override != endpoint:
        raise RuntimeError(f"NHA-006 refuses DOCKER_HOST override {host_override!r}")
    if expected is not None and (context, endpoint) != expected:
        raise RuntimeError("Docker context or endpoint changed during NHA-006 probe")
    return context, endpoint


def local_image():
    machine = platform.machine().lower()
    arch = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(machine)
    if arch is None:
        raise RuntimeError(f"unsupported NHA-006 reference platform: {machine}")
    reference = f"{IMAGE}@{MANIFESTS[arch]}"
    context, endpoint = check_local_docker()
    try:
        inspected = json.loads(
            run(["docker", "--context", context, "image", "inspect", reference])
        )[0]
    except RuntimeError as err:
        if "No such image" in str(err):
            raise RuntimeError(
                f"pinned HA image absent; pull {reference} for linux/{arch} first"
            ) from err
        raise
    if inspected["Architecture"] != arch or reference not in inspected["RepoDigests"]:
        raise RuntimeError("local image does not match the pinned HA platform manifest")
    return reference, arch, context, endpoint


def run_ha_worker(reference, docker_identity, worker_text, fixture_text, *, timeout=120, name=None):
    """Own one container by name and remove it even if attach times out."""
    context, _ = docker_identity
    name = name or f"nha006-probe-{uuid.uuid4().hex}"
    check_local_docker(docker_identity)
    try:
        run(
            [
                "docker", "--context", context, "create", "--name", name, "-i",
                "--pull", "never", "--network", "none", "--read-only",
                "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges", "--pids-limit", "128",
                "--entrypoint", "python3", reference, "-c", worker_text,
            ],
            timeout=30,
        )
        check_local_docker(docker_identity)
        return run(
            ["docker", "--context", context, "start", "-a", "-i", name],
            input_text=fixture_text,
            timeout=timeout,
        )
    finally:
        check_local_docker(docker_identity)
        try:
            run(["docker", "--context", context, "rm", "-f", name], timeout=30)
        except RuntimeError as err:
            if "No such container" not in str(err):
                raise


def collect():
    fixture_bytes = CASES.read_bytes()
    worker_bytes = HA_WORKER.read_bytes()
    fixtures = json.loads(fixture_bytes)
    fixture_ids = [case["id"] for case in fixtures["cases"]]
    if len(fixture_ids) != 10 or len(set(fixture_ids)) != 10:
        raise ValueError("fixtures must contain ten distinct cases")
    fixture_text = fixture_bytes.decode("utf-8")
    worker_text = worker_bytes.decode("utf-8")

    env = dict(os.environ)
    env.update(
        GOTOOLCHAIN="local",
        PATH=str(ROOT / ".tools/bin") + os.pathsep + env["PATH"],
    )
    go_result = json.loads(
        run(
            ["go", "run", "./compat/ha/nha006/cmd/nha006-probe"],
            input_text=fixture_text,
            env=env,
        )
    )
    go_module = json.loads(
        run(["go", "list", "-m", "-json", "github.com/nikolalohinski/gonja/v2"], env=env)
    )
    go_version = run(["go", "env", "GOVERSION"], env=env).strip()
    if (go_module["Path"], go_module["Version"], go_module["Sum"], go_version) != (
        "github.com/nikolalohinski/gonja/v2",
        "v2.9.1",
        "h1:ZDG0zYs5oR3fsqQFAlkaWiWYxPOBrCUK9k2IsRZhMa8=",
        "go1.25.2",
    ):
        raise RuntimeError("Gonja module, sum or Go toolchain drifted from pin")

    reference, arch, context, endpoint = local_image()
    ha_result = json.loads(
        run_ha_worker(reference, (context, endpoint), worker_text, fixture_text)
    )
    if [case["id"] for case in go_result["cases"]] != fixture_ids:
        raise RuntimeError("Gonja dropped or reordered a fixture")
    if [case["id"] for case in ha_result["cases"]] != fixture_ids:
        raise RuntimeError("HA dropped or reordered a fixture")
    if ha_result["runtime"] != {
        "python": "3.14.6", "homeassistant": "2026.10.1", "jinja2": "3.1.6"
    }:
        raise RuntimeError(f"unexpected pinned HA runtime: {ha_result['runtime']}")
    return {
        "inputs": {
            "cases_sha256": hashlib.sha256(fixture_bytes).hexdigest(),
            "ha_worker_sha256": hashlib.sha256(worker_bytes).hexdigest(),
        },
        "reference": {
            "image_index": IMAGE_INDEX,
            "image_manifest": MANIFESTS[arch],
            "platform": f"linux/{arch}",
            "runtime": ha_result["runtime"],
        },
        "ha": ha_result["cases"],
        "gonja": {
            "runtime": {
                "go": go_version,
                "module": go_module["Path"],
                "version": go_module["Version"],
                "sum": go_module["Sum"],
            },
            "cases": go_result["cases"],
        },
    }


def canonical(value):
    """JSON equality that keeps bool/int and int/float distinct."""
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


def semantic_results(observations):
    return {"ha": observations["ha"], "gonja": observations["gonja"]["cases"]}


def check_capture(observations):
    reference = observations["reference"]
    platform_name = reference["platform"]
    if not platform_name.startswith("linux/"):
        raise AssertionError("capture has no Linux platform")
    arch = platform_name.removeprefix("linux/")
    if arch not in MANIFESTS or reference["image_manifest"] != MANIFESTS[arch]:
        raise AssertionError("capture has an unknown platform manifest")
    if reference["image_index"] != IMAGE_INDEX:
        raise AssertionError("capture has the wrong image index")
    if reference["runtime"] != {
        "python": "3.14.6", "homeassistant": "2026.10.1", "jinja2": "3.1.6"
    }:
        raise AssertionError("capture has the wrong HA runtime")
    if observations["gonja"]["runtime"] != {
        "go": "go1.25.2",
        "module": "github.com/nikolalohinski/gonja/v2",
        "version": "v2.9.1",
        "sum": "h1:ZDG0zYs5oR3fsqQFAlkaWiWYxPOBrCUK9k2IsRZhMa8=",
    }:
        raise AssertionError("capture has the wrong Gonja runtime")


def difference(actual, expected):
    actual_text = json.dumps(actual, indent=2, sort_keys=True).splitlines()
    expected_text = json.dumps(expected, indent=2, sort_keys=True).splitlines()
    return "\n".join(
        difflib.unified_diff(expected_text, actual_text, fromfile="observed.json", tofile="live")
    )


@pytest.fixture(scope="module")
def live_observations():
    return collect()


def test_nha_006_live_reference_and_gonja(live_observations):
    archived = json.loads(OBSERVED.read_text())
    check_capture(live_observations)
    check_capture(archived)
    assert canonical(live_observations["inputs"]) == canonical(archived["inputs"]), (
        "fixture or HA worker source changed since the reference capture"
    )
    live_semantics = semantic_results(live_observations)
    archived_semantics = semantic_results(archived)
    assert canonical(live_semantics) == canonical(archived_semantics), difference(
        live_semantics, archived_semantics
    )


def test_nha_006_corrupt_reference_is_rejected(live_observations):
    clean = semantic_results(live_observations)
    corrupted_float = deepcopy(clean)
    corrupted_float["ha"][3]["raw"]["native"]["value"] = 0.0
    corrupted_bool = deepcopy(clean)
    corrupted_bool["ha"][5]["raw"]["native"]["value"] = 1
    assert canonical(clean) != canonical(corrupted_float)
    assert canonical(clean) != canonical(corrupted_bool)
    assert difference(clean, corrupted_float)
    assert difference(clean, corrupted_bool)


def test_nha_006_timeout_reclaims_reference_container():
    reference, _, context, endpoint = local_image()
    name = f"nha006-timeout-{uuid.uuid4().hex}"
    with pytest.raises(TimeoutError):
        run_ha_worker(
            reference, (context, endpoint), "import time; time.sleep(10)",
            "{}", timeout=0.2, name=name,
        )
    check_local_docker((context, endpoint))
    remaining = run(
        [
            "docker", "--context", context, "ps", "-a", "--filter",
            f"name=^{name}$", "--format", "{{.Names}}",
        ]
    )
    assert not remaining.strip(), f"reference test container was left behind: {name}"


def test_nha_006_timeout_stops_local_process_group(tmp_path):
    ready = tmp_path / "child_started"
    orphan = tmp_path / "orphan_wrote"
    child = (
        "import pathlib,time; time.sleep(0.8); "
        f"pathlib.Path({str(orphan)!r}).write_text('orphan')"
    )
    parent = (
        "import pathlib,subprocess,sys,time; "
        f"subprocess.Popen([sys.executable,'-c',{child!r}]); "
        f"pathlib.Path({str(ready)!r}).write_text('started'); time.sleep(10)"
    )
    with pytest.raises(TimeoutError):
        run([sys.executable, "-c", parent], timeout=0.4)
    assert ready.read_text() == "started"
    time.sleep(0.9)
    assert not orphan.exists(), "timed-out subprocess left a child running"


if __name__ == "__main__":
    if sys.argv[1:] != ["--record"]:
        raise SystemExit("usage: python test_nha_006.py --record")
    OBSERVED.write_text(json.dumps(collect(), indent=2, sort_keys=True) + "\n")
