#!/usr/bin/env python3
"""Offline Syft catalogues plus Homeward's small fail-closed closure check."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[1]


def sha256(path):
    result = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def dump(path, value):
    path.write_text(json.dumps(value, sort_keys=True, indent=2) + "\n")


def execute(*args):
    return subprocess.check_output(args, text=True).strip()


def json_stream(text):
    result, decoder = [], json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, end = decoder.raw_decode(text)
        result.append(value)
        text = text[end:]
    return result


def canonical(name):
    return re.sub(r"[-_.]+", "-", name).lower()


def documents(root, relative):
    directory = root / relative
    return {str(p.relative_to(root)): sha256(p) for p in sorted(directory.rglob("*")) if p.is_file()}


def valid_license(root, docs):
    return any(Path(name).name.lower().startswith(("license", "licence", "copyright", "copying"))
               and (root / name).is_file() and (root / name).stat().st_size > 30
               and sha256(root / name) == digest for name, digest in docs.items())


def require_documents(root, docs, label, errors):
    for path, digest in docs.items():
        if not (root / path).is_file() or sha256(root / path) != digest:
            errors.append(label + ": document missing or changed: " + path)
    if not valid_license(root, docs):
        errors.append(label + ": retained license/copyright text missing")


def notice_document(package, source, policy, name, errors):
    """One exact upstream text retained in the output; no SPDX-only fallback."""
    record = policy["supplemental_notices"][name]
    original, retained = source / record["source"], package / record["destination"]
    if (not original.is_file() or sha256(original) != record["sha256"] or
            not retained.is_file() or sha256(retained) != record["sha256"]):
        errors.append("source/retained notice absent or changed: " + name)
    return {record["destination"]: record["sha256"]}


def retain_supplemental_notices(package, source, policy):
    """Copy pinned primary-source notices into both release package inventories."""
    for name, record in policy["supplemental_notices"].items():
        original, retained = source / record["source"], package / record["destination"]
        if not original.is_file() or sha256(original) != record["sha256"] or retained.exists():
            raise ValueError("supplemental notice source/output differs: " + name)
        retained.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(original, retained)
        if sha256(retained) != record["sha256"]:
            raise ValueError("supplemental notice copy differs: " + name)
    runtime_path = package / "inventory/runtime-inventory.json"
    runtime = json.loads(runtime_path.read_text())
    tools = [item for item in runtime["python"] if canonical(item["name"]) == "grpcio-tools"]
    if len(tools) != 1:
        raise ValueError("exactly one installed grpcio-tools distribution required")
    record = policy["supplemental_notices"]["grpcio-tools"]
    tools[0]["documents"][record["destination"].removeprefix("inventory/")] = record["sha256"]
    tools[0]["supplemental_notice_origin"] = {"url": record["source_url"], "sha256": record["sha256"]}
    dump(runtime_path, runtime)


def debian_source(item):
    return "https://sources.debian.org/src/" + quote(item["source"], safe="") + "/" + quote(item["source_version"], safe="") + "/"


def go_dependencies(info):
    result = {}
    for line in info.splitlines():
        fields = line.strip().split("\t")
        if fields[0] == "dep" and len(fields) >= 3:
            result[fields[1]] = fields[2]
    return result


def closure(package, source, policy, server, postgres):
    errors, components = [], []
    provenance = json.loads((package / "provenance.json").read_text())
    versions = json.loads((package / "versions.json").read_text())
    inventory = package / "inventory"
    runtime = json.loads((inventory / "runtime-inventory.json").read_text())
    modules = {m["Path"]: m for m in json_stream((inventory / "go-modules.json").read_text())}
    expected_go = {}
    for name in policy["required_binaries"]:
        if name not in provenance["binaries"] or not (package / name).is_file():
            errors.append("required binary absent: " + name)
            continue
        binary = provenance["binaries"][name]
        if sha256(package / name) != binary["sha256"]:
            errors.append("binary changed: " + name)
        for module, version in go_dependencies(binary["go_build_info"]).items():
            if module in expected_go and expected_go[module] != version:
                errors.append("conflicting embedded Go versions: " + module)
            expected_go[module] = version
    for relative in policy["required_product_documents"]:
        if not (inventory / relative).is_file() or not (inventory / relative).stat().st_size:
            errors.append("product notice missing: " + relative)
    for module, version in sorted(expected_go.items()):
        declared = modules.get(module, {})
        docs = documents(inventory, "licenses/go/" + module + "@" + version)
        require_documents(inventory, docs, "Go " + module, errors)
        if declared.get("Version") != version:
            errors.append("Go embedded dependency not in build module inventory: " + module + "@" + version)
        if "Replace" in declared:
            replacement = policy["local_go_replacements"].get(module)
            if not replacement or declared["Replace"].get("Path") != "./" + replacement["path"]:
                errors.append("unqualified Go replacement: " + module)
            else:
                for key in ("provenance", "license", "notice"):
                    path = source / replacement[key]
                    if not path.is_file() or not path.stat().st_size:
                        errors.append("Go replacement " + module + ": missing " + key)
                # Upstream/local hashes record the fork changes. A stale record
                # is an explicit failure; it is never regenerated during release.
                record = json.loads((source / replacement["provenance"]).read_text())
                for entry in record.get("files", []):
                    path = source / replacement["path"] / entry["path"]
                    if sha256(path) != entry["local_sha256"]:
                        errors.append("stale local fork provenance: " + str(path.relative_to(source)))
            origin = {"module": module, "version": version, "replacement": declared["Replace"], "policy": replacement}
        else:
            if not declared.get("Sum", "").startswith("h1:"):
                errors.append("Go module lacks verified module checksum: " + module)
            origin = {"module": module, "version": version, "sum": declared.get("Sum")}
        components.append({"kind": "go", "name": module, "version": version, "origin": origin, "documents": docs})
    # All installed Debian packages are delivered, even when unused by homed.
    for name, item in runtime["debian"].items():
        require_documents(inventory, item["documents"], "runtime Debian " + name, errors)
        components.append({"kind": "deb", "name": name, "version": item["version"],
                           "source": debian_source(item), "documents": item["documents"]})
    for path, item in runtime["debian_files"].items():
        if item["owner"] not in runtime["debian"] or not re.fullmatch(r"[a-f0-9]{64}", item["sha256"]):
            errors.append("OS file lacks actual package ownership/hash: " + path)
    interpreter = runtime["python_interpreter"]
    require_documents(inventory, interpreter["documents"], "Python interpreter", errors)
    python_image = next(i for i in versions["images"] if i["name"] == "python")
    if interpreter["version"] != python_image["tag"].split("-")[0]:
        errors.append("Python interpreter differs from pinned source image version")
    components.append({"kind": "python-interpreter", "name": "python", "version": interpreter["version"],
                       "origin": python_image, "documents": interpreter["documents"], "files": interpreter["files"]})
    installed = json.loads((inventory / "python-install.json").read_text())
    wheel_origins = {canonical(item["metadata"]["name"]): item["download_info"] for item in installed["install"]}
    pinned = {canonical(w["requirement"].split("==")[0]): w for w in versions["worker"]["wheels"]}
    python = {canonical(p["name"]): p for p in runtime["python"]}
    for name, item in python.items():
        require_documents(inventory, item["documents"], "Python " + name, errors)
        if name == "grpcio-tools" and "grpcio-tools" in policy.get("supplemental_notices", {}):
            record = policy["supplemental_notices"]["grpcio-tools"]
            notice_document(package, source, policy, "grpcio-tools", errors)
            relative = record["destination"].removeprefix("inventory/")
            if (item["documents"].get(relative) != record["sha256"] or
                    item.get("supplemental_notice_origin") != {"url": record["source_url"], "sha256": record["sha256"]} or
                    "grpcio" not in python or python["grpcio"]["version"] != item["version"] or
                    item.get("license") != "Apache License 2.0"):
                errors.append("grpcio-tools notice not tied to same-version upstream gRPC wheel")
        origin = wheel_origins.get(name)
        if name in pinned:
            expected = pinned[name]
            expected_version = expected["requirement"].split("==")[1]
            actual_sha = (origin or {}).get("archive_info", {}).get("hashes", {}).get("sha256")
            if item["version"] != expected_version or actual_sha not in {a["sha256"] for a in expected["artifacts"]}:
                errors.append("Python wheel lacks pinned matching source/hash: " + name)
        elif name == "pip":
            origin = {"source_image": next(i for i in versions["images"] if i["name"] == "python")}
        else:
            errors.append("unqualified Python distribution: " + name + "@" + item["version"])
        components.append({"kind": "python", "name": name, "version": item["version"],
                           "origin": origin, "documents": item["documents"], "native_files": item["native_files"]})
    if not pinned.keys() <= python.keys():
        errors.append("pinned worker distribution missing from actual runtime")
    donor = {}
    for line in (inventory / "donor-packages.tsv").read_text().splitlines():
        name, version, upstream, upstream_version, arch = line.split("\t")
        donor[name] = {"name": name, "version": version, "source": upstream or name.split(":")[0],
                       "source_version": upstream_version or version, "architecture": arch}
    copies = {}
    for line in (inventory / "donor-copies.tsv").read_text().splitlines():
        destination, original, digest, name = line.split("\t")
        docs = documents(inventory, "licenses/postgres/" + name)
        require_documents(inventory, docs, "copied157 " + destination, errors)
        relative = str(Path(destination).relative_to("/opt/homeward/pg-client"))
        if provenance["postgres_clients"]["files_sha256"].get(relative) != digest or name not in donor:
            errors.append("copied157 file lacks matching donor owner/hash: " + destination)
        copies[destination] = {"source_path": original, "sha256": digest, "owner": name, "documents": docs}
    expected_copies = {"/opt/homeward/pg-client/" + name for name in provenance["postgres_clients"]["files_sha256"]
                       if name.startswith("lib/") or name.endswith(".real")}
    if copies.keys() != expected_copies:
        errors.append("copied157 closure does not match every actual client ELF/library")
    # PostgreSQL is independently delivered by Compose. Keep all its packages,
    # source versions, copyright and notices, not just the client subset.
    for name, item in donor.items():
        docs = documents(package, "postgres-doc/" + name.split(":")[0])
        require_documents(package, docs, "PostgreSQL image Debian " + name, errors)
        components.append({"kind": "postgres-deb", "name": name, "version": item["version"],
                           "source": debian_source(item), "documents": docs})
    seen_go, seen_python, seen_runtime_deb, seen_postgres_deb = set(), set(), set(), set()
    gosu_policy = policy.get("postgres_gosu_go", {})
    gosu_modules = gosu_policy.get("modules", {})
    gosu_image = None
    if any(a.get("name") in gosu_modules and a.get("type") == "go-module" for a in postgres.get("artifacts", [])):
        input_path, archive = package / "postgres-input.json", package.parent / "postgres-image.tar"
        if input_path.is_file() and archive.is_file():
            gosu_image = json.loads(input_path.read_text())
            if (gosu_image.get("source") != gosu_policy["image_source"] or
                    gosu_image.get("image_id") != gosu_policy["image_id"] or
                    gosu_image.get("platform") != "linux/amd64" or
                    sha256(archive) != gosu_image.get("archive_sha256")):
                errors.append("gosu donor image source/platform/archive differs")
        else:
            errors.append("gosu donor image/archive provenance missing")
    for image_name, document in (("server", server), ("postgres", postgres)):
        artifacts = document.get("artifacts", [])
        if not artifacts:
            errors.append("scanner produced no packages for " + image_name)
        for artifact in artifacts:
            name, version, kind = artifact.get("name", ""), artifact.get("version", ""), artifact.get("type", "")
            locations = {location.get("path", "") for location in artifact.get("locations", [])}
            matched = False
            if kind == "go-module":
                if name in expected_go and version == expected_go[name]:
                    seen_go.add(name); matched = True
                elif (image_name == "server" and version == "UNKNOWN" and name in expected_go and
                      name in policy["local_go_replacements"] and name in modules and
                      "Replace" in modules[name] and locations):
                    # Syft cannot infer a version for local Go replacements.
                    # Every reported binary must itself embed this exact module
                    # and version; the replacement tree/notices were checked above.
                    embedded = {"/usr/local/bin/" + binary for binary, record in provenance["binaries"].items()
                                if go_dependencies(record["go_build_info"]).get(name) == expected_go[name]}
                    replacement = policy["local_go_replacements"][name]
                    record = json.loads((source / replacement["provenance"]).read_text())
                    licenses = {entry.get("value") for entry in artifact.get("licenses", [])}
                    if (locations <= embedded and record.get("license") in licenses and
                            artifact.get("metadata", {}).get("goCompiledVersion") == "go" + versions["go"]["version"] and
                            artifact.get("metadata", {}).get("mainModule") == "github.com/damione1/Homeward"):
                        seen_go.add(name); matched = True
                        components.append({"kind": "local-go-replacement-scanner", "name": name,
                                           "embedded_version": expected_go[name], "scanner_version": version,
                                           "binary_locations": sorted(locations), "provenance": replacement["provenance"]})
                elif name == "stdlib" and version in (versions["go"]["version"], "go" + versions["go"]["version"]):
                    matched = True
                elif name == "github.com/damione1/Homeward" and version in ("(devel)", "", "UNKNOWN"):
                    matched = True  # exact product commit/notices required above
                elif image_name == "postgres" and name in gosu_modules:
                    expected = gosu_modules[name]
                    metadata = artifact.get("metadata", {})
                    notice = expected["notice"]
                    docs = notice_document(package, source, policy, notice, errors)
                    require_documents(package, docs, "PostgreSQL gosu " + name, errors)
                    if (gosu_image is not None and version == expected["version"] and
                            locations == {gosu_policy["binary_location"]} and
                            metadata.get("goCompiledVersion") == gosu_policy["go_compiled_version"] and
                            metadata.get("mainModule") == (None if name == "stdlib" else "github.com/tianon/gosu") and
                            (name == "stdlib" or metadata.get("architecture") == "amd64") and
                            ("h1" not in expected or metadata.get("h1Digest") == expected["h1"])):
                        matched = True
                        components.append({"kind": "postgres-gosu-go", "name": name, "version": version,
                                           "donor_image": gosu_image, "documents": docs,
                                           "go_build_metadata": {key: metadata.get(key) for key in
                                                                 ("goCompiledVersion", "architecture", "h1Digest", "mainModule")}})
            elif kind == "python" and image_name == "server":
                key = canonical(name)
                if key in python and python[key]["version"] == version:
                    seen_python.add(key); matched = True
            elif kind == "deb":
                inventory_packages = runtime["debian"] if image_name == "server" else donor
                candidates = [n for n, p in inventory_packages.items() if n.split(":")[0] == name and p["version"] == version]
                if candidates:
                    (seen_runtime_deb if image_name == "server" else seen_postgres_deb).update(candidates)
                    matched = True
            if not matched and image_name == "server":
                # Exact Windows launchers are delivered as members of the
                # pinned pip/setuptools wheels, even in this Linux image.
                launcher_specs = versions.get("bundled_launcher_wheels", {})
                for owner, spec in launcher_specs.items():
                    for member, digest in spec["launchers_sha256"].items():
                        path = "/usr/local/lib/python3.13/site-packages/" + member
                        expected_name = "Simple Launcher" if owner == "pip" else Path(member).stem
                        expected_version = "1.1.0.14" if owner == "pip" else "UNKNOWN"
                        if (kind == "binary" and name == expected_name and version == expected_version and
                                locations == {path} and owner in python and python[owner]["version"] == spec["version"] and
                                python[owner]["files"].get(path) == digest and
                                re.fullmatch(r"[a-f0-9]{64}", spec["wheel_sha256"]) and
                                (owner == "pip" or any(a["sha256"] == spec["wheel_sha256"] for a in pinned[owner]["artifacts"]))):
                            matched = True
                            components.append({"kind": "wheel-launcher", "name": name, "version": version,
                                               "path": path, "file_sha256": digest, "owning_wheel": spec["wheel_filename"],
                                               "wheel_sha256": spec["wheel_sha256"], "wheel_source": spec["wheel_source"],
                                               "documents": python[owner]["documents"]})
                            break
                    if matched:
                        break
            if not matched and image_name == "server":
                # Native wheel internals are qualified by their actual owning
                # wheel, its pinned hash and the complete retained vendor notices.
                wheel_owners = [key for key, p in python.items() if locations & set(p["files"])]
                licenses = [entry.get("value", "") for entry in artifact.get("licenses", [])]
                resolved = bool(licenses) and all(value and value.upper() not in ("UNKNOWN", "NOASSERTION", "NONE") for value in licenses)
                if wheel_owners and resolved:
                    matched = True
                    components.append({"kind": "wheel-native-component", "name": name, "version": version,
                                       "licenses": licenses, "owning_wheels": wheel_owners,
                                       "documents": {key: python[key]["documents"] for key in wheel_owners}})
                elif locations & copies.keys():
                    matched = True
                elif locations & runtime["debian_files"].keys():
                    owners = {runtime["debian_files"][p]["owner"] for p in locations & runtime["debian_files"].keys()}
                    matched = True
                    components.append({"kind": "os-file-component", "name": name, "version": version,
                                       "owners": sorted(owners), "files": {p: runtime["debian_files"][p] for p in locations & runtime["debian_files"].keys()}})
                elif locations & interpreter["files"].keys() and name in ("python", "Python") and version == interpreter["version"]:
                    matched = True
            if not matched:
                errors.append("unresolved scanner component: " + image_name + ": " + kind + ": " + name + "@" + version + ": " + ",".join(sorted(locations)))
    for label, expected, actual in (("Go", set(expected_go), seen_go), ("Python", set(python), seen_python),
                                    ("runtime Debian", set(runtime["debian"]), seen_runtime_deb),
                                    ("PostgreSQL Debian", set(donor), seen_postgres_deb)):
        for missing in sorted(expected - actual):
            errors.append("scanner closure missing " + label + " component: " + missing)
    return {"schema_version": 1, "status": "FAIL" if errors else "PASS", "source_commit": provenance["source_commit"],
            "errors": errors, "components": components, "copied157": copies,
            "native_wheel_files": {name: p["native_files"] for name, p in python.items()}}


def sandbox_command(scanner):
    # Image archives need no Docker socket and no network. Fail closed on hosts
    # without an available network isolation mechanism; never silently run online.
    if sys.platform == "darwin" and Path("/usr/bin/sandbox-exec").exists():
        return ["/usr/bin/sandbox-exec", "-p", "(version 1)(allow default)(deny network*)", str(scanner)]
    if sys.platform.startswith("linux") and shutil.which("unshare"):
        return ["unshare", "--user", "--map-root-user", "--net", str(scanner)]
    raise ValueError("offline scanner requires sandbox-exec(macOS) or permitted unshare network namespace(Linux)")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", type=Path, required=True)
    parser.add_argument("--source-root", type=Path, required=True)
    parser.add_argument("--syft", type=Path)
    parser.add_argument("--validate-only", action="store_true")
    args = parser.parse_args()
    package, source = args.package.resolve(), args.source_root.resolve()
    policy = json.loads((source / "packaging/release-policy.json").read_text())
    provenance = json.loads((package / "provenance.json").read_text())
    versions = json.loads((package / "versions.json").read_text())
    if args.validate_only:
        server, postgres = [json.loads((package / ("sbom-" + name + ".syft.json")).read_text()) for name in ("server", "postgres")]
        result = closure(package, source, policy, server, postgres)
        if result["errors"]:
            raise ValueError("; ".join(result["errors"]))
        return 0
    if args.syft is None:
        raise ValueError("a pinned --syft executable is required for scanning")
    scanner = args.syft.resolve()
    offline = sandbox_command(scanner)
    env = {key: value for key, value in os.environ.items() if not key.startswith("SYFT_")}
    env.update({"SYFT_CHECK_FOR_APP_UPDATE": "false", "SYFT_GOLANG_SEARCH_REMOTE_LICENSES": "false",
                "SYFT_GOLANG_USE_PACKAGES_LIB": "false", "GOPROXY": "off"})
    config = source / "packaging/syft.yaml"
    version = subprocess.check_output([*offline, "--config", str(config), "version", "-o", "json"], env=env, text=True)
    if json.loads(version)["version"].lstrip("v") != policy["scanner"]["version"]:
        raise ValueError("scanner version is not pinned Syft" + policy["scanner"]["version"])
    # Preserve the effective tool configuration and binary fingerprint.
    effective = subprocess.check_output([*offline, "--config", str(config), "config", "--load"], env=env, text=True)
    (package / "scanner-effective.yaml").write_text(effective)
    dump(package / "scanner.json", {"binary_sha256": sha256(scanner), "version": json.loads(version),
         "config_sha256": sha256(config), "effective_config_sha256": sha256(package / "scanner-effective.yaml"),
         "network_policy": "OS sandbox denies all network; source local archives", "isolation_command": offline[:-1]})
    postgres_archive = package.parent / "postgres-image.tar"
    postgres_source = versions["postgres_clients"]["source_image"]
    info = json.loads(execute("docker", "image", "inspect", postgres_source))[0]
    actual = info["Os"] + "/" + info["Architecture"]
    if actual != provenance["requested_platform"]:
        raise ValueError("local pinned PostgreSQL image platform differs; pull explicit platform under ROOT admission")
    if not postgres_archive.exists():
        subprocess.run(["docker", "image", "save", "--output", str(postgres_archive), info["Id"]], check=True)
    dump(package / "postgres-input.json", {"source": postgres_source, "image_id": info["Id"], "platform": actual,
                                          "archive_sha256": sha256(postgres_archive)})
    container = execute("docker", "create", "--platform", actual, "--entrypoint", "/bin/true", info["Id"])
    try:
        subprocess.run(["docker", "cp", container + ":/usr/share/doc", str(package / "postgres-doc")], check=True)
        subprocess.run(["docker", "cp", container + ":/usr/share/common-licenses", str(package / "postgres-common-licenses")], check=True)
    finally:
        execute("docker", "rm", "--volumes", container)
    retain_supplemental_notices(package, source, policy)
    for label, archive in (("server", package / "server-image.tar"), ("postgres", postgres_archive)):
        subprocess.run([*offline, "--config", str(config), "scan", "docker-archive:" + str(archive),
                        "--output", "syft-json=" + str(package / ("sbom-" + label + ".syft.json"))], env=env, check=True)
    server, postgres = [json.loads((package / ("sbom-" + name + ".syft.json")).read_text()) for name in ("server", "postgres")]
    result = closure(package, source, policy, server, postgres)
    result["artifacts_sha256"] = {str(p.relative_to(package)): sha256(p) for p in sorted(package.rglob("*"))
                                  if p.is_file() and p.name != "closure.json"}
    dump(package / "closure.json", result)
    if result["errors"]:
        for error in result["errors"]:
            print("release provenance blocked: " + error, file=sys.stderr)
        return 1
    print(package / "closure.json")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        print("release SBOM blocked: " + str(error), file=sys.stderr)
        raise SystemExit(1)
