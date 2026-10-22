#!/usr/bin/env python3
"""Build the isolated server image and record actual binary/tool provenance."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
IMAGE = "homeward-server:nha168"


def command(*args, capture=False):
    return subprocess.run(args, cwd=ROOT, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path)
    parser.add_argument("--image", default=IMAGE)
    parser.add_argument("--skip-build", action="store_true")
    args = parser.parse_args()
    output = args.output or Path(tempfile.mkdtemp(prefix="homeward-package-nha168-"))
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    if ROOT == output.resolve() or ROOT in output.resolve().parents:
        raise SystemExit("package output must be outside the source checkout")
    if not args.skip_build:
        command("docker", "build", "--file", "packaging/Dockerfile", "--tag", args.image,
                "--iidfile", str(output / "image-id"), ".")
    info = json.loads(command("docker", "image", "inspect", args.image, capture=True).stdout)[0]
    container = command("docker", "create", args.image, capture=True).stdout.strip()
    try:
        for name in ("homed", "homeward", "homeward-package"):
            command("docker", "cp", f"{container}:/usr/local/bin/{name}", str(output / name))
    finally:
        command("docker", "rm", container, capture=True)
    command("docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
            "--entrypoint", "/usr/local/bin/homeward-package", args.image, "tzcheck")
    worker_check = """
import importlib.metadata as m, json, sys, ssl
import grpc, google.protobuf, pylutron_caseta, orjson, cryptography, cffi
from drivers.caseta import worker
packages = ['grpcio', 'grpcio-tools', 'protobuf', 'setuptools', 'typing-extensions',
            'cffi', 'cryptography', 'orjson', 'pycparser', 'pylutron-caseta']
print(json.dumps({'python': sys.version, 'openssl': ssl.OPENSSL_VERSION,
                  'packages': {name:m.version(name) for name in packages}}))
"""
    worker = json.loads(command("docker", "run", "--rm", "--network", "none", "--read-only",
                                "--cap-drop", "ALL", "--entrypoint", "python3", args.image,
                                "-c", worker_check, capture=True).stdout)
    declared = json.loads((ROOT / "packaging/versions.json").read_text())
    for line in (ROOT / "testworker/requirements.lock").read_text().splitlines():
        name, version = line.split("==")
        if worker["packages"][name] != version:
            raise SystemExit("runtime worker dependency mismatch")
    if worker["python"].split()[0] != "3.13.17":
        raise SystemExit("runtime Python version mismatch")
    files = {}
    go = ROOT / ".tools/bin/go"
    env = os.environ.copy()
    env["GOTOOLCHAIN"] = "local"
    for name in ("homed", "homeward", "homeward-package"):
        binary = output / name
        binary.chmod(0o755)
        build_info = subprocess.run([str(go), "version", "-m", str(binary)], cwd=ROOT,
                                    env=env, text=True, capture_output=True, check=True).stdout
        if "go1.27.2" not in build_info or "timetzdata" not in build_info:
            raise SystemExit("runtime binary toolchain/timezone provenance mismatch")
        files[name] = {"sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                       "go_build_info": build_info}
    deployment = {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
                  for name in declared["deployment_files"]}
    names = command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", capture=True).stdout.split("\0")
    source_files = {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
                    for name in sorted(set(names)) if name and (ROOT / name).is_file()}
    compose = json.loads(command("docker", "compose", "--file", "packaging/compose.yaml", "config", "--format", "json", capture=True).stdout)
    compose.pop("name", None)
    for service in ("prepare", "homed"):
        compose["services"][service]["image"] = info["Id"]
        compose["services"][service].pop("build", None)
        compose["services"][service]["pull_policy"] = "never"
    # Let the final operator project name own network/volume identities.
    for value in compose.get("networks", {}).values():
        value.pop("name", None)
    for value in compose.get("volumes", {}).values():
        value.pop("name", None)
    (output / "compose.lock.json").write_text(json.dumps(compose, indent=2) + "\n")
    deployment["compose.lock.json"] = hashlib.sha256((output / "compose.lock.json").read_bytes()).hexdigest()
    provenance = {"schema_version": 1, "local_image_id": info["Id"],
                  "distribution_digests": info.get("RepoDigests", []),
                  "source_commit": command("git", "rev-parse", "HEAD", capture=True).stdout.strip(),
                  "source_diff_sha256": hashlib.sha256(command("git", "diff", "HEAD", capture=True).stdout.encode()).hexdigest(),
                  "source_files_sha256": source_files,
                  "binaries": files, "worker": worker, "deployment_sha256": deployment,
                  "declared_provenance": declared,
                  "docker_version": command("docker", "version", "--format", "{{.Client.Version}}/{{.Server.Version}}", capture=True).stdout.strip(),
                  "compose_version": command("docker", "compose", "version", "--short", capture=True).stdout.strip()}
    (output / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
    print(output / "provenance.json")


if __name__ == "__main__":
    main()
