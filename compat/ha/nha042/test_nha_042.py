"""Run the real NHA-042 P0 only on the local, already installed HA image.

No automatic collection, pulling, package installation, or observation file
creation occurs by importing this module. --collect requires an explicit output
path so an unrun oracle cannot be confused with observed.json.
"""

import argparse
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

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
SOCKET = Path("/Users/damien/.orbstack/run/docker.sock")
DOCKER = ["docker", "--host", "unix://" + str(SOCKET)]
IMAGE = "ghcr.io/home-assistant/home-assistant"
MANIFESTS = {
    "arm64": "sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7",
    "amd64": "sha256:a7ecc966ffed643296af32d576ee081910a0362c01e695db24fbee6816175fbe",
}
EVIDENCE = None
COMMAND = 0


def run(arguments, *, input_text=None, timeout=120, raw_output=None):
    global COMMAND
    COMMAND += 1
    prefix = EVIDENCE / f"command-{COMMAND:03d}"
    metadata = {"argv": arguments, "timeout_seconds": timeout, "started_unix": time.time(),
                "stdin_sha256": hashlib.sha256(input_text.encode()).hexdigest() if input_text is not None else None}
    prefix.with_suffix(".json").write_text(json.dumps(metadata, indent=2) + "\n")
    try:
        process = subprocess.Popen(arguments, cwd=ROOT, start_new_session=True,
                                   stdin=subprocess.PIPE if input_text is not None else subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except OSError as error:
        prefix.with_suffix(".stdout").write_bytes(b"")
        prefix.with_suffix(".stderr").write_bytes(repr(error).encode())
        if raw_output is not None:
            raw_output.write_bytes(b"")
        metadata.update({"returncode": None, "spawn_error": type(error).__name__, "timed_out": False, "finished_unix": time.time()})
        prefix.with_suffix(".json").write_text(json.dumps(metadata, indent=2) + "\n")
        raise
    try:
        stdout, stderr = process.communicate(input=input_text.encode() if input_text is not None else None, timeout=timeout)
    except subprocess.TimeoutExpired:
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
        prefix.with_suffix(".stdout").write_bytes(stdout)
        prefix.with_suffix(".stderr").write_bytes(stderr)
        if raw_output is not None:
            raw_output.write_bytes(stdout)
        metadata.update({"returncode": process.returncode, "timed_out": True, "finished_unix": time.time()})
        prefix.with_suffix(".json").write_text(json.dumps(metadata, indent=2) + "\n")
        raise
    prefix.with_suffix(".stdout").write_bytes(stdout)
    prefix.with_suffix(".stderr").write_bytes(stderr)
    if raw_output is not None:
        raw_output.write_bytes(stdout)
    metadata.update({"returncode": process.returncode, "timed_out": False, "finished_unix": time.time()})
    prefix.with_suffix(".json").write_text(json.dumps(metadata, indent=2) + "\n")
    if process.returncode:
        raise RuntimeError(f"command exited {process.returncode}: {stderr[-8000:].decode(errors='replace')}")
    return stdout.decode("utf-8")


def manifest(phase):
    paths = [HERE / "cases.json", HERE / "ha_worker.py", Path(__file__).resolve()]
    source = Path("/Users/damien/Library/Caches/codex-build/homeward/qualification/nha042/source")
    paths += sorted(path for path in source.iterdir() if path.is_file())
    EVIDENCE.joinpath(f"manifest-{phase}.json").write_text(json.dumps(
        {str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in paths}, indent=2, sort_keys=True) + "\n")


def collect(output):
    if not stat.S_ISSOCK(SOCKET.stat().st_mode):
        raise RuntimeError("local OrbStack socket absent")
    if os.environ.get("DOCKER_HOST") not in (None, "", "unix://" + str(SOCKET)):
        raise RuntimeError("refusing another Docker endpoint")
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(platform.machine().lower())
    if arch not in MANIFESTS:
        raise RuntimeError("unsupported architecture")
    reference = IMAGE + "@" + MANIFESTS[arch]
    inspected = json.loads(run(DOCKER + ["image", "inspect", reference]))[0]
    if inspected["Architecture"] != arch or reference not in inspected["RepoDigests"]:
        raise RuntimeError("local HA image differs from pinned manifest")
    worker = (HERE / "ha_worker.py").read_bytes()
    fixtures = (HERE / "cases.json").read_bytes()
    name = "nha042-p0-" + uuid.uuid4().hex
    created = False
    try:
        run(DOCKER + ["create", "--name", name, "-i", "--pull", "never", "--network", "none", "--read-only",
                      "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                      "--pids-limit", "128", "--entrypoint", "python3", reference, "-c", worker.decode()], timeout=30)
        created = True
        raw = run(DOCKER + ["start", "-a", "-i", name], input_text=fixtures.decode(), timeout=180,
                  raw_output=EVIDENCE / "ha-raw.stdout")
        observed = json.loads(raw)
        observed["provenance"] = {"image": reference, "architecture": arch,
                                  "worker_sha256": hashlib.sha256(worker).hexdigest(),
                                  "fixtures_sha256": hashlib.sha256(fixtures).hexdigest()}
        output.write_text(json.dumps(observed, indent=2, sort_keys=True) + "\n")
        print(json.dumps({"output": str(output), "cases": len(observed["cases"]), "runtime": observed["runtime"]}, sort_keys=True))
    finally:
        primary_failed = sys.exc_info()[0] is not None
        # A failed create does not prove ownership of a container with this
        # name. Clean up only after this invocation successfully created it.
        if created:
            try:
                run(DOCKER + ["rm", "-f", name], timeout=30)
            except Exception as cleanup_error:
                if not primary_failed:
                    raise
                print(f"owned container cleanup also failed: {cleanup_error}", file=sys.stderr)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--collect", action="store_true")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--evidence-dir", type=Path, required=True)
    arguments = parser.parse_args()
    if not arguments.collect or arguments.output is None:
        parser.error("explicit --collect and --output are required; no oracle has been collected")
    EVIDENCE = arguments.evidence_dir
    EVIDENCE.mkdir(parents=True, exist_ok=False)
    manifest("before")
    try:
        collect(arguments.output)
    finally:
        manifest("after")
