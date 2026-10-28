#!/usr/bin/env python3
"""Actual configuration owner on an explicit isolated Go Run/PG/TLS fixture.

Only synthetic source comments are activated. The simulation has declared
fixture capabilities; no production configuration or device is accepted.
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
TEST = 'administration_host::configuration::workflow::tls_probe::actual_configuration_owner_writes_plans_simulates_applies_and_rolls_back'


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Use the reserved frontend.py run --cargo environment.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf019' / ('tls-owner-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    sources = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(sources, indent=2) + '\n')
    server, binary = SHARED['PROTOCOL']['build'](proof, '^TestNHF019ConfigurationServer$')
    fixture = proof / 'fixture'
    process = client = None
    try:
        with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('wb') as client_log:
            process = subprocess.Popen([str(server), '-test.run=^TestNHF019ConfigurationServer$', '-test.v', '-test.timeout=11m'], cwd=ROOT / 'internal/server', env=dict(os.environ, HOMEWARD_NHF019_SYNTHETIC_ONLY='1', HOMEWARD_NHF019_FIXTURE=str(fixture)), stdout=server_log, stderr=subprocess.STDOUT)
            ready = SHARED['wait_file'](fixture / 'ready.json', process, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_configuration'] is False and ready['no_physical_providers'] is True and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Only an explicitly isolated loopback configuration fixture is admitted')
            client = subprocess.Popen([str(binary), '--exact', TEST, '--ignored', '--nocapture', '--test-threads=1'], cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF019_TLS_FIXTURE=str(fixture)), stdout=client_log, stderr=subprocess.STDOUT)
            if client.wait(timeout=240) != 0:
                raise RuntimeError('Configuration owner TLS failed: ' + str(proof / 'client.log'))
            if 'NHF019_CONFIGURATION_OWNER_TLS_PASS' not in (proof / 'client.log').read_text():
                raise RuntimeError('Configuration owner success marker missing')
            (fixture / 'stop').touch(mode=0o600)
            if process.wait(timeout=30) != 0:
                raise RuntimeError('Actual configuration Run did not join: ' + str(proof / 'server.log'))
            isolation = json.loads((fixture / 'result.json').read_text())
            counts = isolation['fault_counts']
            if not (isolation['synthetic_only'] and isolation['actual_run_joined'] and isolation['no_physical_providers'] and isolation['production_configuration'] is False and isolation['root_source_restored'] and isolation['after_revision'] == isolation['before_revision'] + 5 and isolation['admin_role_withdrawn'] and counts['lost'] == 1 and counts['injected'] == 3 and counts['canceled'] == 2 and counts['active'] == 0 and counts['received'] == counts['forwarded'] + counts['injected'] + counts['canceled']):
                raise RuntimeError('Configuration isolation or new-revision evidence incomplete')
        if SHARED['source_inventory'](config) != sources:
            raise RuntimeError('Configuration sources changed during qualification')
        (proof / 'result.json').write_text(json.dumps(dict(status='PASS', actual_rust_go_tls=True, children_joined=True, native_rendering=False, isolation=isolation, sql_cas=True, workspace_cas=True, aba_after_rollback=True, source_byte_limit=True, response_lost_after_actual_commit=True, injected_business_outcomes=['commit_unknown', 'committed_reconciling', 'restart_required'], foreground_cancellation=True, admin_withdrawal_purges_source=True, no_implicit_mutation_retry=True, limitations=['Business outcome fault responses are injected without forwarding Apply; they do not prove backend commit/restart behavior', 'Native configuration interactions not yet qualified']), indent=2) + '\n')
        print('Actual configuration Rust/Go TLS owner PASS ' + str(proof), flush=True)
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
