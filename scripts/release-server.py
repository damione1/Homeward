#!/usr/bin/env python3
"""Local release gate. Never publishes, deploys, contacts devices or adds CI."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile

ROOT = Path(__file__).resolve().parents[1]


def sha256(path):
    value = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def write_json(path, value):
    Path(path).write_text(json.dumps(value, sort_keys=True, indent=2) + "\n")


def run(*args, cwd=ROOT, capture=True):
    return subprocess.run(args, cwd=cwd, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def exact_source(commit, output):
    commit = run("git", "rev-parse", "--verify", commit + "^{commit}").strip()
    tree = run("git", "rev-parse", commit + "^{tree}").strip()
    epoch = int(run("git", "show", "-s", "--format=%ct", commit))
    archive = output / "source.tar"
    with archive.open("xb") as stream:
        subprocess.run(["git", "archive", "--format=tar", commit], cwd=ROOT, check=True, stdout=stream)
    files = {}
    with tarfile.open(archive) as stream:
        for member in stream:
            path = Path(member.name)
            if path.is_absolute() or ".." in path.parts or not (member.isfile() or member.isdir()):
                raise ValueError("source archive contains an unsafe/nonregular member: " + member.name)
            if member.isfile():
                files[member.name] = hashlib.sha256(stream.extractfile(member).read()).hexdigest()
    result = {"schema_version": 1, "commit": commit, "tree": tree, "source_date_epoch": epoch,
              "archive_sha256": sha256(archive), "files_sha256": files}
    write_json(output / "source.json", result)
    return result


def unpack_source(archive, output):
    output.mkdir(mode=0o700)
    with tarfile.open(archive) as stream:
        # exact_source already rejects links, special files and traversal.
        stream.extractall(output, filter="data")


def json_differences(left, right, path="$"):
    if type(left) is not type(right):
        return [{"path": path, "first": left, "second": right}]
    if isinstance(left, dict):
        result = []
        for key in sorted(left.keys() | right.keys()):
            child = path + "/" + key.replace("~", "~0").replace("/", "~1")
            if key not in left or key not in right:
                result.append({"path": child, "first_present": key in left,
                               "second_present": key in right, "first": left.get(key), "second": right.get(key)})
            else:
                result.extend(json_differences(left[key], right[key], child))
        return result
    if isinstance(left, list):
        result = []
        for index in range(max(len(left), len(right))):
            if index >= len(left) or index >= len(right):
                result.append({"path": path + "/" + str(index), "first_present": index < len(left),
                               "second_present": index < len(right),
                               "first": left[index] if index < len(left) else None,
                               "second": right[index] if index < len(right) else None})
            else:
                result.extend(json_differences(left[index], right[index], path + "/" + str(index)))
        return result
    return [] if left == right else [{"path": path, "first": left, "second": right}]


def _tar_inventory(stream, *, include_json=False):
    result = {}
    for index, member in enumerate(stream):
        record = {"mode": member.mode, "uid": member.uid, "gid": member.gid, "mtime": member.mtime,
                  "uname": member.uname, "gname": member.gname, "type": member.type.decode("ascii"),
                  "size": member.size, "linkname": member.linkname, "pax_headers": member.pax_headers,
                  "entry_index": index}
        if member.isfile():
            digest = hashlib.sha256()
            small = bytearray() if include_json and member.size < 16 * 1024 * 1024 else None
            # Keep each stream open while hashing; no extraction or rewriting.
            with stream.extractfile(member) as content:
                for chunk in iter(lambda: content.read(1024 * 1024), b""):
                    digest.update(chunk)
                    if small is not None:
                        small.extend(chunk)
            record["sha256"] = digest.hexdigest()
            if small is not None and small.lstrip()[:1] in (b"{", b"["):
                try:
                    record["raw_json"] = json.loads(small)
                except (UnicodeError, ValueError):
                    pass
        key = member.name
        while key in result:
            key = "@entry" + str(index) + "/" + key
        record["path"] = member.name
        result[key] = record
    return result


def tar_inventory(path):
    with tarfile.open(path) as stream:
        return _tar_inventory(stream, include_json=True)


def changed_layer_fields(first, second, raw_images):
    """Inspect differing raw layer archives; expose internal mtimes/content/order."""
    result = {}
    with tarfile.open(first) as a, tarfile.open(second) as b:
        try:
            manifests = [json.load(stream.extractfile("manifest.json")) for stream in (a, b)]
        except KeyError:
            raise ValueError("saved Docker image lacks its raw manifest.json")
        if len(manifests[0]) != 1 or len(manifests[1]) != 1:
            raise ValueError("release image archive must describe exactly one image")
        configs = [json.load(stream.extractfile(manifest[0]["Config"])) for stream, manifest in zip((a, b), manifests)]
        config_fields = json_differences(*configs)
        layers = [manifest[0]["Layers"] for manifest in manifests]
        for index in range(max(len(layers[0]), len(layers[1]))):
            names = [values[index] if index < len(values) else None for values in layers]
            hashes = [raw_images[i][name]["sha256"] if name else None for i, name in enumerate(names)]
            if hashes[0] == hashes[1]:
                continue
            values = []
            for stream, name in zip((a, b), names):
                if name is None:
                    values.append(None)
                else:
                    with stream.extractfile(name) as body:
                        with tarfile.open(fileobj=body, mode="r|*") as layer:
                            values.append(_tar_inventory(layer))
            result[str(index)] = {"first_layer": names[0], "second_layer": names[1],
                                  "first_sha256": hashes[0], "second_sha256": hashes[1],
                                  "raw_fields": json_differences(*values)}
    return {"config_fields": config_fields, "layers": result}


def compare(output):
    first, second = output / "first", output / "second"
    a, b = [json.loads((directory / "provenance.json").read_text()) for directory in (first, second)]
    for field in ("source_commit", "source_tree", "source_files_sha256", "requested_platform", "source_date_epoch"):
        if a[field] != b[field]:
            raise ValueError("builds do not share the same input: " + field)
    if not all(p["exact_commit_archive"] and p["clean_build"] for p in (a, b)):
        raise ValueError("two exact-commit no-cache builds are required")
    names = list(a["binaries"]) + ["server-image.tar", "sbom-server.syft.json", "sbom-postgres.syft.json"]
    hashes = {name: {"first": sha256(first / name) if (first / name).is_file() else None,
                     "second": sha256(second / name) if (second / name).is_file() else None} for name in names}
    differences = {}
    for name in ("image-inspect.json", "sbom-server.syft.json", "sbom-postgres.syft.json", "inventory/runtime-inventory.json"):
        values = [json.loads((directory / name).read_text()) if (directory / name).is_file() else None for directory in (first, second)]
        differences[name] = json_differences(*values)
    raw_images = [tar_inventory(directory / "server-image.tar") for directory in (first, second)]
    differences["raw_image_tar_members"] = json_differences(*raw_images)
    differences["raw_layer_fields"] = changed_layer_fields(first / "server-image.tar", second / "server-image.tar", raw_images)
    # Inspect carries raw image config digest + RootFS diff IDs. The tar member
    # inventory also covers saved config, manifests, compressed layers and metadata.
    result = {"schema_version": 1, "source_commit": a["source_commit"], "platform": a["requested_platform"],
              "hashes": hashes, "raw_json_differences": differences,
              "bit_identical": all(v["first"] is not None and v["first"] == v["second"] for v in hashes.values()),
              "payload_bit_identical": all(hashes[n]["first"] == hashes[n]["second"] for n in list(a["binaries"]) + ["server-image.tar"]),
              "comparison": "Raw bytes and every differing JSON/tar field retained; no normalization applied."}
    write_json(output / "comparison.json", result)
    return result


def verify_evidence(evidence, source, root):
    """Require source-bound logs. A user-authorized165 waiver keeps its raw failure."""
    if evidence.get("schema_version") != 1 or evidence.get("candidate_commit") != source["commit"]:
        raise ValueError("evidence does not identify the exact candidate commit")
    checks = evidence.get("checks", {})
    required = {"make-check", "protobuf-breaking-negative", "nha165-software"}
    if set(checks) != required:
        raise ValueError("required evidence checks are missing or unknown")
    verified_roles = {}
    for name, check in checks.items():
        files = check.get("source_files_sha256", {})
        # Qualified165 may precede collector/packaging-only changes. Explicit
        # source exclusions must be reviewed and recorded, never guessed here.
        excluded = check.get("excluded_candidate_files", {})
        if not files or files.keys() & excluded.keys() or files.keys() | excluded.keys() != source["files_sha256"].keys():
            raise ValueError("evidence must account for every candidate source path: " + name)
        for path, digest in files.items():
            if source["files_sha256"].get(path) != digest:
                raise ValueError("stale evidence source: " + name + ": " + path)
        if excluded and not check.get("scope_review", {}).get("reviewer"):
            raise ValueError("source exclusions need an explicit recorded scope review: " + name)
        for path, reason in excluded.items():
            if not isinstance(reason, str) or not reason.strip():
                raise ValueError("source exclusion has no reviewed reason: " + path)
        artifacts = check.get("artifacts", [])
        if not artifacts or not check.get("command"):
            raise ValueError("missing evidence command or artifacts: " + name)
        for artifact in artifacts:
            path = root / artifact["path"]
            if not path.is_file() or sha256(path) != artifact["sha256"]:
                raise ValueError("missing or changed evidence artifact: " + name)
        roles = {artifact.get("role"): root / artifact["path"] for artifact in artifacts if artifact.get("role")}
        verified_roles[name] = roles
        if "source-manifest" not in roles:
            raise ValueError("raw source manifest missing: " + name)
        raw_sources = json.loads(roles["source-manifest"].read_text())
        raw_files = raw_sources.get("files_sha256", raw_sources)
        raw_files = {path: item.get("sha256") if isinstance(item, dict) else item
                     for path, item in raw_files.items()}
        if any(raw_files.get(path) != digest for path, digest in files.items()):
            raise ValueError("evidence source claims differ from raw source manifest: " + name)
        if excluded:
            if "scope-review" not in roles:
                raise ValueError("hashed source delta review missing: " + name)
            review = json.loads(roles["scope-review"].read_text())
            if (review.get("decision") != "APPROVED_SOURCE_DELTA" or review.get("candidate_commit") != source["commit"]
                    or review.get("excluded_candidate_files") != excluded):
                raise ValueError("source delta review does not approve the exact candidate delta: " + name)
        if name == "make-check":
            if "runner-result" not in roles or "transcript" not in roles:
                raise ValueError("raw make check runner result/transcript missing")
            result = json.loads(roles["runner-result"].read_text())
            jobs = list(result.get("jobs", {}).values())
            passed = [j for j in jobs if j.get("argv") == ["make", "check"] and j.get("payload_exit_code") == 0
                      and j.get("payload_terminal") is True and j.get("status") == "passed" and not j.get("cohort_errors")
                      and j.get("source_manifest", {}).get("sha256") == sha256(roles["source-manifest"])]
            if not result.get("all_jobs_passed") or not result.get("cohort_audit_passed") or not passed:
                raise ValueError("raw make check runner result is not a completed audited PASS on this source manifest")
        if name == "protobuf-breaking-negative":
            if "transcript" not in roles or "negative compatibility probe rejected unreserved deletion of CommandRequest.target #4" not in roles["transcript"].read_text():
                raise ValueError("actual existing Buf breaking-negative rejection is absent")
        if name == "nha165-software" and check.get("outcome") == "ACCEPTED_EXPLICIT_USER_DURATION_EXCEPTION":
            waiver = check.get("duration_exception", {})
            if (check.get("raw_status") != "FAILED" or check.get("exit_code") == 0
                    or not waiver.get("user_authorization_artifact") or not waiver.get("observed_seconds")
                    or not waiver.get("unmeasured_seconds") or not waiver.get("limits")):
                raise ValueError("incomplete165 duration exception; raw FAILED must remain visible")
            referenced = {a["path"] for a in artifacts}
            if waiver["user_authorization_artifact"] not in referenced:
                raise ValueError("165 exception authorization is not hashed evidence")
            if not {"raw-result", "duration-analysis", "authorization"} <= roles.keys():
                raise ValueError("165 raw failed result, measured analysis or authorization missing")
            result = json.loads(roles["raw-result"].read_text())
            analysis = json.loads(roles["duration-analysis"].read_text())
            authorization = json.loads(roles["authorization"].read_text())
            if (str(result.get("status", "")).upper() != "FAILED" or authorization.get("status") != "ROOT_APPROVED_TO_COMMIT_WITH_EXPLICIT_USER_DURATION_EXCEPTION"
                    or analysis.get("original_failed_result_sha256") != sha256(roles["raw-result"])
                    or analysis.get("status") != "MEASURED_PREFIX_THRESHOLDS_PASS_REQUIRES_EXPLICIT_USER_DURATION_EXCEPTION"
                    or not analysis.get("all_exported_core_observations_received")):
                raise ValueError("165 waiver artifacts do not establish accepted measured prefix")
            authorized_hashes = set(authorization.get("refs", {}).values())
            if not {sha256(roles[role]) for role in ("source-manifest", "raw-result", "duration-analysis")} <= authorized_hashes:
                raise ValueError("165 authorization does not bind the exact raw source/result/analysis artifacts")
            if any(raw_files.get(path) != digest for path, digest in authorization.get("owned_sources", {}).items()):
                raise ValueError("165 authorization source claims differ from its actual consumed source manifest")
        elif name == "nha165-software":
            if "profile-report" not in roles:
                raise ValueError("actual165 acceptance profile report is not PASS")
            profile = json.loads(roles["profile-report"].read_text())
            if (profile.get("status") != "SOFTWARE_PROFILE_PASSED_COVERAGE_AND_FULL_GATE_REQUIRED"
                    or profile.get("profile", {}).get("name") != "acceptance"
                    or check.get("exit_code") != 0 or check.get("outcome") != "PASS"):
                raise ValueError("actual165 acceptance profile report is not PASS")
        elif check.get("exit_code") != 0 or check.get("outcome") != "PASS":
            raise ValueError("required local check is not PASS: " + name)
    for role in ("source-manifest", "transcript"):
        if sha256(verified_roles["make-check"][role]) != sha256(verified_roles["protobuf-breaking-negative"][role]):
            raise ValueError("Buf negative evidence is not from the same make check execution/source")
    return checks


def checksums(output):
    records = {str(path.relative_to(output)): sha256(path) for path in sorted(output.rglob("*"))
               if path.is_file() and path.name not in ("SHA256SUMS", "SHA256SUMS.sha256")
               and not any(part in ("source-first", "source-second") for part in path.relative_to(output).parts)}
    sums = output / "SHA256SUMS"
    sums.write_text("".join(digest + "  " + name + "\n" for name, digest in records.items()))
    (output / "SHA256SUMS.sha256").write_text(sha256(sums) + "  SHA256SUMS\n")
    return records


def build(args):
    output = args.output.resolve()
    if ROOT == output or ROOT in output.parents:
        raise ValueError("release outputs must be outside source checkout")
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    source = exact_source(args.commit, output)
    for name in ("first", "second"):
        directory = output / name
        directory.mkdir(mode=0o700)
        snapshot = output / ("source-" + name)
        unpack_source(output / "source.tar", snapshot)
        image = "homeward-server:nha119-" + args.platform.split("/")[1]
        run(sys.executable, str(snapshot / "scripts/package-server.py"), "--platform", args.platform,
            "--source-root", str(snapshot), "--source-manifest", str(output / "source.json"),
            "--go-tool", str(ROOT / ".tools/bin/go"),
            "--no-cache", "--export-image", "--image", image, "--output", str(directory), capture=False)
        # Keep both raw builds even when license closure is red. AC2's second
        # clean build and comparison must not disappear behind an earlier gate.
        sbom = subprocess.run([sys.executable, str(snapshot / "scripts/release_sbom.py"), "--package", str(directory),
                               "--source-root", str(snapshot), "--syft", str(args.syft)], cwd=ROOT)
        write_json(directory / "sbom-exit.json", {"exit_code": sbom.returncode})
    comparison = compare(output)
    write_json(output / "release-state.json", {"state": "BUILT_NOT_QUALIFIED", "commit": source["commit"],
               "comparison_sha256": sha256(output / "comparison.json"), "bit_identical": comparison["bit_identical"]})


def qualify(args):
    output = args.output.resolve()
    source = json.loads((output / "source.json").read_text())
    if sha256(output / "source.tar") != source["archive_sha256"]:
        raise ValueError("exact commit source archive changed")
    if run("git", "rev-parse", source["commit"] + "^{tree}").strip() != source["tree"]:
        raise ValueError("source tree does not match the recorded Git commit")
    evidence = json.loads(args.evidence.read_text())
    checks = verify_evidence(evidence, source, args.evidence.parent)
    comparison = compare(output)
    for name in ("first", "second"):
        snapshot = output / ("source-" + name)
        actual = {str(p.relative_to(snapshot)): sha256(p) for p in sorted(snapshot.rglob("*")) if p.is_file()}
        if actual != source["files_sha256"]:
            raise ValueError("source snapshot changed after build: " + name)
        closure = json.loads((output / name / "closure.json").read_text())
        if closure.get("status") != "PASS" or closure.get("source_commit") != source["commit"]:
            raise ValueError("SBOM/license closure is not qualified: " + name)
        for path, digest in closure["artifacts_sha256"].items():
            if sha256(output / name / path) != digest:
                raise ValueError("SBOM/license artifact changed: " + name + ": " + path)
        # Recompute policy from raw SBOM/inventories/licenses, using the actual
        # candidate script; a hand-edited status field cannot authorize release.
        run(sys.executable, str(snapshot / "scripts/release_sbom.py"), "--validate-only", "--package", str(output / name),
            "--source-root", str(snapshot), capture=False)
    # Copy evidence and its exact hashed reports into a self-contained bundle.
    proof = output / "evidence"
    proof.mkdir(exist_ok=True)
    shutil.copyfile(args.evidence, proof / "original-manifest.json")
    relocated = json.loads(args.evidence.read_text())
    for name, check in checks.items():
        paths = {}
        for index, artifact in enumerate(check["artifacts"]):
            target = name + "-" + str(index)
            shutil.copyfile(args.evidence.parent / artifact["path"], proof / target)
            paths[artifact["path"]] = target
            relocated["checks"][name]["artifacts"][index]["path"] = target
        if "duration_exception" in check:
            waiver = relocated["checks"][name]["duration_exception"]
            waiver["user_authorization_artifact"] = paths[waiver["user_authorization_artifact"]]
    write_json(proof / "manifest.json", relocated)
    result = {"schema_version": 1, "source_commit": source["commit"], "source_tree": source["tree"],
              "platform": comparison["platform"], "software_release": "QUALIFIED_LOCAL",
              "bit_identical": comparison["bit_identical"], "comparison_sha256": sha256(output / "comparison.json"),
              "nha165": checks["nha165-software"]["outcome"],
              "nha166": "OPEN: independent real continuous24h observation not established by this release",
              "nha167": "OPEN: manual physical acceptance by Damien; no device actions performed",
              "publication": "local artifact bundle only; no network publication or deployment"}
    write_json(output / "release.json", result)
    write_json(output / "release-state.json", {"state": "QUALIFIED_LOCAL", "commit": source["commit"]})
    checksums(output)
    print(output / "release.json")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    builder = commands.add_parser("build")
    builder.add_argument("--commit", required=True)
    builder.add_argument("--platform", choices=("linux/amd64", "linux/arm64"), required=True)
    builder.add_argument("--syft", type=Path, required=True)
    builder.add_argument("--output", type=Path, required=True)
    for action in ("compare", "qualify"):
        sub = commands.add_parser(action)
        sub.add_argument("--output", type=Path, required=True)
        if action == "qualify":
            sub.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.action == "build":
            build(args)
        elif args.action == "compare":
            compare(args.output)
        else:
            qualify(args)
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        print("release blocked: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
