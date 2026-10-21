#!/usr/bin/env python3
"""Real child process/SIGTERM and cold-start proof on an isolated PostgreSQL cluster."""
import hashlib
import json
import os
import pathlib
import selectors
import signal
import socket
import subprocess
import tempfile
import time


def run(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True)


def expect_process_event(output, code, level):
    lines = output.splitlines()
    assert len(lines) == 1, "expected exactly one closed process diagnostic"
    event = json.loads(lines[0])
    assert set(event) == {"code", "level", "component", "occurredAt", "author"}
    assert event["code"] == code and event["level"] == level, "unexpected process diagnostic"
    assert event["component"] == "LOG_COMPONENT_PROCESS"
    assert event["author"] == {"process": {}}
    assert isinstance(event["occurredAt"], str) and event["occurredAt"]


def expect_public_released(port):
    with socket.socket() as sock:
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind(("127.0.0.1", port))
        sock.listen(1)


def expect_ready_then_stop(binary, cfg, env, password, public_port):
    proc = subprocess.Popen([str(binary), "-config", str(cfg)], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env)
    try:
        selector = selectors.DefaultSelector()
        selector.register(proc.stdout, selectors.EVENT_READ)
        try:
            deadline = time.monotonic() + 10
            data_read = b""
            while b"\n" not in data_read:
                remaining = deadline - time.monotonic()
                assert remaining > 0 and selector.select(timeout=remaining), "startup log timeout"
                chunk = os.read(proc.stdout.fileno(), 4096)
                assert chunk, "process exited before startup"
                data_read += chunk
                assert len(data_read) <= 4096, "process diagnostic exceeded its closed bound"
            line = data_read.decode()
            assert password not in line
            expect_process_event(line, "LOG_CODE_PROCESS_READY", "LOG_LEVEL_INFO")
            with socket.create_connection(("127.0.0.1", public_port), timeout=2):
                pass
        finally:
            selector.close()
        proc.send_signal(signal.SIGTERM)
        output = proc.communicate(timeout=5)[0]
        assert proc.returncode == 0 and password not in output
        expect_process_event(output, "LOG_CODE_PROCESS_STOPPED", "LOG_LEVEL_INFO")
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait()
    expect_public_released(public_port)


def start_postgres(data, log, options):
    try:
        run("pg_ctl", "-D", str(data), "-l", str(log), "-o", options, "-w", "start")
    except subprocess.CalledProcessError as exc:
        detail = f"pg_ctl start: {exc}; stdout: {(exc.stdout or '')[-8192:]}; stderr: {(exc.stderr or '')[-8192:]}"
        try:
            with log.open("rb") as server_log:
                server_log.seek(0, os.SEEK_END)
                server_log.seek(max(0, server_log.tell() - 8192))
                tail_bytes = server_log.read(8192)
                tail = tail_bytes.decode(errors="replace")
            if tail:
                detail += f"; postgres.log (last {len(tail_bytes)} bytes): {tail}"
        except FileNotFoundError:
            pass
        except OSError as log_error:
            detail += f"; postgres.log read: {log_error}"
        try:
            status = subprocess.run(["pg_ctl", "-D", str(data), "status"], capture_output=True, text=True)
            if status.returncode == 0:
                stopped = subprocess.run(["pg_ctl", "-D", str(data), "-m", "immediate", "-w", "stop"], capture_output=True, text=True)
                if stopped.returncode != 0:
                    detail += f"; cleanup: pg_ctl stop exited {stopped.returncode}: {stopped.stderr[-8192:]}"
            elif status.returncode != 3:
                detail += f"; cleanup: pg_ctl status exited {status.returncode}: {status.stderr[-8192:]}"
        except OSError as status_error:
            detail += f"; cleanup: pg_ctl status: {status_error}"
        raise RuntimeError(detail) from exc


with tempfile.TemporaryDirectory(prefix="nha027-bootstrap-") as directory:
    root = pathlib.Path(directory)
    binary = root / "homed"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/homed"], check=True)
    password = "NHA027_smoke_canary"
    password_file = root / "password"
    password_file.write_text(password + "\n")
    password_file.chmod(0o600)
    data = root / "data"
    run("initdb", "-D", str(data), "--username=homeward_test", "--auth-host=scram-sha-256", "--auth-local=trust", "--pwfile=" + str(password_file), "--no-instructions")
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        pg_port = sock.getsockname()[1]
    log = root / "postgres.log"
    pg_options = f"-h 127.0.0.1 -p {pg_port} -c unix_socket_directories=''"
    start_postgres(data, log, pg_options)
    try:
        # The closed process event deliberately carries no address. The fixture
        # owns its configured loopback port and verifies that exact listener.
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            public_port = sock.getsockname()[1]
        cfg = root / "config.yaml"
        settings = (f"listen: '127.0.0.1:{public_port}'\n"
                    f"data_dir: '{root / 'a'}'\npostgres:\n"
                    "  host: '127.0.0.1'\n"
                    f"  port: {pg_port}\n"
                    "  database: postgres\n  user: homeward_test\n"
                    "  password_env: HOMEWARD_POSTGRES_PASSWORD\n  sslmode: disable\n")
        cfg.write_text(settings)
        env = os.environ.copy()
        env["HOMEWARD_POSTGRES_PASSWORD"] = password
        sidecar = pathlib.Path(str(cfg) + ".bootstrap.yaml")
        master_key = root / "a" / ".env"
        assert not sidecar.exists() and not master_key.exists(), "bootstrap fixture was already established"
        # With no stable launch bootstrap or SQL generation, unknown fields
        # must still fail the actual CLI admission before any key/listener.
        cfg.write_text(settings + "unknown: true\n")
        result = subprocess.run([str(binary), "-config", str(cfg)], capture_output=True, text=True, timeout=5, env=env)
        assert result.returncode != 0 and not result.stdout.strip()
        assert password not in result.stdout + result.stderr
        expect_process_event(result.stderr, "LOG_CODE_PROCESS_CONFIG_ADMISSION_FAILED", "LOG_LEVEL_ERROR")
        expect_public_released(public_port)
        assert not sidecar.exists() and not master_key.exists(), "invalid fresh source established a bootstrap/key"
        cfg.write_text(settings)
        pg_env = env.copy()
        pg_env["PGPASSWORD"] = password
        psql = ["psql", "-h", "127.0.0.1", "-p", str(pg_port), "-U", "homeward_test", "-d", "postgres", "-X", "-A", "-t"]
        fresh = subprocess.run(psql + ["-c", "SELECT to_regclass('public.config_active') IS NULL AND to_regclass('public.secret_bootstrap') IS NULL"], check=True, capture_output=True, text=True, env=pg_env)
        assert fresh.stdout.strip() == "t", "invalid fresh source reached SQL initialization"
        expect_ready_then_stop(binary, cfg, env, password, public_port)
        assert sidecar.is_file() and master_key.is_file(), "valid startup omitted the stable bootstrap/key"
        sidecar_before = sidecar.read_bytes()
        key_before = hashlib.sha256(master_key.read_bytes()).digest()
        assert password.encode() not in sidecar_before, "launch bootstrap exposed the database password"
        snapshot_sql = """SELECT jsonb_build_object(
          'active', (SELECT jsonb_build_object('revision', g.revision,
              'manifest', encode(g.manifest_hash, 'hex'), 'format', g.manifest_format)
            FROM public.config_active a JOIN public.config_generations g
              ON g.revision = a.generation_revision WHERE a.singleton),
          'generations', (SELECT count(*) FROM public.config_generations),
          'versions', (SELECT count(*) FROM public.config_versions),
          'attempts', (SELECT count(*) FROM public.config_activation_attempts),
          'commands', (SELECT count(*) FROM public.command_intents),
          'keys', (SELECT count(*) FROM public.secret_bootstrap),
          'salts', (SELECT count(*) FROM public.secret_salts),
          'bootstrap', (SELECT to_jsonb(b) FROM public.config_bootstrap_active b WHERE b.singleton)
        )"""
        def authority_snapshot():
            result = subprocess.run(psql + ["-c", snapshot_sql], check=True, capture_output=True, text=True, env=pg_env)
            return json.loads(result.stdout)
        established = authority_snapshot()
        assert established["active"]["revision"] > 0 and established["active"]["format"] == "compiled_v2"
        assert established["generations"] == 1 and established["attempts"] == 1 and established["commands"] == 0 and established["keys"] == 1
        # Once established, the sidecar selects SQL without parsing a dirty
        # workspace. Readiness is correct; it must not mint a generation,
        # activation audit, command, or key to recover from those invalid bytes.
        cfg.write_text(settings + "unknown: true\n")
        expect_ready_then_stop(binary, cfg, env, password, public_port)
        assert authority_snapshot() == established, "dirty-workspace replay changed SQL authority or effects"
        assert sidecar.read_bytes() == sidecar_before, "dirty-workspace replay changed the launch bootstrap"
        assert hashlib.sha256(master_key.read_bytes()).digest() == key_before, "dirty-workspace replay replaced the master key"
        assert cfg.read_text() == settings + "unknown: true\n", "SQL replay rewrote the dirty workspace"
        cfg.write_text(settings)
        current = int(subprocess.run(psql + ["-c", "SELECT max(version) FROM public.homeward_schema_migrations"], check=True, capture_output=True, text=True, env=pg_env).stdout.strip())
        future_version = current + 1
        subprocess.run(psql + ["-c", f"INSERT INTO public.homeward_schema_migrations(version) VALUES ({future_version})"], check=True, capture_output=True, text=True, env=pg_env)
        future = subprocess.run([str(binary), "-config", str(cfg)], capture_output=True, text=True, timeout=10, env=env)
        assert future.returncode != 0 and not future.stdout.strip()
        assert password not in future.stdout + future.stderr
        expect_process_event(future.stderr, "LOG_CODE_PROCESS_RUNTIME_START_FAILED", "LOG_LEVEL_ERROR")
        expect_public_released(public_port)
    finally:
        run("pg_ctl", "-D", str(root / "data"), "-m", "immediate", "-w", "stop")

    cfg.write_text(settings)
    result = subprocess.run([str(binary), "-config", str(cfg)], capture_output=True, text=True, timeout=10, env=env)
    assert result.returncode != 0 and not result.stdout.strip()
    assert password not in result.stdout + result.stderr
    expect_process_event(result.stderr, "LOG_CODE_PROCESS_RUNTIME_START_FAILED", "LOG_LEVEL_ERROR")
    expect_public_released(public_port)
print("isolated PostgreSQL startup, SIGTERM, fresh invalid YAML, dirty-workspace SQL replay, future schema, and unavailable DB: pass")
