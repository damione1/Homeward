#!/usr/bin/env python3
"""Real Rust media owner and action controller on an opt-in synthetic Go TLS mock.

Run under frontend.py run --cargo. No real device, native rendering or production
media driver is qualified by this protocol/lifecycle probe.
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
TEST = 'media_host::tls_probe::real_tls_media_owner_and_single_attempt_actions'


class Control(Enum):
    CONFIRM = 'confirm'
    RESTORE_ARTWORK = 'restore-artwork'
    DENY_ARTWORK = 'deny-artwork'
    EXPIRE_ARTWORK = 'expire-artwork'
    ABSENT_ARTWORK = 'absent-artwork'
    SWITCH_COORDINATOR = 'switch-coordinator'
    LOSE_REPLY = 'lose-next-reply'
    REVOKE_WRITE = 'revoke-write'
    ALLOW_WRITE = 'allow-write'
    WITHDRAW = 'withdraw-capabilities'
    RESTORE_CAPABILITIES = 'restore-capabilities'
    REMOVE_COORDINATOR = 'remove-coordinator'
    RESTORE_COORDINATOR = 'restore-coordinator'
    OUTAGE = 'outage'
    RESTORE_NETWORK = 'restore-network'
    REVOKE_READ = 'revoke-read'
    ALLOW_READ = 'allow-read'


EXPECTED = (
    Control.CONFIRM, Control.RESTORE_ARTWORK, Control.CONFIRM, Control.CONFIRM,
    Control.DENY_ARTWORK, Control.EXPIRE_ARTWORK, Control.ABSENT_ARTWORK,
    Control.RESTORE_ARTWORK, Control.SWITCH_COORDINATOR, Control.LOSE_REPLY,
    Control.CONFIRM, Control.REVOKE_WRITE, Control.ALLOW_WRITE,
    Control.WITHDRAW, Control.RESTORE_CAPABILITIES, Control.REMOVE_COORDINATOR,
    Control.RESTORE_COORDINATOR, Control.OUTAGE, Control.RESTORE_NETWORK,
    Control.REVOKE_READ, Control.ALLOW_READ,
)


def qualify(proof, server_binary, client_binary):
    fixture = proof / 'fixture'
    ready_path = fixture / 'ready.json'
    server = client = None
    controls, report = [], None
    with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('w') as client_log:
        try:
            server = subprocess.Popen(
                [str(server_binary), '-test.run=^TestNHF014MediaServer$', '-test.v', '-test.timeout=11m'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF014_FIXTURE=str(fixture),
                    HOMEWARD_NHF014_SOURCES=str(ROOT / 'qualification/frontend/media'),
                    HOMEWARD_NHF014_SYNTHETIC_ONLY='1'), stdout=server_log, stderr=subprocess.STDOUT)
            ready = PROTOCOL['wait_file'](ready_path, server, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_media'] is False
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1'
                    and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Explicit loopback synthetic media fixture required')
            client = subprocess.Popen(
                [str(client_binary), '--exact', TEST, '--ignored', '--nocapture', '--test-threads=1'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF014_READY=str(ready_path)),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, bufsize=0)
            deadline, pending = time.monotonic() + 300, b''
            with selectors.DefaultSelector() as selector:
                selector.register(client.stdout, selectors.EVENT_READ)
                while True:
                    if time.monotonic() > deadline:
                        raise RuntimeError('Owned media probe exceeded its deadline')
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
                        if 'NHF014_CONTROL ' in line:
                            command = json.loads(line.split('NHF014_CONTROL ', 1)[1])
                            action = Control(command['action'])
                            expected_keys = {'sequence', 'action', 'command_id'} if action is Control.CONFIRM else {'sequence', 'action'}
                            if (set(command) != expected_keys or len(controls) >= len(EXPECTED)
                                    or command['sequence'] != len(controls) + 1
                                    or action is not EXPECTED[len(controls)]
                                    or action is Control.CONFIRM and not (isinstance(command['command_id'], str) and 0 < len(command['command_id']) <= 256)):
                                raise RuntimeError('Unbound or out-of-order media fixture control')
                            SHARED['acknowledge'](fixture, server, command)
                            controls.append(command)
                            client.stdin.write((json.dumps(command) + '\n').encode())
                            client.stdin.flush()
                        if 'NHF014_REPORT ' in line:
                            if report is not None:
                                raise RuntimeError('Duplicate media report')
                            report = json.loads(line.split('NHF014_REPORT ', 1)[1])
            if client.wait(timeout=10) != 0 or report is None:
                raise RuntimeError('Actual Rust media probe failed; inspect ' + str(proof / 'client.log'))
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('Ordinary media Run did not stop cleanly')
            final = json.loads((fixture / 'result.json').read_text())
            if not (all(report[key] is True for key in (
                    'pass', 'actual_media_tls', 'shared_action_controller', 'lost_reply_recovered_same_id',
                    'stale_route_refused', 'revocation_purge', 'no_reconnect_replay', 'artwork_fallbacks', 'owners_joined'))
                    and report['native_rendering'] is False and report['production_media'] is False
                    and len(controls) == len(EXPECTED) and final['server_joined'] is True
                    and final['command_driver_entries'] == 0 and final['real_device_writes'] == 0
                    and final['synthetic_submit_calls'] == 5 and final['synthetic_mutations'] == 4
                    and final['synthetic_action_records'] == 4):
                raise RuntimeError('Incomplete media owner evidence or unexpected command replay')
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
    subprocess.run(['gofmt', '-w', 'internal/server/nhf014_media_mock_test.go',
                    'internal/server/nhf014_media_fixture_test.go'], cwd=ROOT, check=True)
    subprocess.run(['cargo', 'fmt', '--all'], cwd=ROOT, check=True)
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'media' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(before, indent=2) + '\n')
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    server, client = PROTOCOL['build'](proof, '^TestNHF014Media(RouteChanges|CanonicalZero|DescriptorAndExpiry)')
    result = qualify(proof, server, client)
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Sources changed during media qualification')
    (proof / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(dict(proof=str(proof), **result), indent=2))


if __name__ == '__main__':
    main()
