#!/usr/bin/env python3
"""Actual logic owner on an explicit isolated Go Run/PG/TLS fixture.

No routine engine, action service or physical provider is invoked. The mock
models server-owned catalogue, run, cancellation and capture transitions only.
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
TEST = 'administration_host::logic::workflow::tls_probe::actual_logic_owner_modes_gaps_lost_reply_restart_and_withdrawal'


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Use the reserved frontend.py run --cargo environment.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf020' / ('tls-owner-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    sources = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(sources, indent=2) + '\n')
    server, binary = SHARED['PROTOCOL']['build'](proof, '^TestNHF020LogicServer$')
    fixture = proof / 'fixture'
    process = client = None
    try:
        with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('wb') as client_log:
            process = subprocess.Popen([str(server), '-test.run=^TestNHF020LogicServer$', '-test.v', '-test.timeout=11m'], cwd=ROOT / 'internal/server', env=dict(os.environ, HOMEWARD_NHF020_SYNTHETIC_ONLY='1', HOMEWARD_NHF020_FIXTURE=str(fixture)), stdout=server_log, stderr=subprocess.STDOUT)
            ready = SHARED['wait_file'](fixture / 'ready.json', process, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_configuration'] is False and ready['no_physical_providers'] is True and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Only an explicitly isolated loopback logic fixture is admitted')
            client = subprocess.Popen([str(binary), '--exact', TEST, '--ignored', '--nocapture', '--test-threads=1'], cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF020_TLS_FIXTURE=str(fixture)), stdout=client_log, stderr=subprocess.STDOUT)
            if client.wait(timeout=240) != 0:
                raise RuntimeError('Logic owner TLS failed: ' + str(proof / 'client.log'))
            if 'NHF020_LOGIC_OWNER_TLS_PASS' not in (proof / 'client.log').read_text():
                raise RuntimeError('Logic owner success marker missing')
            (fixture / 'stop').touch(mode=0o600)
            if process.wait(timeout=30) != 0:
                raise RuntimeError('Actual logic Run did not join: ' + str(proof / 'server.log'))
            isolation = json.loads((fixture / 'result.json').read_text())
            counts = isolation['counts']
            if not (isolation['synthetic_only'] and isolation['actual_run_joined'] and isolation['no_physical_providers'] and isolation['production_configuration'] is False and isolation['configuration_unchanged'] and isolation['admin_role_withdrawn'] and isolation['synthetic_restart_injected'] and counts['starts'] == 9 and counts['cancellations'] == 2 and counts['lookups'] == 8 and counts['lost_replies'] == 1 and counts['restarts'] == 1):
                raise RuntimeError('Logic isolation or exact mutation counts incomplete: ' + str(isolation))
        if SHARED['source_inventory'](config) != sources:
            raise RuntimeError('Logic sources changed during qualification')
        (proof / 'result.json').write_text(json.dumps(dict(status='PASS', actual_rust_go_tls=True, children_joined=True, native_rendering=False, isolation=isolation, modes=['single','restart','queued','parallel'], rows_in_capture=480, visible_known_and_unknown_gaps=True, actor_not_inferred=True, lost_start_resolved_without_replay=True, no_restart_resume=True, foreground_and_role_purge=True, limitations=['NHA163 production service remains deferred', 'Run transitions and restart are synthetic; no routine or hardware execution', 'Native rendering and interaction remain to qualify']), indent=2) + '\n')
        print('Actual logic Rust/Go TLS owner PASS ' + str(proof), flush=True)
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
