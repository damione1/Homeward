#!/usr/bin/env python3
"""Actual ListService TLS/owner qualification on a fresh isolated database.

Run under frontend.py run --cargo. No real endpoint, device Host or driver.
Native UI/delegate evidence is a separate qualification.
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
sha, wait_file, stop = (PROTOCOL[key] for key in ('sha', 'wait_file', 'stop'))
TEST = 'list_host::tls_probe::real_tls_lists_owner_and_cas'


class Control(Enum):
    OUTAGE = 'outage'
    RESTORE = 'restore-network'
    REVOKE = 'revoke-reader'
    ALLOW = 'allow-reader'
    SNAPSHOT = 'snapshot'


EXPECTED_CHANGES = (Control.OUTAGE, Control.RESTORE, Control.REVOKE, Control.ALLOW)


def qualify(proof, server_binary, client_binary):
    fixture = proof / 'fixture'
    ready_path = fixture / 'ready.json'
    server = client = None
    controls, changes, snapshots, report = [], [], [], None
    with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('w') as client_log:
        try:
            server = subprocess.Popen(
                [str(server_binary), '-test.run=^TestNHF011ListsServer$', '-test.v', '-test.timeout=9m'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF011_FIXTURE=str(fixture),
                                  HOMEWARD_NHF011_SYNTHETIC_ONLY='1'),
                stdout=server_log, stderr=subprocess.STDOUT)
            ready = wait_file(ready_path, server)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and origin.scheme == 'https'
                    and origin.hostname == '127.0.0.1' and origin.port
                    and not origin.username and not origin.password
                    and ready['seed_item_count'] == 10000):
                raise RuntimeError('Only an explicit loopback TLS synthetic fixture is admitted')
            client = subprocess.Popen(
                [str(client_binary), '--exact', TEST, '--ignored', '--nocapture', '--test-threads=1'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF011_READY=str(ready_path)),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=0)
            deadline, pending = time.monotonic() + 360, b''
            with selectors.DefaultSelector() as selector:
                selector.register(client.stdout, selectors.EVENT_READ)
                eof = False
                while not eof:
                    if time.monotonic() > deadline:
                        raise RuntimeError('Owned list probe exceeded its deadline')
                    events = selector.select(.25)
                    if not events:
                        if client.poll() is not None:
                            break
                        continue
                    chunk = os.read(client.stdout.fileno(), 65536)
                    if not chunk:
                        eof = True
                    pending += chunk
                    while b'\n' in pending:
                        raw, pending = pending.split(b'\n', 1)
                        line = raw.decode() + '\n'
                        client_log.write(line)
                        client_log.flush()
                        if 'NHF011_CONTROL ' in line:
                            command = json.loads(line.split('NHF011_CONTROL ', 1)[1])
                            action = Control(command['action'])
                            if (set(command) != {'sequence', 'action'}
                                    or command['sequence'] != len(controls) + 1 or len(controls) >= 40):
                                raise RuntimeError('Unbound or excessive list fixture control')
                            if action is not Control.SNAPSHOT:
                                if len(changes) >= len(EXPECTED_CHANGES) or action != EXPECTED_CHANGES[len(changes)]:
                                    raise RuntimeError('List fixture changes are out of order')
                                changes.append(action)
                            SHARED['acknowledge'](fixture, server, command)
                            ack = dict(command)
                            if action is Control.SNAPSHOT:
                                stats = wait_file(fixture / f"stats-{command['sequence']:02d}.json", server)
                                snapshots.append(stats)
                                ack['stats'] = stats
                            controls.append(command)
                            client.stdin.write((json.dumps(ack) + '\n').encode())
                            client.stdin.flush()
                        if 'NHF011_REPORT ' in line:
                            if report is not None:
                                raise RuntimeError('Duplicate list qualification report')
                            report = json.loads(line.split('NHF011_REPORT ', 1)[1])
            if client.wait(timeout=10) != 0 or report is None:
                raise RuntimeError('Rust list owner probe failed; inspect ' + str(proof / 'client.log'))
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('Isolated list server did not stop cleanly')
            if not (report['pass'] and report['seeded_items'] == 10000
                    and report['mutation_requests'] == 15 and report['cas_conflict']
                    and report['explicit_resolution'] and report['offline_no_replay']
                    and report['read_write_independent'] and report['read_revocation_purge']
                    and report['owners_joined'] and report['active_streams'] == 0
                    and report['action_rpc_requests'] == report['device_drivers'] == 0
                    and report['controls'] == len(controls) and tuple(changes) == EXPECTED_CHANGES):
                raise RuntimeError('Incomplete actual list host evidence')
            if not snapshots or snapshots[-1] != dict(
                    mutation_requests=15, active_streams=0, action_rpc_requests=0,
                    device_drivers=0, seeded_items=10000):
                raise RuntimeError('Server mutation/stream/safety counts differ from the client proof')
            return dict(client=report, server=snapshots[-1], controls=controls, snapshots=snapshots,
                        server_joined=True)
        finally:
            try:
                stop(client)
            finally:
                try:
                    stop(server)
                finally:
                    with contextlib.suppress(FileNotFoundError):
                        ready_path.unlink()


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Run through scripts/frontend.py run --cargo.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'lists' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(before, indent=2) + '\n')
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    server, client = PROTOCOL['build'](proof, '^TestNHF011ListsFixtureControls$')
    result = qualify(proof, server, client)
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Sources changed during list qualification')
    (proof / 'result.json').write_text(json.dumps(dict(
        status='PASS', result=result, harness_sha256=sha(proof / 'harness.py'),
        server_sha256=sha(server), client_sha256=sha(client),
        scope='Actual Rust TLS/session/list owner and real isolated Go ListService/auth/policy; '
              '10000 rows bulk-seeded only in this fresh database; '
              'headless owner proof, separate from native UI/delegates/real devices',
    ), indent=2) + '\n')
    print('Lists TLS qualification PASS: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
