#!/usr/bin/env python3
"""NHA119 policy regressions: isolated real files; no Docker/network/devices."""
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
import sys

sys.dont_write_bytecode = True


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).parent / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


release = load("release119", "release-server.py")
sbom = load("sbom119", "release_sbom.py")
package_builder = load("package119", "package-server.py")


class TestNHA119(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="nha119-policy-")
        self.root = Path(self.temp.name)
        self.package = self.root / "package"
        self.source = self.root / "source"
        self.inventory = self.package / "inventory"
        self.package.mkdir(); self.source.mkdir(); self.inventory.mkdir()

    def tearDown(self):
        self.temp.cleanup()

    def file(self, root, name, data="Synthetic license text and copyright retained for fixture only.\n"):
        path = root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data.encode() if isinstance(data, str) else data)
        return path

    def json(self, root, name, value):
        return self.file(root, name, json.dumps(value))

    def make_closure(self):
        self.policy = json.loads((Path(__file__).parents[1] / "packaging/release-policy.json").read_text())
        for name in self.policy["required_product_documents"]:
            self.file(self.inventory, name)
        self.go = "example.org/qualified"
        self.fork = "github.com/mctofu/homekit"
        dependencies = "\tdep\t" + self.go + "\tv1.0.0\th1:synthetic\n\tdep\t" + self.fork + "\tv1.2.0\n\t=>\t./third_party/homekit\t(devel)\n"
        binaries = {}
        for name in self.policy["required_binaries"]:
            path = self.file(self.package, name, b"synthetic executable " + name.encode())
            binaries[name] = {"sha256": release.sha256(path), "go_build_info": dependencies}
        for module, version in ((self.go, "v1.0.0"), (self.fork, "v1.2.0")):
            self.file(self.inventory, "licenses/go/" + module + "@" + version + "/LICENSE")
        replacement = self.policy["local_go_replacements"][self.fork]
        for key in ("license", "notice"):
            self.file(self.source, replacement[key])
        fork_source = self.file(self.source, replacement["path"] + "/source.go", "package fork\n")
        self.json(self.source, replacement["provenance"], {"files": [{"path": "source.go", "local_sha256": release.sha256(fork_source)}]})
        modules = [{"Path": self.go, "Version": "v1.0.0", "Sum": "h1:synthetic"},
                   {"Path": self.fork, "Version": "v1.2.0", "Replace": {"Path": "./" + replacement["path"]}}]
        self.file(self.inventory, "go-modules.json", "\n".join(json.dumps(m) for m in modules))
        debdoc = "licenses/debian/libc6:amd64/copyright"
        self.file(self.inventory, debdoc)
        pydoc = "licenses/python/grpcio/grpcio.dist-info/LICENSE"
        self.file(self.inventory, pydoc)
        interpreter_doc = "licenses/python-interpreter/LICENSE.txt"
        self.runtime = {"debian": {"libc6:amd64": {"version": "2.36", "source": "glibc", "source_version": "2.36", "documents": {debdoc: release.sha256(self.inventory / debdoc)}}},
                        "debian_files": {"/usr/lib/libc.so.6": {"owner": "libc6:amd64", "sha256": "f" * 64}},
                        "python_interpreter": {"version": "3.13.17", "files": {"/usr/local/bin/python3.13": "e" * 64},
                                               "documents": {interpreter_doc: release.sha256(self.inventory / interpreter_doc)}},
                        "python": [{"name": "grpcio", "version": "1.0", "documents": {pydoc: release.sha256(self.inventory / pydoc)},
                                    "native_files": {"/usr/local/lib/python3.13/site-packages/grpc/native.so": "synthetic"},
                                    "files": {"/usr/local/lib/python3.13/site-packages/grpc/native.so": "synthetic"}}]}
        self.json(self.inventory, "runtime-inventory.json", self.runtime)
        self.file(self.inventory, "donor-packages.tsv", "postgresql-client-17\t17.10\tpostgresql-17\t17.10\tamd64\n")
        copies = {"bin/pg_dump.real": "dump-hash", "bin/pg_restore.real": "restore-hash", "lib/ld-linux.so": "loader-hash"}
        self.file(self.inventory, "donor-copies.tsv", "".join("/opt/homeward/pg-client/" + path + "\t/donor/" + path + "\t" + digest + "\tpostgresql-client-17\n" for path, digest in copies.items()))
        self.file(self.inventory, "licenses/postgres/postgresql-client-17/copyright")
        self.file(self.package, "postgres-doc/postgresql-client-17/copyright")
        self.json(self.inventory, "python-install.json", {"install": [{"metadata": {"name": "grpcio"}, "download_info": {"url": "https://example.invalid/fixture.whl", "archive_info": {"hashes": {"sha256": "wheel-hash"}}}}]})
        self.json(self.package, "versions.json", {"go": {"version": "1.27.2"}, "images": [{"name": "python", "tag": "3.13.17-slim-bookworm"}], "worker": {"wheels": [{"requirement": "grpcio==1.0", "artifacts": [{"sha256": "wheel-hash"}]}]}})
        self.json(self.package, "provenance.json", {"source_commit": "c" * 40, "binaries": binaries,
                                                    "postgres_clients": {"files_sha256": copies}})
        artifact = lambda name, version, kind: {"name": name, "version": version, "type": kind, "locations": []}
        self.server = {"artifacts": [artifact(self.go, "v1.0.0", "go-module"), artifact(self.fork, "v1.2.0", "go-module"),
                                     artifact("libc6", "2.36", "deb"), artifact("grpcio", "1.0", "python")]}
        self.postgres = {"artifacts": [artifact("postgresql-client-17", "17.10", "deb")]}

    def result(self):
        return sbom.closure(self.package, self.source, self.policy, self.server, self.postgres)

    def test_closed_inventory_passes_and_retains_copied_native_notices(self):
        self.make_closure()
        result = self.result()
        self.assertEqual([], result["errors"])
        self.assertEqual(3, len(result["copied157"]))
        self.assertTrue(result["native_wheel_files"]["grpcio"])

    def test_unknown_delivered_dependency_is_red(self):
        self.make_closure()
        self.server["artifacts"].append({"name": "unqualified", "version": "9", "type": "go-module", "locations": []})
        self.assertTrue(any("unresolved scanner component" in e for e in self.result()["errors"]))

    def test_missing_license_is_red(self):
        self.make_closure()
        (self.inventory / "licenses/go" / (self.go + "@v1.0.0") / "LICENSE").unlink()
        self.assertTrue(any("retained license" in e for e in self.result()["errors"]))

    def test_missing_product_notice_is_red(self):
        self.make_closure()
        (self.inventory / "licenses/product/hc-NOTICE").unlink()
        self.assertTrue(any("product notice missing" in e for e in self.result()["errors"]))

    def test_copied157_missing_ownership_mapping_is_red(self):
        self.make_closure()
        path = self.inventory / "donor-copies.tsv"
        path.write_text("\n".join(path.read_text().splitlines()[:-1]) + "\n")
        self.assertTrue(any("every actual client ELF" in e for e in self.result()["errors"]))

    def test_copied157_wrong_bytes_are_red(self):
        self.make_closure()
        path = self.inventory / "donor-copies.tsv"
        path.write_text(path.read_text().replace("dump-hash", "foreign-hash"))
        self.assertTrue(any("matching donor owner/hash" in e for e in self.result()["errors"]))

    def test_stale_fork_provenance_is_red(self):
        self.make_closure()
        self.file(self.source, "third_party/homekit/source.go", "package changed\n")
        self.assertTrue(any("stale local fork" in e for e in self.result()["errors"]))

    def test_unpinned_wheel_is_red(self):
        self.make_closure()
        self.json(self.inventory, "python-install.json", {"install": []})
        self.assertTrue(any("pinned matching source/hash" in e for e in self.result()["errors"]))

    def test_incomplete_scanner_catalogue_is_red(self):
        self.make_closure()
        self.server["artifacts"] = self.server["artifacts"][1:]
        self.assertTrue(any("scanner closure missing Go" in e for e in self.result()["errors"]))

    def test_native_wheel_subcomponent_requires_real_wheel_file_ownership(self):
        self.make_closure()
        component = {"name": "vendor-native", "version": "1", "type": "rust-crate",
                     "licenses": [{"value": "MIT"}], "locations": [{"path": "/usr/local/lib/python3.13/site-packages/grpc/native.so"}]}
        self.server["artifacts"].append(component)
        self.assertEqual([], self.result()["errors"])
        component["locations"][0]["path"] = "/unowned/native.so"
        self.assertTrue(any("vendor-native" in e for e in self.result()["errors"]))

    def test_native_wheel_unknown_license_cannot_hide_behind_owning_wheel(self):
        self.make_closure()
        self.server["artifacts"].append({"name": "unlicensed-native", "version": "1", "type": "rust-crate",
                                         "licenses": [{"value": "NOASSERTION"}],
                                         "locations": [{"path": "/usr/local/lib/python3.13/site-packages/grpc/native.so"}]})
        self.assertTrue(any("unlicensed-native" in e for e in self.result()["errors"]))

    def test_scanned_os_binary_and_python_interpreter_use_actual_file_ownership(self):
        self.make_closure()
        self.server["artifacts"] += [{"name": "libc", "version": "2.36", "type": "binary", "locations": [{"path": "/usr/lib/libc.so.6"}]},
                                     {"name": "python", "version": "3.13.17", "type": "binary", "locations": [{"path": "/usr/local/bin/python3.13"}]}]
        self.assertEqual([], self.result()["errors"])
        self.runtime["debian_files"]["/usr/lib/libc.so.6"]["owner"] = "unqualified-owner"
        self.json(self.inventory, "runtime-inventory.json", self.runtime)
        self.assertTrue(any("ownership/hash" in e for e in self.result()["errors"]))

    def test_foreign_python_interpreter_version_is_red(self):
        self.make_closure()
        self.runtime["python_interpreter"]["version"] = "3.12.0"
        self.json(self.inventory, "runtime-inventory.json", self.runtime)
        self.assertTrue(any("Python interpreter differs" in e for e in self.result()["errors"]))

    def make_evidence(self):
        self.candidate = {"commit": "c" * 40, "files_sha256": {"go.mod": "source-hash"}}
        self.json(self.root, "sources.json", {"go.mod": {"type": "file", "sha256": "source-hash"}})
        self.json(self.root, "runner.json", {"all_jobs_passed": True, "cohort_audit_passed": True,
                 "jobs": {"test": {"argv": ["make", "check"], "payload_exit_code": 0, "payload_terminal": True, "status": "passed", "cohort_errors": [],
                                   "source_manifest": {"sha256": release.sha256(self.root / "sources.json")}}}})
        self.file(self.root, "check.log", "negative compatibility probe rejected unreserved deletion of CommandRequest.target #4\n")
        self.json(self.root, "profile.json", {"status": "SOFTWARE_PROFILE_PASSED_COVERAGE_AND_FULL_GATE_REQUIRED", "profile": {"name": "acceptance"}})
        def artifact(name, role):
            return {"path": name, "role": role, "sha256": release.sha256(self.root / name)}
        common = {"source_files_sha256": {"go.mod": "source-hash"}, "command": ["synthetic-fixture"], "exit_code": 0, "outcome": "PASS"}
        self.evidence = {"schema_version": 1, "candidate_commit": self.candidate["commit"], "checks": {}}
        for name in ("make-check", "protobuf-breaking-negative", "nha165-software"):
            record = copy.deepcopy(common)
            record["artifacts"] = [artifact("sources.json", "source-manifest")]
            self.evidence["checks"][name] = record
        self.evidence["checks"]["make-check"]["artifacts"] += [artifact("runner.json", "runner-result"), artifact("check.log", "transcript")]
        self.evidence["checks"]["protobuf-breaking-negative"]["artifacts"].append(artifact("check.log", "transcript"))
        self.evidence["checks"]["nha165-software"]["artifacts"].append(artifact("profile.json", "profile-report"))

    def test_evidence_links_actual_sources_and_result(self):
        self.make_evidence()
        self.assertEqual(3, len(release.verify_evidence(self.evidence, self.candidate, self.root)))

    def test_stale_evidence_is_red(self):
        self.make_evidence()
        self.candidate["files_sha256"]["go.mod"] = "new-source-hash"
        with self.assertRaisesRegex(ValueError, "stale evidence source"):
            release.verify_evidence(self.evidence, self.candidate, self.root)

    def test_old_runner_result_cannot_be_mixed_with_new_matching_source_claims(self):
        self.make_evidence()
        self.candidate["files_sha256"]["go.mod"] = "new-source-hash"
        self.json(self.root, "sources.json", {"go.mod": {"type": "file", "sha256": "new-source-hash"}})
        for check in self.evidence["checks"].values():
            check["source_files_sha256"]["go.mod"] = "new-source-hash"
            for item in check["artifacts"]:
                if item["role"] == "source-manifest":
                    item["sha256"] = release.sha256(self.root / "sources.json")
        with self.assertRaisesRegex(ValueError, "on this source manifest"):
            release.verify_evidence(self.evidence, self.candidate, self.root)

    def test_raw_failed_check_cannot_be_relabelled_pass(self):
        self.make_evidence()
        self.json(self.root, "runner.json", {"all_jobs_passed": False, "jobs": {}})
        item = next(a for a in self.evidence["checks"]["make-check"]["artifacts"] if a["role"] == "runner-result")
        item["sha256"] = release.sha256(self.root / "runner.json")
        with self.assertRaisesRegex(ValueError, "completed audited PASS"):
            release.verify_evidence(self.evidence, self.candidate, self.root)

    def test_missing_evidence_is_red(self):
        self.make_evidence()
        (self.root / "check.log").unlink()
        with self.assertRaisesRegex(ValueError, "evidence artifact"):
            release.verify_evidence(self.evidence, self.candidate, self.root)

    def test_profile_prefight_cannot_qualify_the_hour(self):
        self.make_evidence()
        self.json(self.root, "profile.json", {"status": "PREFLIGHT_PASSED_NOT_ONE_HOUR_NOT_AC2", "profile": {"name": "preflight"}})
        item = self.evidence["checks"]["nha165-software"]["artifacts"][-1]
        item["sha256"] = release.sha256(self.root / "profile.json")
        with self.assertRaisesRegex(ValueError, "acceptance profile"):
            release.verify_evidence(self.evidence, self.candidate, self.root)

    def make_waiver(self):
        self.make_evidence()
        raw = self.json(self.root, "raw08.json", {"status": "failed", "errors": [{"stage": "payload", "returncode": 143}]})
        self.json(self.root, "analysis08.json", {"status": "MEASURED_PREFIX_THRESHOLDS_PASS_REQUIRES_EXPLICIT_USER_DURATION_EXCEPTION",
                                                 "original_failed_result_sha256": release.sha256(raw), "all_exported_core_observations_received": True})
        self.json(self.root, "authorization.json", {"status": "ROOT_APPROVED_TO_COMMIT_WITH_EXPLICIT_USER_DURATION_EXCEPTION",
                  "owned_sources": {"go.mod": "source-hash"},
                  "refs": {name: release.sha256(self.root / name) for name in ("sources.json", "raw08.json", "analysis08.json")}})
        check = self.evidence["checks"]["nha165-software"]
        check.update(outcome="ACCEPTED_EXPLICIT_USER_DURATION_EXCEPTION", raw_status="FAILED", exit_code=143,
                     duration_exception={"user_authorization_artifact": "authorization.json", "observed_seconds": 3613,
                                         "unmeasured_seconds": 47, "limits": "Final counters and unmeasured burst tail remain unknown."})
        for name, role in (("raw08.json", "raw-result"), ("analysis08.json", "duration-analysis"), ("authorization.json", "authorization")):
            check["artifacts"].append({"path": name, "role": role, "sha256": release.sha256(self.root / name)})

    def test_explicit_duration_exception_retains_raw_failure_and_limits(self):
        self.make_waiver()
        before = (self.root / "raw08.json").read_bytes()
        result = release.verify_evidence(self.evidence, self.candidate, self.root)
        self.assertEqual("FAILED", result["nha165-software"]["raw_status"])
        self.assertEqual(before, (self.root / "raw08.json").read_bytes())
        self.assertEqual(47, result["nha165-software"]["duration_exception"]["unmeasured_seconds"])

    def test_duration_exception_without_hashed_authorization_is_red(self):
        self.make_waiver()
        check = self.evidence["checks"]["nha165-software"]
        check["artifacts"] = [item for item in check["artifacts"] if item["role"] != "authorization"]
        with self.assertRaisesRegex(ValueError, "authorization"):
            release.verify_evidence(self.evidence, self.candidate, self.root)

    def test_duration_authorization_cannot_be_mixed_with_foreign_source_result(self):
        self.make_waiver()
        authorization = self.json(self.root, "authorization.json", {"status": "ROOT_APPROVED_TO_COMMIT_WITH_EXPLICIT_USER_DURATION_EXCEPTION",
                                                                    "refs": {"foreign-source": "f" * 64}})
        for item in self.evidence["checks"]["nha165-software"]["artifacts"]:
            if item["role"] == "authorization":
                item["sha256"] = release.sha256(authorization)
        with self.assertRaisesRegex(ValueError, "does not bind"):
            release.verify_evidence(self.evidence, self.candidate, self.root)

    def test_image_platform_and_elf_arch_are_both_checked(self):
        with self.assertRaisesRegex(ValueError, "image platform mismatch"):
            package_builder.verify_platform({"Os": "linux", "Architecture": "arm64"}, "linux/amd64")
        binary = self.file(self.root, "homed", b"\x7fELF\x02\x01" + b"\0" * 12 + (183).to_bytes(2, "little"))
        with self.assertRaisesRegex(ValueError, "ELF/build-info"):
            package_builder.verify_binary(binary, "GOOS=linux GOARCH=amd64 CGO_ENABLED=0", "linux/amd64")

    def test_raw_tar_timestamp_difference_is_visible(self):
        for name, timestamp in (("first.tar", 1), ("second.tar", 2)):
            with tarfile.open(self.root / name, "w") as archive:
                item = tarfile.TarInfo("layer/file"); item.size = 3; item.mtime = timestamp
                archive.addfile(item, io.BytesIO(b"abc"))
        first, second = [release.tar_inventory(self.root / name) for name in ("first.tar", "second.tar")]
        self.assertEqual(first["layer/file"]["sha256"], second["layer/file"]["sha256"])
        differences = release.json_differences(first, second)
        self.assertEqual("$/layer~1file/mtime", differences[0]["path"])
        self.assertNotEqual(release.sha256(self.root / "first.tar"), release.sha256(self.root / "second.tar"))

    def test_image_config_and_inner_layer_mtime_differences_are_visible(self):
        paths = []
        for name, timestamp in (("first", 1), ("second", 2)):
            layer = io.BytesIO()
            with tarfile.open(fileobj=layer, mode="w") as archive:
                item = tarfile.TarInfo("usr/bin/homed"); item.size = 3; item.mtime = timestamp
                archive.addfile(item, io.BytesIO(b"abc"))
            files = {"layer.tar": layer.getvalue(), "config.json": json.dumps({"created": str(timestamp)}).encode(),
                     "manifest.json": json.dumps([{"Config": "config.json", "Layers": ["layer.tar"]}]).encode()}
            path = self.root / (name + "-image.tar")
            with tarfile.open(path, "w") as archive:
                for filename, content in files.items():
                    item = tarfile.TarInfo(filename); item.size = len(content)
                    archive.addfile(item, io.BytesIO(content))
            paths.append(path)
        inventories = [release.tar_inventory(path) for path in paths]
        result = release.changed_layer_fields(*paths, inventories)
        self.assertEqual("$/created", result["config_fields"][0]["path"])
        self.assertTrue(any(item["path"] == "$/usr~1bin~1homed/mtime" for item in result["layers"]["0"]["raw_fields"]))


if __name__ == "__main__":
    unittest.main(verbosity=2)
