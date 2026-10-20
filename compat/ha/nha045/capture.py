"""Explicit, owned, offline two-runtime qualification. Importing has no effects."""

import argparse
from dataclasses import dataclass
from decimal import Decimal
from hashlib import sha256
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import stat
import subprocess
import sys
import time
import uuid


HERE = Path(__file__).resolve().parent
SOCKET = Path("/Users/damien/.orbstack/run/docker.sock")
DOCKER = Path("/usr/local/bin/docker")
IMAGE = "ghcr.io/home-assistant/home-assistant@sha256:17100609af8bb79bf5390f5d514b9aaeb00d184ce758f37769cd80c450851db7"
CACHE = Path("/Users/damien/Library/Caches/codex-build/homeward")
MODULE_CACHE = Path("/Users/damien/go/pkg/mod")
TOOLS = Path("/Users/damien/Projects/Homeward/.tools")
MAX_OUTPUT = 8 << 20
MEMORY = 512 << 20


class QualificationFailure(RuntimeError):
    pass


def require(condition, detail):
    if not condition:
        raise QualificationFailure(detail)


def encode(value):
    return json.dumps(value, sort_keys=True, allow_nan=False, indent=2).encode() + b"\n"


def load_json(data):
    def unique(items):
        result = {}
        for key, value in items:
            require(key not in result, "duplicate JSON key: " + key)
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=unique,
                      parse_constant=lambda value: (_ for _ in ()).throw(QualificationFailure(value)))


def docker_size(text):
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)\s*(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)", text)
    require(match is not None, "unrecognized Docker storage measurement: " + text)
    units = {"B": 1, "kB": 1000, "MB": 1000**2, "GB": 1000**3, "TB": 1000**4,
             "KiB": 1024, "MiB": 1024**2, "GiB": 1024**3, "TiB": 1024**4}
    return math.ceil(Decimal(match[1]) * units[match[2]])


def validate_envelope(inspected, *, image_id, probe, python):
    host, config, state = inspected["HostConfig"], inspected["Config"], inspected["State"]
    require(inspected["Image"] == image_id and config["Image"] == IMAGE, "image identity drift")
    require(state["Status"] == "created" and not state["Running"], "container was started before admission")
    require(host["NetworkMode"] == "none" and not host.get("PublishAllPorts") and not host.get("PortBindings"), "physical network envelope")
    require(not host.get("Privileged") and host["ReadonlyRootfs"] is True, "writable or privileged root")
    require(host.get("CapDrop") == ["ALL"] and not host.get("CapAdd"), "capabilities not dropped")
    require(set(host.get("SecurityOpt") or []) in ({"no-new-privileges"}, {"no-new-privileges=true"}), "no-new-privileges absent")
    require(host["PidsLimit"] == 128 and host["Memory"] == MEMORY and host["MemorySwap"] == MEMORY, "resource limits drift")
    require(not host.get("Devices") and not host.get("DeviceRequests") and not host.get("ExtraHosts"), "host device or gateway exposure")
    require(host.get("PidMode", "") == "" and host.get("UTSMode", "") == "" and host.get("IpcMode") == "private", "host namespace exposure")
    tmpfs = host.get("Tmpfs") or {}
    require(set(tmpfs) == {"/tmp"}, "unexpected tmpfs")
    flags = set(tmpfs["/tmp"].split(","))
    require({"rw", "nosuid", "nodev", "size=67108864"} <= flags, "tmpfs bounds drift")
    networks = inspected["NetworkSettings"].get("Networks") or {}
    require(set(networks) <= {"none"}, "unexpected attached network")
    for network in networks.values():
        require(not network.get("IPAddress") and not network.get("GlobalIPv6Address") and not network.get("Gateway"), "routed network present")
    mounts = inspected.get("Mounts") or []
    if python:
        require(not mounts and config["Entrypoint"] == ["python3"], "HA worker received host mounts")
    else:
        require(len(mounts) == 1 and config["Entrypoint"] == ["/nha045-probe"], "probe mounts/entrypoint drift")
        mount = mounts[0]
        require(mount["Type"] == "bind" and mount["Source"] == str(probe) and mount["Destination"] == "/nha045-probe" and mount["RW"] is False,
                "probe mount is not the sole immutable artifact")


@dataclass(frozen=True)
class CommandResult:
    stdout: bytes
    code: int
    stderr: bytes = b""


def confirms_container_absence(result, identity):
    """Only Docker 29.4.0's exact reply for this object proves absence."""
    if type(result) is not CommandResult or type(identity) is not str:
        return False
    if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", identity) is None:
        return False
    if type(result.code) is not int or type(result.stdout) is not bytes or type(result.stderr) is not bytes:
        return False
    return (result.code == 1 and result.stdout == b"[]\n"
            and result.stderr == ("Error response from daemon: No such container: " + identity + "\n").encode("ascii"))


class Runner:
    def __init__(self, probe, evidence):
        require(sys.version_info[:3] == (3, 12, 8), "reuse the pinned Python 3.12.8 driver")
        self.lease = os.environ.get("HOMEWARD_NHA045_ROOT_LEASE", "")
        require(bool(re.fullmatch(r"[A-Za-z0-9_-]{8,100}", self.lease)), "root heavy lease absent")
        self.probe = probe.resolve(strict=True)
        require(self.probe.is_file() and self.probe.stat().st_size <= 128 << 20, "invalid probe artifact")
        self.probe_hash = sha256(self.probe.read_bytes()).hexdigest()
        self.evidence = evidence.resolve()
        self.evidence.mkdir(parents=True, exist_ok=False)
        self.counter = 0
        self.deadline = time.monotonic() + 900
        self.socket_identity = None
        self.image_id = None
        self.containers = []
        self.commands = []
        self.budgets = []
        self.source_hashes = {str(path): sha256(path.read_bytes()).hexdigest()
                              for path in sorted(HERE.rglob("*")) if path.is_file() and "__pycache__" not in path.parts}

    def socket_check(self):
        require(os.environ.get("DOCKER_HOST") in (None, "", "unix://" + str(SOCKET)), "non-local DOCKER_HOST override")
        metadata = SOCKET.stat()
        require(stat.S_ISSOCK(metadata.st_mode), "local Unix socket absent")
        identity = (metadata.st_dev, metadata.st_ino)
        require(self.socket_identity is None or identity == self.socket_identity, "Docker endpoint changed")
        self.socket_identity = identity

    def command(self, args, *, payload=None, timeout=30, failure_ok=False, terminal=False):
        self.socket_check()
        remaining = self.deadline - time.monotonic() - (0 if terminal else 60)
        require(remaining > 0, "global qualification deadline reached")
        timeout = min(timeout, remaining)
        self.counter += 1
        prefix = self.evidence / f"command-{self.counter:04d}"
        record = {"argv": [str(value) for value in args], "cwd": str(HERE), "timeout_seconds": timeout,
                  "stdin_sha256": sha256(payload).hexdigest() if payload is not None else None,
                  "started_unix": time.time(), "lease": self.lease}
        prefix.with_suffix(".json").write_bytes(encode(record))
        with prefix.with_suffix(".stdout").open("wb") as stdout, prefix.with_suffix(".stderr").open("wb") as stderr:
            process = subprocess.Popen(record["argv"], cwd=HERE, start_new_session=True,
                                       stdin=subprocess.PIPE if payload is not None else subprocess.DEVNULL,
                                       stdout=stdout, stderr=stderr)
            record.update(pid=process.pid, pgid=process.pid)
            timed_out = False
            overflow = False
            command_deadline = time.monotonic() + timeout
            try:
                first = True
                while True:
                    remaining = command_deadline - time.monotonic()
                    if remaining <= 0:
                        raise subprocess.TimeoutExpired(record["argv"], timeout)
                    try:
                        process.communicate(input=payload if first else None, timeout=min(remaining, 0.25))
                        break
                    except subprocess.TimeoutExpired:
                        first = False
                        stdout.flush()
                        stderr.flush()
                        if (prefix.with_suffix(".stdout").stat().st_size
                                + prefix.with_suffix(".stderr").stat().st_size > MAX_OUTPUT):
                            overflow = True
                            raise
            except subprocess.TimeoutExpired:
                timed_out = True
                # Only the Popen-owned CLI child is signalled. The owned
                # container is removed by ID in the enclosing finally block.
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            try:
                os.killpg(process.pid, 0)
                group_absent = False
            except ProcessLookupError:
                group_absent = True
            record.update(code=process.returncode, timed_out=timed_out, overflow=overflow,
                          finished_unix=time.time(), leader_joined=True, process_group_absent=group_absent)
        record["stdout_bytes"] = prefix.with_suffix(".stdout").stat().st_size
        record["stderr_bytes"] = prefix.with_suffix(".stderr").stat().st_size
        prefix.with_suffix(".json").write_bytes(encode(record))
        self.commands.append(record)
        require(group_absent, "owned CLI process group is not terminal")
        require(not overflow, "capture_overflow: command output")
        require(not timed_out, "harness_timeout: " + str(args[:4]))
        require(record["stdout_bytes"] + record["stderr_bytes"] <= MAX_OUTPUT, "capture_overflow: aggregate command output")
        output = prefix.with_suffix(".stdout").read_bytes()
        require(failure_ok or process.returncode == 0, "command failed; see " + str(prefix))
        error_output = prefix.with_suffix(".stderr").read_bytes()
        return CommandResult(output, process.returncode, error_output)

    def docker(self, arguments, **kwargs):
        return self.command([DOCKER, "--host", "unix://" + str(SOCKET), *arguments], **kwargs)

    def budget(self):
        # du -P does not follow worktree/tool symlinks. Each root is counted
        # once; Docker's daemon-wide rows are counted once, conservatively.
        roots = [CACHE, MODULE_CACHE, TOOLS]
        result = self.command(["/usr/bin/du", "-P", "-sk", *roots], timeout=30)
        rows = result.stdout.decode().splitlines()
        require(len(rows) == len(roots), "incomplete storage measurement")
        allocated = sum(int(row.split()[0]) * 1024 for row in rows)
        docker_rows = self.docker(["system", "df", "--format", "{{json .}}"], timeout=30)
        docker_bytes = sum(docker_size(load_json(row)["Size"]) for row in docker_rows.stdout.decode().splitlines() if row.strip())
        free = min(shutil.disk_usage(path).free for path in [self.evidence, CACHE, MODULE_CACHE, TOOLS])
        observation = {"utc_unix": time.time(), "aggregate_bytes": allocated + docker_bytes,
                       "docker_once_bytes": docker_bytes, "free_bytes": free, "reliable": True}
        self.budgets.append(observation)
        (self.evidence / "storage.json").write_bytes(encode(self.budgets))
        require(observation["aggregate_bytes"] <= 20 << 30 and free >= 40 << 30, "storage policy exceeded")

    def preflight(self):
        version_row = self.docker(["version", "--format", "{{.Server.Version}}"])
        require(version_row.stdout.decode().strip() == "29.4.0", "Docker server version drift")
        inspected = load_json(self.docker(["image", "inspect", IMAGE]).stdout)[0]
        require(inspected["Architecture"] == "arm64" and IMAGE in inspected["RepoDigests"], "pinned local image unavailable")
        self.image_id = inspected["Id"]
        require(sha256(self.probe.read_bytes()).hexdigest() == self.probe_hash, "probe changed")
        self.budget()
        (self.evidence / "source-manifest.json").write_bytes(encode({"sources": self.source_hashes, "probe": str(self.probe),
            "probe_sha256": self.probe_hash, "image": IMAGE, "image_id": self.image_id, "lease": self.lease}))

    def owned_identity(self, inspected, *, name, python, expected_id=None):
        identity = inspected.get("Id")
        require(isinstance(identity, str) and re.fullmatch(r"[0-9a-f]{64}", identity) is not None,
                "owned container identity absent or invalid")
        require(expected_id is None or identity == expected_id, "create/inspect identity mismatch")
        require(inspected.get("Name") == "/" + name, "container name is not owned by this invocation")
        config = inspected.get("Config") or {}
        labels = config.get("Labels") or {}
        require(labels.get("homeward.nha045.lease") == self.lease, "container lease is foreign or absent")
        require(inspected.get("Image") == self.image_id and config.get("Image") == IMAGE, "owned image identity drift")
        require(config.get("Entrypoint") == (["python3"] if python else ["/nha045-probe"]),
                "owned container role drift")
        return identity

    def persist_containers(self):
        (self.evidence / "containers.json").write_bytes(encode(self.containers))

    def isolated(self, label, *, mode="run", payload=None, python=False, failure_ok=False):
        self.budget()
        require(sha256(self.probe.read_bytes()).hexdigest() == self.probe_hash, "probe artifact changed")
        name = "nha045-" + uuid.uuid4().hex
        absent = self.docker(["container", "inspect", name], failure_ok=True)
        require(confirms_container_absence(absent, name), "candidate container name absence not proven")
        arguments = ["create", "--name", name, "-i", "--pull", "never", "--network", "none", "--read-only",
                     "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
                     "--memory", str(MEMORY), "--memory-swap", str(MEMORY),
                     "--tmpfs", "/tmp:rw,nosuid,nodev,size=67108864", "--label", "homeward.nha045.lease=" + self.lease]
        if python:
            arguments += ["--entrypoint", "python3", IMAGE, "-c", (HERE / "ha_worker.py").read_text()]
        else:
            arguments += ["--mount", f"type=bind,source={self.probe},target=/nha045-probe,readonly",
                          "--entrypoint", "/nha045-probe", IMAGE, "--mode", mode]
        container_id = None
        create_attempted = False
        record = {"label": label, "name": name, "lease": self.lease,
                  "role": "ha" if python else "go", "terminal": False,
                  "id_absence_confirmed": False, "name_absence_confirmed": False}
        # The name and lease are owned before the create CLI can become
        # uncertain. Never depend on receiving its stdout to retain ownership.
        self.containers.append(record)
        self.persist_containers()
        primary = None
        try:
            create_attempted = True
            candidate = self.docker(arguments).stdout.decode().strip()
            require(re.fullmatch(r"[0-9a-f]{64}", candidate) is not None, "create did not return a container identity")
            inspected = load_json(self.docker(["container", "inspect", candidate]).stdout)[0]
            container_id = self.owned_identity(inspected, name=name, python=python, expected_id=candidate)
            record["id"] = container_id
            validate_envelope(inspected, image_id=self.image_id, probe=self.probe, python=python)
            (self.evidence / (name + "-before.json")).write_bytes(encode(inspected))
            result = self.docker(["start", "-a", "-i", container_id], payload=payload, timeout=180, failure_ok=True)
            final = load_json(self.docker(["container", "inspect", container_id], terminal=True).stdout)[0]
            (self.evidence / (name + "-after.json")).write_bytes(encode(final))
            require(not final["State"]["Running"] and final["State"]["ExitCode"] == result.code, "container exit/attach mismatch")
            require(failure_ok or result.code == 0, "isolated worker failed: " + label)
            record["exit_code"] = result.code
            return result
        except BaseException as error:
            primary = error
            record["primary_error"] = repr(error)
            raise
        finally:
            cleanup_error = None
            try:
                if create_attempted and container_id is None:
                    recovered = self.docker(["container", "inspect", name], failure_ok=True, terminal=True)
                    if recovered.code == 0:
                        rows = load_json(recovered.stdout)
                        require(isinstance(rows, list) and len(rows) == 1, "ambiguous owned-name recovery")
                        container_id = self.owned_identity(rows[0], name=name, python=python)
                        record["id"] = container_id
                        record["recovered_by_name"] = True
                        (self.evidence / (name + "-recovered.json")).write_bytes(encode(rows[0]))
                    elif confirms_container_absence(recovered, name):
                        record["creation_absent"] = True
                        record["name_absence_confirmed"] = True
                        record["terminal"] = True
                    else:
                        # A failed inspect can also mean an unavailable daemon.
                        # Do not turn an uncertain create into proven absence.
                        record["recovery_unknown"] = True
                        raise QualificationFailure("owned-name recovery could not establish identity or absence")
                if container_id is not None:
                    self.docker(["rm", "-f", "-v", container_id], terminal=True)
                    absent = self.docker(["container", "inspect", container_id], failure_ok=True, terminal=True)
                    require(confirms_container_absence(absent, container_id), "owned container ID absence not proven after cleanup")
                    record["id_absence_confirmed"] = True
                    absent_name = self.docker(["container", "inspect", name], failure_ok=True, terminal=True)
                    require(confirms_container_absence(absent_name, name), "owned container name absence not proven after cleanup")
                    record["name_absence_confirmed"] = True
                    record["terminal"] = True
            except BaseException as cleanup:
                cleanup_error = cleanup
                record["cleanup_error"] = repr(cleanup)
            finally:
                # Persist even when cleanup itself is the primary exception.
                self.persist_containers()
            if cleanup_error is not None:
                if primary is None:
                    raise cleanup_error
                print("NHA045_CLEANUP " + repr(cleanup_error), file=sys.stderr, flush=True)

    def compare(self, label, fixture, ha, go, *, expect_failure=False):
        payload = encode({"fixture": load_json(fixture), "ha": load_json(ha), "go": load_json(go)})
        result = self.isolated(label, mode="compare", payload=payload, failure_ok=expect_failure)
        (self.evidence / (label + ".json")).write_bytes(result.stdout)
        parsed = load_json(result.stdout)
        require((result.code != 0 and parsed["equivalent"] is False and parsed["differences"]) if expect_failure
                else (result.code == 0 and parsed["equivalent"] is True), "unexpected comparator terminal")
        return parsed

    def collect(self):
        self.preflight()
        contract = load_json(self.isolated("contract", mode="contract").stdout)
        require(contract["image"] == IMAGE, "worker contract image drift")
        rows = load_json(self.isolated("corpus", mode="corpus").stdout)
        require(len(rows) == 10 and len({row["id"] for row in rows}) == 10, "ten distinct fixtures required")
        (self.evidence / "contract.json").write_bytes(encode(contract))
        (self.evidence / "corpus.json").write_bytes(encode(rows))
        captures = {}
        for row in rows:
            fixture = row["fixture"].encode()
            require(sha256(fixture).hexdigest() == row["sha256"], "corpus input hash drift")
            (self.evidence / (row["id"] + ".fixture.json")).write_bytes(fixture)
            envelope = encode({"contract": contract, "fixture": row["fixture"], "fixture_sha256": row["sha256"]})
            # Admission/execution through the strict Go profile precedes HA.
            # An unsupported YAML never starts an HA scenario worker.
            go = self.isolated(row["id"] + "-go", payload=fixture).stdout
            ha_raw = self.isolated(row["id"] + "-ha", python=True, payload=envelope).stdout
            (self.evidence / (row["id"] + ".ha-raw.json")).write_bytes(ha_raw)
            ha = self.isolated(row["id"] + "-project", mode="project-ha",
                               payload=encode({"fixture": load_json(fixture), "ha": load_json(ha_raw)})).stdout
            (self.evidence / (row["id"] + ".ha.json")).write_bytes(ha)
            (self.evidence / (row["id"] + ".go.json")).write_bytes(go)
            self.compare(row["id"] + "-comparison", fixture, ha, go)
            captures[row["id"]] = (fixture, ha, go)
        original, ha, _ = captures["01-restart-delay"]
        mutant = load_json(original)
        changed = 0
        for obj in mutant["objects"]:
            if obj["id"] == mutant["root"]:
                require(obj["yaml"].count("mode: restart\n") == 1, "mutant source no longer has one restart mode")
                obj["yaml"] = obj["yaml"].replace("mode: restart\n", "mode: single\n", 1)
                changed += 1
        require(changed == 1, "mutant must change one YAML object")
        mutant_bytes = encode(mutant)
        (self.evidence / "mutant.fixture.json").write_bytes(mutant_bytes)
        mutant_go = self.isolated("mutant-go", payload=mutant_bytes).stdout
        (self.evidence / "mutant.go.json").write_bytes(mutant_go)
        diff = self.compare("mutant-comparison", original, ha, mutant_go, expect_failure=True)
        require(any(row["field"] == "fact" for row in diff["differences"]), "mutant detected only by input hash")
        replay_go = self.isolated("mutant-replay-go", payload=mutant_bytes).stdout
        (self.evidence / "mutant-replay.go.json").write_bytes(replay_go)
        replay = self.compare("mutant-replay-comparison", original, ha, replay_go, expect_failure=True)
        require(diff == replay, "mutant semantic diff is not replayable")
        self.budget()
        require(all(row["terminal"] for row in self.containers), "container terminal audit incomplete")
        require(all(sha256(Path(path).read_bytes()).hexdigest() == digest for path, digest in self.source_hashes.items()), "qualification sources changed")
        result = {"schema": contract["schema"], "status": "passed", "cases": 10, "mutant_failed": True,
                  "mutant_replay_equal": True, "containers_terminal": True, "source_sha256": self.source_hashes,
                  "probe_sha256": self.probe_hash, "commands": len(self.commands), "lease": self.lease}
        (self.evidence / "result.json").write_bytes(encode(result))
        return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--collect", action="store_true")
    parser.add_argument("--probe", type=Path, required=True)
    parser.add_argument("--evidence-dir", type=Path, required=True)
    arguments = parser.parse_args()
    parser.error("explicit --collect is required") if not arguments.collect else None
    runner = Runner(arguments.probe, arguments.evidence_dir)
    try:
        print(json.dumps(runner.collect(), sort_keys=True))
    except BaseException as error:
        (runner.evidence / "failure.json").write_bytes(encode({"status": "failed", "error_type": type(error).__name__,
            "detail": str(error), "containers": runner.containers, "lease": runner.lease}))
        raise


if __name__ == "__main__":
    main()
