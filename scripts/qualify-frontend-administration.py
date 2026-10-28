#!/usr/bin/env python3
"""Actual session-owned administration on fresh isolated Go Run/Rust TLS.

Default off, synthetic accounts and disabled workday only. No device endpoint,
production configuration, native rendering, IME or Android qualification.
"""
import contextlib
import json
import os
from pathlib import Path
import runpy
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
TEST = 'administration_host::tls_probe::actual_administration_owner_reads_personal_access_and_withdrawal'


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Use frontend.py run --cargo.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf018' / ('tls-owner-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    sources = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(sources, indent=2) + '\n')
    server, binary = SHARED['PROTOCOL']['build'](proof, '^TestNHF018AdministrationServer$')
    fixture = proof / 'fixture'
    process = client = None
    try:
        with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('wb') as client_log:
            process = subprocess.Popen([str(server), '-test.run=^TestNHF018AdministrationServer$',
                '-test.v', '-test.timeout=11m'], cwd=ROOT / 'internal/server',
                env=dict(os.environ, HOMEWARD_NHF018_SYNTHETIC_ONLY='1', HOMEWARD_NHF018_FIXTURE=str(fixture)),
                stdout=server_log, stderr=subprocess.STDOUT)
            ready = SHARED['wait_file'](fixture / 'ready.json', process, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_configuration'] is False
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port
                    and not origin.username and not origin.password):
                raise RuntimeError('Only a fresh isolated loopback administration fixture is admitted')
            client = subprocess.Popen([str(binary), '--exact', TEST, '--ignored', '--nocapture', '--test-threads=1'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF018_TLS_FIXTURE=str(fixture)),
                stdout=client_log, stderr=subprocess.STDOUT)
            if client.wait(timeout=180) != 0:
                raise RuntimeError('Administration TLS owner failed: ' + str(proof / 'client.log'))
            if 'NHF018_ADMINISTRATION_OWNER_TLS_PASS' not in (proof / 'client.log').read_text():
                raise RuntimeError('Administration TLS owner success marker missing')
            (fixture / 'stop').touch(mode=0o600)
            if process.wait(timeout=30) != 0:
                raise RuntimeError('Actual administration Run did not join: ' + str(proof / 'server.log'))
            isolation = json.loads((fixture / 'result.json').read_text())
            if not (isolation['synthetic_only'] and isolation['actual_run_joined']
                    and isolation['only_disabled_workday'] and isolation['production_configuration'] is False):
                raise RuntimeError('Administration fixture isolation evidence incomplete')
        if SHARED['source_inventory'](config) != sources:
            raise RuntimeError('Administration sources changed during qualification')
        (proof / 'result.json').write_text(json.dumps(dict(status='PASS', actual_rust_go_tls=True,
            children_joined=True, native_rendering=False, isolation=isolation), indent=2) + '\n')
        print('Actual administration Rust/Go TLS owner PASS ' + str(proof), flush=True)
    finally:
        SHARED['stop'](client)
        if process is not None and process.poll() is None:
            if fixture.is_dir():
                (fixture / 'stop').touch(mode=0o600)
            with contextlib.suppress(subprocess.TimeoutExpired):
                process.wait(timeout=30)
        SHARED['stop'](process)
        with contextlib.suppress(FileNotFoundError):
            (fixture / 'ready.json').unlink()


if __name__ == '__main__':
    main()
