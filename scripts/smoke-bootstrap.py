#!/usr/bin/env python3
"""Real child process/SIGTERM and cold-start proof on an isolated PostgreSQL cluster."""
import os
import pathlib
import re
import selectors
import signal
import socket
import subprocess
import tempfile
import time


def run(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True)


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
        cfg = root / "config.yaml"
        settings = ("listen: '127.0.0.1:0'\n"
                    f"data_dir: '{root / 'app-data'}'\npostgres:\n"
                    "  host: '127.0.0.1'\n"
                    f"  port: {pg_port}\n"
                    "  database: postgres\n  user: homeward_test\n"
                    "  password_env: HOMEWARD_POSTGRES_PASSWORD\n  sslmode: disable\n")
        cfg.write_text(settings)
        env = os.environ.copy()
        env["HOMEWARD_POSTGRES_PASSWORD"] = password
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
                line = data_read.decode()
                assert password not in line
                port = int(re.search(r"public transport ready 127\.0\.0\.1:(\d+)", line)[1])
            finally:
                selector.close()
            proc.send_signal(signal.SIGTERM)
            output = proc.communicate(timeout=5)[0]
            assert proc.returncode == 0 and "shutdown complete" in output and password not in output
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()

        cfg.write_text(settings + "unknown: true\n")
        result = subprocess.run([str(binary), "-config", str(cfg)], capture_output=True, text=True, timeout=5, env=env)
        assert result.returncode != 0 and "unknown field" in result.stderr and password not in result.stderr
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", port))
        cfg.write_text(settings)
        pg_env = env.copy()
        pg_env["PGPASSWORD"] = password
        psql = ["psql", "-h", "127.0.0.1", "-p", str(pg_port), "-U", "homeward_test", "-d", "postgres", "-X", "-A", "-t"]
        current = int(subprocess.run(psql + ["-c", "SELECT max(version) FROM public.homeward_schema_migrations"], check=True, capture_output=True, text=True, env=pg_env).stdout.strip())
        future_version = current + 1
        subprocess.run(psql + ["-c", f"INSERT INTO public.homeward_schema_migrations(version) VALUES ({future_version})"], check=True, capture_output=True, text=True, env=pg_env)
        future = subprocess.run([str(binary), "-config", str(cfg)], capture_output=True, text=True, timeout=10, env=env)
        assert future.returncode != 0 and f"schema version {future_version} is newer" in future.stderr
        assert "public transport ready" not in future.stderr and password not in future.stderr
    finally:
        run("pg_ctl", "-D", str(root / "data"), "-m", "immediate", "-w", "stop")

    cfg.write_text(settings)
    result = subprocess.run([str(binary), "-config", str(cfg)], capture_output=True, text=True, timeout=10, env=env)
    assert result.returncode != 0 and "postgres: unavailable" in result.stderr
    assert "public transport ready" not in result.stderr and password not in result.stderr
print("isolated PostgreSQL startup, SIGTERM, invalid YAML, future schema, and unavailable DB: pass")
