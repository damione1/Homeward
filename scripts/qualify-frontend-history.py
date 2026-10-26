#!/usr/bin/env python3
"""Real Rust history owner / ordinary Go Run on an isolated synthetic database.

Run through frontend.py run --cargo. Cost mocks and native rendering remain
separate evidence; this fixture has no physical command driver.
"""
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import runpy
import selectors
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
PROTOCOL = SHARED['PROTOCOL']
TEST = 'history_host::tls_probe::real_tls_history_owner_and_source_values'


class Control(Enum):
    STALE = 'publish-stale'
    REVOKE = 'revoke-history'


def qualify(proof, server_binary, client_binary):
    fixture = proof / 'fixture'
    ready_path = fixture / 'ready.json'
    server = client = None
    controls, report = [], None
    source = proof / 'source.json'
    source.write_bytes((ROOT / 'qualification/frontend/history/source.json').read_bytes())
    with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('w') as client_log:
        try:
            server = subprocess.Popen(
                [str(server_binary), '-test.run=^TestNHF013HistoryServer$', '-test.v', '-test.timeout=11m'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF013_FIXTURE=str(fixture), HOMEWARD_NHF013_SYNTHETIC_ONLY='1', HOMEWARD_NHF013_SOURCE=str(source)),
                stdout=server_log, stderr=subprocess.STDOUT)
            ready = PROTOCOL['wait_file'](ready_path, server, timeout=240)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and origin.scheme == 'https'
                    and origin.hostname == '127.0.0.1' and origin.port
                    and not origin.username and not origin.password
                    and ready['point_count'] == 20000 and ready['costs_mock'] is False):
                raise RuntimeError('Explicit loopback synthetic history fixture required')
            client = subprocess.Popen(
                [str(client_binary), '--exact', TEST, '--ignored', '--nocapture', '--test-threads=1'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF013_READY=str(ready_path)),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=0)
            deadline, pending = time.monotonic() + 240, b''
            with selectors.DefaultSelector() as selector:
                selector.register(client.stdout, selectors.EVENT_READ)
                while True:
                    if time.monotonic() > deadline:
                        raise RuntimeError('Owned history probe exceeded its deadline')
                    if not selector.select(.25):
                        if client.poll() is not None:
                            break
                        continue
                    chunk = os.read(client.stdout.fileno(), 65536)
                    if not chunk:
                        break
                    pending += chunk
                    while b'\n' in pending:
                        raw, pending = pending.split(b'\n', 1)
                        line = raw.decode() + '\n'
                        client_log.write(line)
                        client_log.flush()
                        if 'NHF013_CONTROL ' in line:
                            command = json.loads(line.split('NHF013_CONTROL ', 1)[1])
                            action = Control(command['action'])
                            if (set(command) != {'sequence', 'action'} or len(controls) >= len(Control)
                                    or command['sequence'] != len(controls) + 1
                                    or action != tuple(Control)[len(controls)]):
                                raise RuntimeError('Unbound or out-of-order history fixture control')
                            SHARED['acknowledge'](fixture, server, command)
                            controls.append(command)
                            client.stdin.write((json.dumps(command) + '\n').encode())
                            client.stdin.flush()
                        if 'NHF013_REPORT ' in line:
                            if report is not None:
                                raise RuntimeError('Duplicate history report')
                            report = json.loads(line.split('NHF013_REPORT ', 1)[1])
            if client.wait(timeout=10) != 0 or report is None:
                raise RuntimeError('Actual Rust history probe failed; inspect ' + str(proof / 'client.log'))
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('Ordinary history Run did not stop cleanly')
            final = json.loads((fixture / 'result.json').read_text())
            if not (report['pass'] and report['points'] == 20000 and report['restored_points'] == 20001
                    and all(report[key] is True for key in ('actual_history_tls', 'ordinary_run_recorder',
                        'server_aggregates', 'stale_bound_cursor', 'invalid_hour_rejected',
                        'foreign_entity_denied', 'revocation_purge', 'owners_joined'))
                    and report['costs_mock'] is False and report['native_rendering'] is False
                    and len(controls) == len(Control)
                    and final == dict(server_joined=True, command_driver_entries=0, real_device_writes=0)):
                raise RuntimeError('Incomplete actual history owner evidence')
            return dict(client=report, server=final, controls=controls)
        finally:
            try:
                PROTOCOL['stop'](client)
            finally:
                try:
                    PROTOCOL['stop'](server)
                finally:
                    with contextlib.suppress(FileNotFoundError):
                        ready_path.unlink()


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Run through scripts/frontend.py run --cargo.')
    subprocess.run(['gofmt', '-w', 'internal/server/nhf013_history_server_test.go'], cwd=ROOT, check=True)
    subprocess.run(['cargo', 'fmt', '--all'], cwd=ROOT, check=True)
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'history' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(before, indent=2) + '\n')
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    server, client = PROTOCOL['build'](proof, '^TestNHF013HistoryFixtureControls$')
    result = qualify(proof, server, client)
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Sources changed during history qualification')
    (proof / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(dict(proof=str(proof), **result), indent=2))


if __name__ == '__main__':
    main()
