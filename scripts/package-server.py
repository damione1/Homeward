#!/usr/bin/env python3
"""Build the isolated server image and record actual binary/tool provenance."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import shutil


ROOT = Path(__file__).resolve().parents[1]
TOOLS_ROOT = ROOT
IMAGE = "homeward-server:nha168"


def command(*args, capture=False):
    return subprocess.run(args, cwd=ROOT, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None)


def sha256(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def verify_platform(info, platform):
    actual = info["Os"] + "/" + info["Architecture"]
    if actual != platform:
        raise ValueError(f"image platform mismatch: requested {platform}, actual {actual}")


def verify_binary(binary, build_info, platform):
    data = binary.read_bytes()[:20]
    arch = platform.split("/")[1]
    machine = {"amd64": 62, "arm64": 183}[arch]
    if (data[:6] != b"\x7fELF\x02\x01" or len(data) != 20
            or int.from_bytes(data[18:20], "little") != machine
            or "GOOS=linux" not in build_info or f"GOARCH={arch}" not in build_info
            or "CGO_ENABLED=0" not in build_info):
        raise ValueError("binary ELF/build-info platform mismatch: " + binary.name)


def main():
    global ROOT
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path)
    parser.add_argument("--image", default=IMAGE)
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--platform", required=True, choices=("linux/amd64", "linux/arm64"))
    parser.add_argument("--no-cache", action="store_true")
    parser.add_argument("--source-root", type=Path)
    parser.add_argument("--source-manifest", type=Path)
    parser.add_argument("--source-date-epoch", type=int)
    parser.add_argument("--export-image", action="store_true")
    parser.add_argument("--go-tool", type=Path, default=TOOLS_ROOT / ".tools/bin/go")
    args = parser.parse_args()
    if args.source_root:
        ROOT = args.source_root.resolve()
    output = args.output or Path(tempfile.mkdtemp(prefix="homeward-package-nha168-"))
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    if ROOT == output.resolve() or ROOT in output.resolve().parents:
        raise SystemExit("package output must be outside the source checkout")
    declared = json.loads((ROOT / "packaging/versions.json").read_text())
    binaries = declared["binaries"]
    source_manifest = None
    if args.source_manifest:
        source_manifest = json.loads(args.source_manifest.read_text())
        actual = {str(p.relative_to(ROOT)): sha256(p) for p in sorted(ROOT.rglob("*")) if p.is_file()}
        if actual != source_manifest["files_sha256"]:
            raise SystemExit("source archive differs from exact commit manifest")
    epoch = args.source_date_epoch
    if epoch is None:
        if source_manifest:
            epoch = source_manifest["source_date_epoch"]
        else:
            epoch = int(command("git", "show", "-s", "--format=%ct", "HEAD", capture=True).stdout)
    if not args.skip_build:
        clean = ("--no-cache",) if args.no_cache else ()
        command("docker", "build", "--platform", args.platform, *clean,
                "--build-arg", f"SOURCE_DATE_EPOCH={epoch}",
                "--file", "packaging/Dockerfile", "--tag", args.image,
                "--iidfile", str(output / "image-id"), ".")
    info = json.loads(command("docker", "image", "inspect", args.image, capture=True).stdout)[0]
    verify_platform(info, args.platform)
    image_id = info["Id"]
    # Resolve tags once. Every probe/export below uses this immutable image ID.
    (output / "image-inspect.json").write_text(json.dumps(info, sort_keys=True, indent=2) + "\n")
    container = command("docker", "create", "--platform", args.platform, image_id, capture=True).stdout.strip()
    try:
        for name in binaries:
            command("docker", "cp", f"{container}:/usr/local/bin/{name}", str(output / name))
        command("docker", "cp", f"{container}:/opt/homeward/release", str(output / "inventory"))
    finally:
        command("docker", "rm", container, capture=True)
    command("docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
            "--entrypoint", "/usr/local/bin/homeward-package", image_id, "tzcheck")
    worker_check = """
import importlib.metadata as m, json, sys, ssl, hashlib
from pathlib import Path
import grpc, google.protobuf, pylutron_caseta, orjson, cryptography, cffi
from drivers.caseta import worker
packages = ['grpcio', 'grpcio-tools', 'protobuf', 'setuptools', 'typing-extensions',
            'cffi', 'cryptography', 'orjson', 'pycparser', 'pylutron-caseta']
inventory=json.loads(Path('/opt/homeward/release/runtime-inventory.json').read_text())
files={path:item['sha256'] for path,item in inventory['debian_files'].items()}
for distribution in inventory['python']: files.update(distribution['files'])
files.update(inventory['python_interpreter']['files'])
for path,digest in files.items():
 if hashlib.sha256(Path(path).read_bytes()).hexdigest()!=digest: raise SystemExit('runtime package file changed: '+path)
print(json.dumps({'python': sys.version, 'openssl': ssl.OPENSSL_VERSION,
                  'packages': {name:m.version(name) for name in packages}, 'verified_inventory_files':len(files)}))
"""
    worker = json.loads(command("docker", "run", "--rm", "--network", "none", "--read-only",
                                "--cap-drop", "ALL", "--entrypoint", "python3", image_id,
                                "-c", worker_check, capture=True).stdout)
    for line in (ROOT / "testworker/requirements.lock").read_text().splitlines():
        name, version = line.split("==")
        if worker["packages"][name] != version:
            raise SystemExit("runtime worker dependency mismatch")
    if worker["python"].split()[0] != "3.13.17":
        raise SystemExit("runtime Python version mismatch")
    client_check = """
import hashlib,json,os,subprocess
from pathlib import Path
root=Path('/opt/homeward/pg-client')
loaders=list((root/'lib').glob('ld-linux*'))
assert os.getuid()==10001 and len(loaders)==1
tools={}
for name in ('pg_dump','pg_restore'):
 version=subprocess.check_output([str(root/'bin'/name),'--version'],text=True).strip()
 assert version.split()[:3]==[name,'(PostgreSQL)','17.10']
 linkage=subprocess.check_output([str(loaders[0]),'--library-path',str(root/'lib'),'--list',str(root/'bin'/(name+'.real'))],text=True)
 assert 'not found' not in linkage
 assert all(line.split('=>',1)[1].strip().split()[0].startswith(str(root/'lib')+'/') for line in linkage.splitlines() if '=> /' in line)
 tools[name]={'version':version,'runtime_linkage':linkage}
print(json.dumps({'uid':os.getuid(),'source_image':(root/'source-image.txt').read_text().strip(),
 'source_linkage':(root/'source-linkage.txt').read_text(),'tools':tools,
 'files_sha256':{str(p.relative_to(root)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(root.rglob('*')) if p.is_file()}}))
"""
    clients = json.loads(command("docker", "run", "--rm", "--network", "none", "--read-only",
                                 "--cap-drop", "ALL", "--entrypoint", "/usr/local/bin/python3", image_id,
                                 "-c", client_check, capture=True).stdout)
    if clients["source_image"] != declared["postgres_clients"]["source_image"]:
        raise SystemExit("runtime PostgreSQL client provenance mismatch")
    files = {}
    go = args.go_tool.resolve()
    env = os.environ.copy()
    env["GOTOOLCHAIN"] = "local"
    for name in binaries:
        binary = output / name
        binary.chmod(0o755)
        build_info = subprocess.run([str(go), "version", "-m", str(binary)], cwd=ROOT,
                                    env=env, text=True, capture_output=True, check=True).stdout
        if "go1.27.2" not in build_info or "timetzdata" not in build_info:
            raise SystemExit("runtime binary toolchain/timezone provenance mismatch")
        verify_binary(binary, build_info, args.platform)
        files[name] = {"sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                       "go_build_info": build_info}
    deployment = {name: hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
                  for name in declared["deployment_files"]}
    if source_manifest:
        source_files = source_manifest["files_sha256"]
        source_commit = source_manifest["commit"]
        source_tree = source_manifest["tree"]
        source_diff = hashlib.sha256(b"").hexdigest()
    else:
        names = command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", capture=True).stdout.split("\0")
        source_files = {name: sha256(ROOT / name) for name in sorted(set(names)) if name and (ROOT / name).is_file()}
        source_commit = command("git", "rev-parse", "HEAD", capture=True).stdout.strip()
        source_tree = command("git", "rev-parse", "HEAD^{tree}", capture=True).stdout.strip()
        source_diff = hashlib.sha256(command("git", "diff", "HEAD", capture=True).stdout.encode()).hexdigest()
    compose = json.loads(command("docker", "compose", "--file", "packaging/compose.yaml", "config", "--format", "json", capture=True).stdout)
    compose.pop("name", None)
    for service in ("prepare", "homed"):
        compose["services"][service]["image"] = info["Id"]
        compose["services"][service].pop("build", None)
        compose["services"][service]["pull_policy"] = "never"
        compose["services"][service]["platform"] = args.platform
    compose["services"]["postgres"]["platform"] = args.platform
    # Let the final operator project name own network/volume identities.
    for value in compose.get("networks", {}).values():
        value.pop("name", None)
    for value in compose.get("volumes", {}).values():
        value.pop("name", None)
    (output / "compose.lock.json").write_text(json.dumps(compose, indent=2) + "\n")
    deployment["compose.lock.json"] = hashlib.sha256((output / "compose.lock.json").read_bytes()).hexdigest()
    provenance = {"schema_version": 1, "local_image_id": info["Id"],
                  "image_platform": {"os": info["Os"], "architecture": info["Architecture"], "variant": info.get("Variant", "")},
                  "distribution_digests": info.get("RepoDigests", []),
                  "source_commit": source_commit, "source_tree": source_tree,
                  "source_diff_sha256": source_diff,
                  "exact_commit_archive": source_manifest is not None,
                  "source_date_epoch": epoch, "requested_platform": args.platform,
                  "clean_build": args.no_cache and not args.skip_build,
                  "source_files_sha256": source_files,
                  "binaries": files, "worker": worker, "postgres_clients": clients, "deployment_sha256": deployment,
                  "declared_provenance": declared,
                  "docker_version": command("docker", "version", "--format", "{{.Client.Version}}/{{.Server.Version}}", capture=True).stdout.strip(),
                  "compose_version": command("docker", "compose", "version", "--short", capture=True).stdout.strip()}
    shutil.copyfile(ROOT / "packaging/versions.json", output / "versions.json")
    if args.export_image:
        command("docker", "image", "save", "--output", str(output / "server-image.tar"), image_id)
        provenance["image_archive_sha256"] = sha256(output / "server-image.tar")
    (output / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
    print(output / "provenance.json")


if __name__ == "__main__":
    main()
