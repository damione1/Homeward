#!/usr/bin/env python3
"""Qualify an isolated Compose project; never read device configuration."""

import base64
import hashlib
import json
import os
from pathlib import Path
import http.client
import socket
import re
import subprocess
import tempfile
import time
import uuid


ROOT = Path(__file__).resolve().parents[1]
IMAGE = "homeward-server:nha168"
PASSWORD = "NHA168_Synthetic_terminal_password_123!"
BACKUP_SECRET = "NHA157_SYNTHETIC_BACKUP_CREDENTIAL"


class CommandFailure(RuntimeError):
    def __init__(self, code, expected, stdout, stderr):
        super().__init__(f"synthetic command failed (exit {code}, expected {expected})")
        self.returncode, self.stdout, self.stderr = code, stdout, stderr


def execute(args, *, env, expected=0, timeout=120):
    result = subprocess.run(args, cwd=ROOT, env=env, text=True, capture_output=True, timeout=timeout)
    if result.returncode != expected:
        # Deliberately omit captured streams: a failed secret-redaction check
        # must never turn the harness into a credential printer.
        raise CommandFailure(result.returncode, expected, result.stdout, result.stderr)
    if any(marker in result.stdout + result.stderr for marker in (PASSWORD, BACKUP_SECRET, "HOMEWARD_SECRETS_KEY_V1=", "AGE-SECRET-KEY-")):
        raise RuntimeError("synthetic command disclosed a secret")
    return result.stdout


def failure_diagnostics(compose, env, engine, stage, failure):
    """Emit closed states and file metadata before down; never raw logs/env."""
    deadline = time.monotonic() + 20
    report = {"stage": stage, "services": {}}
    if isinstance(failure, CommandFailure):
        report["command_exit"] = failure.returncode
        combined = failure.stdout + failure.stderr
        report["compose_error_classes"] = [name for name, marker in (
            ("dependency_failed", "dependency failed"), ("container_unhealthy", "unhealthy"),
            ("service_exited", "exited"), ("port_unavailable", "port is already allocated"),
            ("permission_denied", "permission denied")) if marker in combined.lower()]

    def capture(args):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError()
        return subprocess.run(compose + args, cwd=ROOT, env=env, text=True,
                              capture_output=True, timeout=min(4, remaining))

    codes = {"LOG_CODE_PROCESS_CONFIG_READ_FAILED", "LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED",
             "LOG_CODE_PROCESS_RUNTIME_START_FAILED", "LOG_CODE_PROCESS_RUNTIME_FAILED",
             "LOG_CODE_PROCESS_READY", "LOG_CODE_PROCESS_STOPPED"}
    for service in ("prepare", "postgres", "homed"):
        item = {}
        report["services"][service] = item
        try:
            ids = capture(["ps", "--all", "--quiet", service]).stdout.split()
            if ids:
                identity = ids[0]
                if len(identity) != 64 or any(c not in "0123456789abcdef" for c in identity):
                    raise ValueError()
                state = engine.request("GET", engine.prefix + "/containers/" + identity + "/json")["State"]
                status = state.get("Status")
                item["state"] = status if status in {"created", "running", "paused", "restarting", "removing", "exited", "dead"} else "unknown"
                item["exit_code"] = state.get("ExitCode")
                item["engine_error_present"] = bool(state.get("Error"))
                health = state.get("Health") or {}
                item["health"] = health.get("Status") if health.get("Status") in {"starting", "healthy", "unhealthy"} else "none"
                item["health_exit_codes"] = [entry.get("ExitCode") for entry in health.get("Log", [])[-3:]]
            logs = capture(["logs", "--no-color", "--no-log-prefix", "--tail", "12", service]).stdout
            safe_codes = []
            for line in logs.splitlines():
                if line.strip() == "package: private deployment files unavailable; existing files were preserved":
                    safe_codes.append("PACKAGE_PRIVATE_FILES_UNAVAILABLE")
                try:
                    value = json.loads(line)
                except (ValueError, TypeError):
                    continue
                if isinstance(value, dict) and value.get("code") in codes:
                    safe_codes.append(value["code"])
            item["process_codes"] = safe_codes
        except Exception:
            item["diagnostic_unavailable"] = True
    # A short-lived process in this same synthetic project checks metadata as
    # the exact prepare UID. No file contents or directory entry names escape.
    metadata = """
import errno,itertools,json,os,stat
paths=['/var/lib/homeward','/var/lib/homeward/data','/var/lib/homeward/configuration.yaml','/var/lib/homeward/configuration.yaml.bootstrap.yaml','/var/lib/homeward/data/.env','/var/lib/homeward/data/server.crt','/var/lib/homeward/data/server.key','/run/homeward-credentials','/run/homeward-credentials/password','/existing-postgres']
result={}
for path in paths:
 try:
  s=os.lstat(path);result[path]={'uid':s.st_uid,'gid':s.st_gid,'mode':oct(stat.S_IMODE(s.st_mode)),'directory':stat.S_ISDIR(s.st_mode),'symlink':stat.S_ISLNK(s.st_mode)}
  if stat.S_ISDIR(s.st_mode):
   try:
    with os.scandir(path) as entries:result[path]['entry_count_capped_32']=sum(1 for _ in itertools.islice(entries,32))
   except OSError as e:result[path]['list_errno']=e.errno
 except OSError as e:result[path]={'stat_errno':e.errno}
print(json.dumps(result))
"""
    try:
        probe = capture(["run", "--rm", "--no-deps", "--entrypoint", "python3", "prepare", "-c", metadata])
        report["private_file_metadata"] = json.loads(probe.stdout) if probe.returncode == 0 else {"probe_exit": probe.returncode}
    except Exception:
        report["private_file_metadata"] = {"diagnostic_unavailable": True}
    print("nha168 failure diagnostics: " + json.dumps(report, sort_keys=True), flush=True)


def qualify_manual_backup(run):
    """Real packaged clients, synthetic records and an empty separate database."""
    operator = "/run/nha157-operator"
    data = "/var/lib/homeward/data"
    restored = "/var/lib/homeward/nha157-restored"
    package = "/usr/local/bin/homeward-package"
    homed = "/usr/local/bin/homed"
    source = lambda code, **kw: run("exec", "-T", "homed", "python3", "-c", code, **kw)
    sql = lambda query, database="homeward": run("exec", "-T", "postgres", "psql", "-v", "ON_ERROR_STOP=1", "-U", "homeward", "-d", database, "-At", "-c", query)
    backup = lambda *args, **kw: run("exec", "-T", "homed", package, "installation", "backup", *args, **kw)
    # The independent volume is mounted separately from the server data and DB.
    run("exec", "-T", "homed", homed, "recovery", "export", "--private-output", operator + "/retained.key", "--recipient-output", data + "/recovery.pub", "--server-data-dir", data)
    run("exec", "-T", "homed", homed, "recovery", "copy", "--private-file", operator + "/retained.key", "--private-output", operator + "/second.key", "--server-data-dir", data)
    run("exec", "-T", "homed", homed, "recovery", "verify", "--private-file", operator + "/second.key", "--recipient-file", data + "/recovery.pub", "--server-data-dir", data)
    run("stop", "homed")
    edit = """
import os,stat
from pathlib import Path
p=Path('/var/lib/homeward/configuration.yaml.bootstrap.yaml');s=p.lstat()
assert stat.S_ISREG(s.st_mode) and stat.S_IMODE(s.st_mode)==0o600 and s.st_uid==os.getuid()
raw=p.read_bytes();assert b'backup_recovery_recipient:' not in raw
recipient=Path('/var/lib/homeward/data/recovery.pub').read_text().strip()
assert recipient.startswith('age1') and '\\n' not in recipient
q=p.with_name(p.name+'.nha157-new');fd=os.open(q,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'wb') as f:f.write(raw+b'\\nbackup_recovery_recipient: '+recipient.encode()+b'\\n');f.flush();os.fsync(f.fileno())
os.replace(q,p);fd=os.open(p.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
assert p.read_bytes().startswith(raw) and p.stat().st_uid==s.st_uid
"""
    run("run", "--rm", "--no-deps", "--entrypoint", "python3", "homed", "-c", edit)
    run("up", "--detach", "--no-build", "--wait", "--wait-timeout", "120", timeout=150)
    # Seed one real 051 cipher record using the documented cipher/AAD format;
    # backup and restore both authenticate it through the production Go path.
    seed = r'''
import base64,hashlib,os,struct
from pathlib import Path
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.kdf.hkdf import HKDF
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
master=base64.b64decode(Path('/var/lib/homeward/data/.env').read_text().strip().split('=',1)[1],validate=True)
key_id=hashlib.sha256(b'homeward/secret-master/v1\x00'+master).digest();salt=os.urandom(32);nonce=os.urandom(12)
parts=[b'homeward-secret-aad-v1',b'aes256gcm_hkdf_sha256_v1',key_id,b'nha157-fixture',b'synthetic-token',salt]
aad=b''.join(struct.pack('>I',len(p))+p for p in parts)+struct.pack('>Q',1)
key=HKDF(algorithm=hashes.SHA256(),length=32,salt=salt,info=b'homeward/integration-secret/aes256gcm/v1').derive(master)
cipher=nonce+AESGCM(key).encrypt(nonce,b'NHA157_SYNTHETIC_BACKUP_CREDENTIAL',aad)
print("BEGIN; INSERT INTO public.secret_salts(salt) VALUES(decode('%s','hex')); INSERT INTO public.integration_secrets(instance_id,secret_name,revision,key_id,suite,salt,ciphertext) VALUES('nha157-fixture','synthetic-token',1,decode('%s','hex'),'aes256gcm_hkdf_sha256_v1',decode('%s','hex'),decode('%s','hex')); COMMIT;"%(salt.hex(),key_id.hex(),salt.hex(),cipher.hex()))
'''
    sql(source(seed))
    sql("BEGIN; INSERT INTO public.registry_devices(id) VALUES('dev_15700000000000000000000000000000'); INSERT INTO public.registry_device_sources(instance_id,source_device,device_id) VALUES('nha157-fixture','nha157-synthetic','dev_15700000000000000000000000000000'); INSERT INTO public.registry_entities(id,device_id,manifest_alias) VALUES('ent_15700000000000000000000000000000','dev_15700000000000000000000000000000','input_boolean.away_mode'); INSERT INTO public.registry_source_bindings(instance_id,source_device,source_entity,entity_id) VALUES('nha157-fixture','nha157-synthetic','boolean','ent_15700000000000000000000000000000'); INSERT INTO public.registry_aliases(alias,entity_id,current) VALUES('input_boolean.away_mode','ent_15700000000000000000000000000000',true); COMMIT;")
    witness_sql = """SELECT json_build_object(
 'generation',(SELECT generation_revision FROM public.config_active WHERE singleton),
 'registry',(SELECT json_agg(r ORDER BY id) FROM public.registry_entities r),
 'accounts',(SELECT json_agg(a ORDER BY id) FROM public.auth_accounts a),
 'key',(SELECT row_to_json(k) FROM public.secret_bootstrap k),
 'secrets',(SELECT json_agg(s ORDER BY instance_id,secret_name) FROM public.integration_secrets s),
 'sources',(SELECT json_agg(s ORDER BY document_id) FROM (SELECT e.document_id,e.version_revision,encode(e.source_hash,'hex') AS hash FROM public.config_generation_entries e JOIN public.config_active a ON a.generation_revision=e.generation_revision WHERE a.singleton) s));"""
    before = json.loads(sql(witness_sql))
    if not all(before[name] for name in ("registry", "accounts", "secrets", "sources")):
        raise RuntimeError("manual backup lacks nonempty synthetic witnesses")
    source_key_hash = source("import hashlib;from pathlib import Path;print(hashlib.sha256(Path('/var/lib/homeward/data/.env').read_bytes()).hexdigest())").strip()
    # An exec has no injected server-child password; only the package wrapper
    # may bridge its private credential file into the command's environment.
    source("import os;assert 'HOMEWARD_POSTGRES_PASSWORD' not in os.environ")
    outcome = backup()
    match = re.fullmatch(r"backup prepared; artifact id ([0-9a-f]{32}); instance local\nbackup verified; encrypted local archive; no runtime started\n", outcome)
    if match is None:
        raise RuntimeError("manual backup did not publish and verify")
    artifact = match[1]
    if json.loads(sql(witness_sql)) != before:
        raise RuntimeError("backup mutated active generation or source witnesses")
    source("from pathlib import Path;p=Path('/var/lib/homeward/data/backups/local/" + artifact + ".hwb');raw=p.read_bytes();assert raw[:8]==b'HWBK\\x00\\x01\\r\\n' and raw[96:].startswith(b'age-encryption.org/v1\\n') and b'HOMEWARD_SECRETS_KEY_V1' not in raw and b'NHA157_SYNTHETIC_BACKUP_CREDENTIAL' not in raw")
    # Native client-major mismatch must fail closed before publication.
    source(r'''
from pathlib import Path
p=Path('/var/lib/homeward/nha157-wrong-pg');p.mkdir(mode=0o700)
for name in ('pg_dump','pg_restore'):
 tool=p/name;tool.write_text("#!/usr/local/bin/python3\nprint('"+name+" (PostgreSQL) 16.0')\n");tool.chmod(0o700)
''')
    for tool in ("pg_dump", "pg_restore"):
        if run("exec", "-T", "homed", "/var/lib/homeward/nha157-wrong-pg/" + tool, "--version").strip() != tool + " (PostgreSQL) 16.0":
            raise RuntimeError("client mismatch fixture did not execute")
    backup("--pg-bin-dir", "/var/lib/homeward/nha157-wrong-pg", expected=1)
    # A lost package credential never causes a replacement to be generated.
    credential_edit = lambda code: run("run", "--rm", "--no-deps", "--entrypoint", "python3", "prepare", "-c", code)
    credential_edit("import os;os.rename('/run/homeward-credentials/password','/run/homeward-credentials/nha157-retained-password')")
    try:
        backup(expected=1)
        credential_edit("from pathlib import Path;assert not Path('/run/homeward-credentials/password').exists()")
    finally:
        credential_edit("import os;os.rename('/run/homeward-credentials/nha157-retained-password','/run/homeward-credentials/password')")
    sql("CREATE DATABASE nha157_restored")
    target = "listen: '127.0.0.1:8788'\ndata_dir: '" + restored + "'\npostgres:\n  host: postgres\n  port: 5432\n  database: nha157_restored\n  user: homeward\n  password_env: HOMEWARD_POSTGRES_PASSWORD\n  sslmode: disable\n"
    source("import os;from pathlib import Path;p=Path('/run/nha157-operator/target.yaml');fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.write(fd," + repr(target.encode()) + ");os.fsync(fd);os.close(fd)")
    restore_args = ["--local-root", data + "/backups", "--artifact-id", artifact, "--private-file", operator + "/second.key", "--output", restored, "--target-bootstrap", operator + "/target.yaml"]
    restore = lambda *extra, **kw: run("exec", "-T", "homed", package, "installation", "restore", *restore_args, *extra, **kw)
    restore("--private-file", data + "/recovery.pub", expected=1)
    if sql("SELECT count(*) FROM pg_class WHERE relnamespace='public'::regnamespace", "nha157_restored").strip() != "0":
        raise RuntimeError("bad recovery identity mutated target")
    recovery = restore()
    if "server remains stopped" not in recovery or "snapshot age " not in recovery or "recovery duration " not in recovery:
        raise RuntimeError("isolated restore lacked measured success")
    after = json.loads(sql(witness_sql, "nha157_restored"))
    restored_generation = after.pop("generation")
    if restored_generation <= before["generation"] or after != {key: value for key, value in before.items() if key != "generation"}:
        raise RuntimeError("isolated restore lost registry/auth/secrets/source witnesses")
    source("import hashlib;from pathlib import Path;assert hashlib.sha256(Path('/var/lib/homeward/nha157-restored/.env').read_bytes()).hexdigest()==" + repr(source_key_hash))
    restore(expected=1)  # Existing output may never be replaced.
    occupied = target.replace("database: nha157_restored", "database: homeward").replace(restored, restored + "-occupied")
    source("import os;fd=os.open('/run/nha157-operator/occupied.yaml',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.write(fd," + repr(occupied.encode()) + ");os.close(fd)")
    restore("--target-bootstrap", operator + "/occupied.yaml", "--output", restored + "-occupied", expected=1)
    if json.loads(sql(witness_sql)) != before:
        raise RuntimeError("occupied target refusal changed source database")
    source("from pathlib import Path;assert not Path('/var/lib/homeward/nha157-restored-occupied').exists();assert sorted(p.name for p in Path('/var/lib/homeward/data/backups/local').glob('*.hwb'))==[" + repr(artifact + ".hwb") + "]")
    # Boot only the restored synthetic declarative installation, with the
    # explicit isolated launch locator; no archived address is used.
    ready_code = r'''
import http.client,json,os,socket,subprocess,time
from pathlib import Path
env=os.environ.copy();env['HOMEWARD_POSTGRES_PASSWORD']=Path('/run/homeward-credentials/password').read_text()
p=subprocess.Popen(['/usr/local/bin/homed','-config','/var/lib/homeward/nha157-restored/configuration.yaml','-bootstrap','/run/nha157-operator/target.yaml'],env=env)
try:
 deadline=time.monotonic()+45
 while time.monotonic()<deadline:
  if p.poll() is not None:raise RuntimeError('restored server exited')
  c=http.client.HTTPConnection('localhost',timeout=1);c.sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);c.sock.settimeout(1)
  try:
   c.sock.connect('/var/lib/homeward/nha157-restored/admin.sock');c.request('GET','/health');r=c.getresponse();health=json.loads(r.read(512))
   if r.status==200 and health['live'] and health['ready'] and not health['degraded']:break
  except (OSError,ValueError):pass
  finally:c.close()
  time.sleep(0.2)
 else:raise RuntimeError('restored server never became ready')
finally:
 p.terminate();p.wait(timeout=15)
'''
    source(ready_code, timeout=70)
    return {"artifact": artifact, "result": "pass", "restored_generation": restored_generation,
            "source_witness_sha256": hashlib.sha256(json.dumps(before, sort_keys=True).encode()).hexdigest(),
            "recovery": recovery.strip(), "automatic_schedule": False}


class DockerEngine:
    """A real Linux container TTY over the local Engine socket, no host PTY."""

    def __init__(self, env):
        endpoint = env.get("DOCKER_HOST")
        if not endpoint:
            endpoint = execute(["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], env=env).strip()
        if not endpoint.startswith("unix://"):
            raise RuntimeError("smoke requires an explicit local Unix Docker endpoint")
        self.path = endpoint.removeprefix("unix://")
        version = self.request("GET", "/version")
        self.prefix = "/v" + version["ApiVersion"]

    def request(self, method, path, body=None):
        connection = http.client.HTTPConnection("localhost", timeout=3)
        connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        connection.sock.settimeout(3)
        connection.sock.connect(self.path)
        try:
            connection.request(method, path, body=json.dumps(body) if body is not None else None,
                               headers={"Content-Type": "application/json"})
            response = connection.getresponse()
            raw = response.read(1 << 20)
            if response.status >= 300:
                raise RuntimeError("local Docker Engine request refused")
            return json.loads(raw) if raw else None
        finally:
            connection.close()

    def terminal(self, container, command, *, prompts, expected=0):
        process = self.request("POST", self.prefix + "/containers/" + container + "/exec",
                               {"AttachStdin": True, "AttachStdout": True, "AttachStderr": True,
                                "Tty": True, "User": "10001:10001", "Cmd": command})["Id"]
        stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        stream.settimeout(3)
        stream.connect(self.path)
        transcript = bytearray()
        prompt_index = 0
        try:
            body = b'{"Detach":false,"Tty":true}'
            request = ("POST " + self.prefix + "/exec/" + process + "/start HTTP/1.1\r\n"
                       "Host: localhost\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n"
                       "Content-Type: application/json\r\nContent-Length: " + str(len(body)) + "\r\n\r\n").encode() + body
            stream.sendall(request)
            headers = bytearray()
            while b"\r\n\r\n" not in headers:
                block = stream.recv(4096)
                if not block:
                    raise RuntimeError("local terminal closed before HTTP upgrade")
                headers.extend(block)
                if len(headers) > 16384:
                    raise RuntimeError("local terminal response headers too large")
            head, initial = headers.split(b"\r\n\r\n", 1)
            if not head.startswith((b"HTTP/1.1 101", b"HTTP/1.1 200")):
                raise RuntimeError("local Docker terminal start refused")
            transcript.extend(initial)
            stream.settimeout(0.2)
            deadline = time.monotonic() + 30
            while time.monotonic() < deadline:
                if prompt_index < len(prompts) and prompts[prompt_index] in transcript:
                    # The CLI writes the prompt immediately before disabling echo.
                    time.sleep(0.05)
                    stream.sendall(PASSWORD.encode() + b"\n")
                    prompt_index += 1
                try:
                    block = stream.recv(4096)
                except TimeoutError:
                    block = None
                if block == b"":
                    break
                if block:
                    transcript.extend(block)
                if len(transcript) > 32768:
                    raise RuntimeError("synthetic terminal output limit exceeded")
                state = self.request("GET", self.prefix + "/exec/" + process + "/json")
                if not state["Running"]:
                    break
            state = self.request("GET", self.prefix + "/exec/" + process + "/json")
            if state["Running"] or state["ExitCode"] != expected or prompt_index != len(prompts):
                raise RuntimeError("synthetic Linux terminal outcome mismatch")
            stream.settimeout(1)
            while len(transcript) <= 32768:
                try:
                    block = stream.recv(4096)
                except TimeoutError:
                    break
                if not block:
                    break
                transcript.extend(block)
            if PASSWORD.encode() in transcript:
                raise RuntimeError("Linux terminal echoed a password")
        finally:
            # The caller's finally removes this isolated container/project,
            # including any timed-out exec. No host pseudo-terminal exists.
            stream.close()


def main():
    project = "nha168-smoke-" + uuid.uuid4().hex[:12]
    cli_volume = project + "-remote-cli"
    operator_volume = project + "-recovery"
    missing_worker_image = project + "-missing-worker:qualification"
    cli_container = project + "-remote-cli"
    env = os.environ.copy()
    env.update(HOMEWARD_HOSTNAME="homeward.test", HOMEWARD_BIND_ADDRESS="127.0.0.1")
    engine = DockerEngine(env)
    with tempfile.TemporaryDirectory(prefix=project + "-") as temporary:
        temporary = Path(temporary)
        override = temporary / "isolated.yaml"
        override.write_text('services:\n  homed:\n    ports: !override ["127.0.0.1::8787"]\n    volumes:\n      - recovery:/run/nha157-operator\n    networks:\n      default:\n        aliases: [homeward.test]\nnetworks:\n  default:\n    internal: true\nvolumes:\n  recovery:\n    name: ' + operator_volume + '\n')
        compose = ["docker", "compose", "--project-name", project, "--file", str(ROOT / "packaging/compose.yaml"), "--file", str(override)]
        run = lambda *args, **kwargs: execute(compose + list(args), env=env, **kwargs)
        local = ["/usr/local/bin/homeward", "--config-dir", "/var/lib/homeward/data/operator-profiles"]
        certificate = temporary / "server.crt"
        stage = "startup"
        try:
            print("nha168: isolated startup and artifact prerequisites", flush=True)
            execute(["docker", "image", "inspect", IMAGE], env=env)
            execute(["docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--mount", f"type=volume,source={operator_volume},target=/var/lib/homeward", "--entrypoint", "/bin/true", IMAGE], env=env)
            rendered = json.loads(run("config", "--format", "json"))
            if set(rendered["services"]) != {"prepare", "postgres", "homed"} or rendered["services"]["postgres"].get("ports"):
                raise RuntimeError("unexpected service or PostgreSQL publication")
            for service in rendered["services"].values():
                if service.get("env_file"):
                    raise RuntimeError("runtime .env required before first boot")
            run("up", "--detach", "--no-build", "--wait", "--wait-timeout", "120", timeout=150)
            health = json.loads(run("exec", "-T", "homed", "/usr/local/bin/homeward-package", "health"))
            if not health["live"] or not health["ready"] or health["degraded"]:
                raise RuntimeError("fresh composed server not ready")
            run("exec", "-T", "homed", "/usr/local/bin/homeward-package", "tzcheck")
            run("exec", "-T", "homed", "python3", "/opt/homeward/drivers/caseta/worker.py", "--help")
            print("nha168: protected local bootstrap and separate HTTPS CLI", flush=True)
            stage = "bootstrap_and_https_cli"
            run("exec", "-T", "homed", *local, "profiles", "add", "operator", "--socket", "/var/lib/homeward/data/admin.sock")
            container = run("ps", "--quiet", "homed").strip()
            engine.terminal(container, local + ["--profile", "operator", "bootstrap", "admin", "--display-name", "Synthetic admin"], prompts=[b"Admin password: ", b"Confirm password: "])
            engine.terminal(container, local + ["--profile", "operator", "bootstrap", "second", "--display-name", "Refused second admin"], prompts=[b"Admin password: ", b"Confirm password: "], expected=5)
            execute(["docker", "cp", container + ":/var/lib/homeward/data/server.crt", str(certificate)], env=env)
            certificate.chmod(0o644)
            execute(["docker", "run", "--detach", "--name", cli_container,
                     "--network", project + "_default", "--mount", f"type=volume,source={cli_volume},target=/var/lib/homeward",
                     "--mount", f"type=bind,source={certificate},target=/run/server.crt,readonly",
                     "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--read-only",
                     "--entrypoint", "/usr/local/bin/python3", IMAGE, "-c", "import time;time.sleep(600)"], env=env)
            remote_command = ["/usr/local/bin/homeward", "--config-dir", "/var/lib/homeward/profiles"]
            remote = ["docker", "exec", cli_container] + remote_command
            execute(remote + ["profiles", "add", "remote", "--url", "https://homeward.test:8787", "--ca", "/run/server.crt"], env=env)
            # Daily CLI process has neither the server volume nor admin.sock.
            engine.terminal(cli_container, remote_command + ["--profile", "remote", "login", "admin"], prompts=[b"Password: "])
            execute(remote + ["--profile", "remote", "accounts", "list"], env=env)
            execute(remote + ["--profile", "remote", "bootstrap", "public", "--display-name", "Refused public bootstrap"], env=env, expected=4)
            execute(remote + ["profiles", "add", "untrusted", "--url", "https://homeward.test:8787"], env=env)
            engine.terminal(cli_container, remote_command + ["--profile", "untrusted", "login", "admin"], prompts=[b"Password: "], expected=6)
            print("nha168: persistent recreation and encryption-key refusal", flush=True)
            stage = "persistent_recreation_and_key_refusal"
            snapshot_code = """
import hashlib,json,os
from pathlib import Path
paths=['/var/lib/homeward/configuration.yaml','/var/lib/homeward/configuration.yaml.bootstrap.yaml','/var/lib/homeward/data/.env','/var/lib/homeward/data/server.key','/run/homeward-credentials/password']
print(json.dumps({p:{'sha256':hashlib.sha256(Path(p).read_bytes()).hexdigest(),'mode':oct(os.stat(p).st_mode&0o777)} for p in paths}))
"""
            before = json.loads(run("exec", "-T", "homed", "python3", "-c", snapshot_code))
            if any(value["mode"] != "0o600" for value in before.values()):
                raise RuntimeError("persistent package files not private")
            database_sql = "SELECT json_build_object('key', (SELECT row_to_json(b) FROM public.secret_bootstrap b), 'accounts', (SELECT json_agg(a ORDER BY id) FROM public.auth_accounts a), 'secrets', (SELECT json_agg(s ORDER BY instance_id,secret_name) FROM public.integration_secrets s));"
            database_before = run("exec", "-T", "postgres", "psql", "-U", "homeward", "-d", "homeward", "-At", "-c", database_sql)
            run("up", "--detach", "--no-build", "--force-recreate", "--wait", "--wait-timeout", "120", timeout=150)
            after = json.loads(run("exec", "-T", "homed", "python3", "-c", snapshot_code))
            if before != after:
                raise RuntimeError("container recreation changed persistent YAML, TLS, credentials or server key")
            run("stop", "homed")
            modify = lambda source: run("run", "--rm", "--no-deps", "--entrypoint", "python3", "homed", "-c", source)
            modify("import os;os.rename('/var/lib/homeward/data/.env','/var/lib/homeward/data/.nha168-retained-key')")
            run("run", "--rm", "--no-deps", "homed", expected=1, timeout=20)
            modify("from pathlib import Path;assert not Path('/var/lib/homeward/data/.env').exists()")
            wrong = "HOMEWARD_SECRETS_KEY_V1=" + base64.b64encode(bytes([0x68])*32).decode() + "\n"
            modify("import os;f=os.open('/var/lib/homeward/data/.env',os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.write(f," + repr(wrong.encode()) + ");os.close(f)")
            run("run", "--rm", "--no-deps", "homed", expected=1, timeout=20)
            modify("from pathlib import Path;p=Path('/var/lib/homeward/data/.env');assert p.read_bytes()==" + repr(wrong.encode()) + ";p.unlink();Path('/var/lib/homeward/data/.nha168-retained-key').rename(p)")
            database_after = run("exec", "-T", "postgres", "psql", "-U", "homeward", "-d", "homeward", "-At", "-c", database_sql)
            if database_before != database_after:
                raise RuntimeError("key refusal changed durable accounts/secrets")
            run("up", "--detach", "--no-build", "--wait", "--wait-timeout", "120", timeout=150)
            if json.loads(run("exec", "-T", "homed", "python3", "-c", snapshot_code)) != before:
                raise RuntimeError("restoring retained key failed to preserve deployment")
            containers = json.loads(execute(["docker", "inspect", run("ps", "--quiet", "postgres").strip()], env=env))
            if any(value for value in containers[0]["NetworkSettings"]["Ports"].values()):
                raise RuntimeError("PostgreSQL port published")
            print("nha168: reject artifact missing expected worker executable", flush=True)
            stage = "missing_worker_artifact"
            missing_dockerfile = temporary / "Dockerfile.missing-worker"
            # Only changes the image configuration; all layers remain shared.
            missing_dockerfile.write_text("FROM " + IMAGE + "\nENV PATH=/nha168-no-tools\n")
            execute(["docker", "build", "--network", "none", "--tag", missing_worker_image, "--file", str(missing_dockerfile), str(temporary)], env=env)
            execute(["python3", str(ROOT / "scripts/package-server.py"), "--skip-build", "--image", missing_worker_image, "--output", str(temporary / "missing-worker-artifact")], env=env, expected=1)
            print("nha157: packaged manual encrypted backup and isolated PostgreSQL17 restore", flush=True)
            stage = "manual_backup_and_isolated_restore"
            manual_backup = qualify_manual_backup(run)
            print(json.dumps({"project": project, "result": "pass", "health": health, "persistent_file_hashes": before, "database_fingerprint": hashlib.sha256(database_after.encode()).hexdigest(), "manual_backup": manual_backup}))
        except Exception as failure:
            failure_diagnostics(compose, env, engine, stage, failure)
            raise
        finally:
            # Only this randomly named project and its one client volume.
            subprocess.run(["docker", "rm", "--force", cli_container], cwd=ROOT, env=env, capture_output=True)
            run("down", "--volumes", "--remove-orphans", timeout=90)
            subprocess.run(["docker", "volume", "rm", cli_volume], cwd=ROOT, env=env, capture_output=True)
            subprocess.run(["docker", "image", "rm", missing_worker_image], cwd=ROOT, env=env, capture_output=True)


if __name__ == "__main__":
    main()
