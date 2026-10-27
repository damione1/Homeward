#!/usr/bin/env python3
"""Actual Calendar TLS contract and native screen owner on isolated Go/SQL mocks.

Run under frontend.py run --cargo. No production CalendarService, device driver,
physical device, native rendering, IME or screen reader is qualified here.
"""
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import runpy
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
PROTOCOL = SHARED['PROTOCOL']


class Probe(Enum):
    CONTRACT = ('contract', 'actual_calendar_tls_mutations_fold_resync_and_uncertainty', 'NHF015_CALENDAR_TLS_PASS')
    OWNER = ('owner', 'calendar_host::tls_probe::actual_calendar_owner_tls_cas_uncertainty_and_revocation', 'NHF015_OWNER_TLS_PASS')


def qualify(directory, server_binary, client_binary, probe):
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    server = client = None
    _, test, marker = probe.value
    with (directory / 'server.log').open('wb') as server_log, (directory / 'client.log').open('wb') as client_log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF015CalendarServer$',
                '-test.v', '-test.timeout=6m'], cwd=ROOT / 'internal/server',
                env=dict(os.environ, HOMEWARD_NHF015_SYNTHETIC_ONLY='1', HOMEWARD_NHF015_EMPTY_LOCAL='1' if probe is Probe.OWNER else '',
                    HOMEWARD_NHF015_FIXTURE=str(fixture), HOMEWARD_NHF015_SOURCES=str(ROOT / 'qualification/frontend/calendars')),
                stdout=server_log, stderr=subprocess.STDOUT)
            ready = SHARED['wait_file'](fixture / 'ready.json', server, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_calendars'] is False
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1'
                    and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Explicit isolated loopback Calendar fixture required')
            command = [str(client_binary), '--exact', test, '--nocapture', '--test-threads=1']
            if probe is Probe.OWNER:
                command.append('--ignored')
            client = subprocess.Popen(command, cwd=ROOT,
                env=dict(os.environ, HOMEWARD_NHF015_TLS_FIXTURE=str(fixture)),
                stdout=client_log, stderr=subprocess.STDOUT)
            if client.wait(timeout=180) != 0:
                raise RuntimeError('Calendar probe failed: ' + str(directory / 'client.log'))
            if marker not in (directory / 'client.log').read_text():
                raise RuntimeError('Actual Calendar probe success marker missing')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('Owned Calendar server failed to join')
            final = json.loads((fixture / 'result.json').read_text())
            if not (final['synthetic_only'] and final['real_device_writes'] == 0
                    and final['real_device_driver_instantiated'] is False):
                raise RuntimeError('Calendar isolation evidence is incomplete')
            return dict(status='PASS', probe=probe.value[0], actual_rust_go_tls=True,
                        native_rendering=False, production_calendars=False, children_joined=True)
        finally:
            SHARED['stop'](client)
            if server is not None and server.poll() is None:
                if fixture.is_dir():
                    (fixture / 'stop').touch(mode=0o600)
                with contextlib.suppress(subprocess.TimeoutExpired):
                    server.wait(timeout=30)
            SHARED['stop'](server)
            with contextlib.suppress(FileNotFoundError):
                (fixture / 'ready.json').unlink()


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Run under frontend.py run --cargo.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf015' / ('tls-owner-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(before, indent=2) + '\n')
    server, owner = PROTOCOL['build'](proof, '^TestNHF015')
    command = ['cargo', 'test', '--locked', '-p', 'home-client-transport', '--test',
               'nhf015_calendars_tls', '--no-run', '--message-format=json']
    build = subprocess.run(command, cwd=ROOT, text=True, stdout=subprocess.PIPE, check=False)
    (proof / 'transport-build.jsonl').write_text(build.stdout)
    build.check_returncode()
    binaries = [json.loads(line)['executable'] for line in build.stdout.splitlines()
                if line.startswith('{') and json.loads(line).get('reason') == 'compiler-artifact'
                and json.loads(line).get('target', {}).get('name') == 'nhf015_calendars_tls'
                and json.loads(line).get('executable')]
    if len(binaries) != 1:
        raise RuntimeError('One exact Cargo Calendar transport test artifact required')
    results = []
    for probe in Probe:
        binary = owner if probe is Probe.OWNER else Path(binaries[0])
        results.append(qualify(proof / probe.value[0], server, binary, probe))
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Calendar source inputs changed during qualification')
    (proof / 'result.json').write_text(json.dumps(dict(status='PASS', probes=results), indent=2) + '\n')
    print('Calendar contract and owner TLS PASS ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
