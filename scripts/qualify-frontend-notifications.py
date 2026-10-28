#!/usr/bin/env python3
"""Notification protocol over actual Rust/Go-race/native-session TLS.

Explicit fresh synthetic PostgreSQL/CA only. This is not an OS collector,
native UI, lifecycle or Android qualification. Run through frontend.py run.
"""
import argparse
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
class Probe(Enum):
    TRANSPORT = 'transport'
    OWNER = 'owner'
    ALL = 'all'

    def __str__(self):
        return self.value

    def spec(self):
        match self:
            case Probe.TRANSPORT:
                return ('nhf022_notifications_tls', ['-p', 'home-client-transport', '--test', 'nhf022_notifications_tls'],
                        'actual_notifications_tls_private_inbox_receipts_targets_and_revocation',
                        'NHF022_NOTIFICATIONS_TRANSPORT_TLS_PASS', dict(registrations=2, updates=4, receipts=7, revocations=1, seeded=8, opens=2, lost_replies=3))

            case Probe.OWNER:
                return ('homeward_app', ['-p', 'homeward-app', '--lib'],
                        'notification_host::tls_probe::actual_notification_owner_inbox_native_reservations_and_revocation',
                        'NHF022_NOTIFICATION_OWNER_TLS_PASS', dict(registrations=3, updates=6, receipts=13, revocations=1, seeded=8, opens=2, lost_replies=3))
            case Probe.ALL:
                raise ValueError('Choose a leaf probe')


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Use the reserved frontend.py run --cargo environment')
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--probe', type=Probe, choices=list(Probe), default=Probe.ALL)
    args = parser.parse_args()
    selected = (Probe.TRANSPORT, Probe.OWNER) if args.probe is Probe.ALL else (args.probe,)
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf022' / ('tls-' + args.probe.value + '-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    sources = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(sources, indent=2) + '\n')
    server = proof / 'server.test'
    subprocess.run(['go', 'test', '-c', '-race', '-o', str(server), './internal/server'], cwd=ROOT, check=True)
    subprocess.run([str(server), '-test.run=^TestNHF022NotificationServer$', '-test.v'], cwd=ROOT, check=True)
    for probe in selected:
        run_probe(proof / probe.value, server, probe, sources, config)
    if SHARED['source_inventory'](config) != sources:
        raise RuntimeError('Notification source inputs changed during qualification')
    (proof / 'result.json').write_text(json.dumps(dict(status='PASS', probes=[p.value for p in selected], children_joined=True, actual_rust_go_tls=True, native_rendering=False), indent=2) + '\n')
    print('Notification actual Rust/Go TLS PASS ' + str(proof), flush=True)


def run_probe(proof, server, probe, sources, config):
    proof.mkdir(mode=0o700)
    target, cargo_arguments, test, marker, expected_counts = probe.spec()
    built = subprocess.run(['cargo', 'test', '--locked', *cargo_arguments, '--no-run', '--message-format=json'], cwd=ROOT, text=True, stdout=subprocess.PIPE, check=False)
    (proof / 'build.jsonl').write_text(built.stdout)
    built.check_returncode()
    artifacts = [json.loads(line) for line in built.stdout.splitlines() if line.startswith('{')]
    binaries = [a['executable'] for a in artifacts if a.get('reason') == 'compiler-artifact' and a.get('executable') and a['target']['name'] == target]
    if len(binaries) != 1:
        raise RuntimeError('Exactly one current notification artifact required')
    (proof / 'artifact.json').write_text(json.dumps(dict(server=str(server), server_sha256=SHARED['sha'](server), client=binaries[0], client_sha256=SHARED['sha'](Path(binaries[0]))), indent=2) + '\n')
    fixture = proof / 'fixture'
    process = client = None
    try:
        with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('wb') as client_log:
            process = subprocess.Popen([str(server), '-test.run=^TestNHF022NotificationServer$', '-test.v', '-test.timeout=11m'], cwd=ROOT / 'internal/server', env=dict(os.environ, HOMEWARD_NHF022_FIXTURE=str(fixture), HOMEWARD_NHF022_SYNTHETIC_ONLY='1'), stdout=server_log, stderr=subprocess.STDOUT)
            ready = SHARED['wait_file'](fixture / 'ready.json', process, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_configuration'] is False and ready['no_physical_providers'] is True and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Explicit isolated loopback notification fixture required')
            client = subprocess.Popen([binaries[0], '--exact', test, '--ignored', '--nocapture', '--test-threads=1'], cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF022_TLS_FIXTURE=str(fixture)), stdout=client_log, stderr=subprocess.STDOUT)
            if client.wait(timeout=180) != 0 or marker not in (proof / 'client.log').read_text():
                raise RuntimeError('Notification transport TLS failed: ' + str(proof / 'client.log'))
            (fixture / 'stop').touch(mode=0o600)
            if process.wait(timeout=30) != 0:
                raise RuntimeError('Notification Run did not join: ' + str(proof / 'server.log'))
            isolation = json.loads((fixture / 'result.json').read_text())
            if not (isolation['synthetic_only'] and isolation['actual_run_joined'] and isolation['no_physical_providers'] and isolation['configuration_unchanged'] and isolation['production_configuration'] is False and isolation['account_revoked'] and isolation['counts'] == expected_counts and isolation['companion_counts'] == dict(registrations=2 if probe is Probe.OWNER else 1, consent_changes=0, publications=0, revocations=1, lost_replies=0)):
                raise RuntimeError('Incomplete notification isolation or exact counts: ' + str(isolation))
        if SHARED['source_inventory'](config) != sources:
            raise RuntimeError('Notification source inputs changed during qualification')
        (proof / 'result.json').write_text(json.dumps(dict(status='PASS', probe=probe.value, actual_rust_go_tls=True, children_joined=True, isolation=isolation, native_account_session_bound=True, default_native_delivery_off=True, installation_and_account_revocation=True, owner_integration=probe is Probe.OWNER, limitations=['Measurements and permissions are injected, not OS evidence', 'Native rendering and Android qualification remain separate', 'Production notification service remains deferred', 'OS permission and presentation receipts are injected for protocol qualification']), indent=2) + '\n')
        print('Notification ' + probe.value + ' actual Rust/Go TLS PASS ' + str(proof), flush=True)
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
