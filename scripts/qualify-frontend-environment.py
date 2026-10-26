#!/usr/bin/env python3
"""Actual Rust TLS/cache/host environment qualification on isolated Go inputs.

Run through frontend.py run --cargo. No device driver or production endpoint.
Native rendering and future extension enums remain separate qualifications.
"""
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import runpy
import selectors
import shutil
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
PROTOCOL = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-recipes.py'))
TEST = 'environment_probe::real_tls_environment_profiles_and_private_cache'
sha, wait_file, stop = (PROTOCOL[key] for key in ('sha', 'wait_file', 'stop'))


class Control(Enum):
    STALE = 'stale'
    UNKNOWN = 'unknown'
    ABSENT = 'absent'
    MISSING = 'missing'
    NOMINAL = 'nominal'
    PARIS = 'profile-paris'
    TORONTO = 'profile-toronto'
    REVOKE = 'revoke-details'
    ALLOW = 'allow-details'


def source_inventory(config):
    roots = dict(homeward=ROOT, free=Path(config['free_kit']), pro=Path(config['pro_kit']))
    result = {}
    for key, root in roots.items():
        paths = subprocess.check_output(
            ['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'], cwd=root
        ).decode().split('\0')
        result[key] = {p: sha(root / p) for p in paths if p and (root / p).is_file()}
    return result


def acknowledge(fixture, process, command):
    pending = fixture / 'command.writing'
    pending.write_text(json.dumps(command) + '\n')
    pending.chmod(0o600)
    pending.replace(fixture / 'command.json')
    deadline = time.monotonic() + 15
    while process.poll() is None and time.monotonic() < deadline:
        path = fixture / f"ack-{command['sequence']:02d}.json"
        if path.exists():
            ack = json.loads(path.read_text())
            if ack['sequence'] == command['sequence']:
                if ack != command:
                    raise RuntimeError('fixture acknowledged a different control')
                return
            if ack['sequence'] > command['sequence']:
                raise RuntimeError('fixture skipped a control')
        time.sleep(.025)
    raise RuntimeError('owned fixture did not acknowledge control')


def qualify(proof, server_binary, client_binary):
    source = proof / 'sources'
    shutil.copytree(ROOT / 'qualification/frontend/environment', source)
    fixture = proof / 'fixture'
    ready_path = fixture / 'ready.json'
    server = client = None
    controls, report = [], None
    with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('w') as client_log:
        try:
            env = dict(os.environ, HOMEWARD_NHF010_FIXTURE=str(fixture),
                       HOMEWARD_NHF010_SOURCES=str(source),
                       HOMEWARD_NHF007_CATALOGUE=str(source / 'native-catalogue.json'))
            env.pop('HOMEWARD_NHF005_DASHBOARD', None)
            env.pop('HOMEWARD_NHF010_SMOKE', None)
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF010EnvironmentServer$',
                                       '-test.v', '-test.timeout=6m'], cwd=ROOT, env=env,
                                      stdout=server_log, stderr=subprocess.STDOUT)
            ready = wait_file(ready_path, server)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and origin.scheme == 'https'
                    and origin.hostname == '127.0.0.1' and origin.port):
                raise RuntimeError('only an explicit loopback TLS fixture is admitted')
            client = subprocess.Popen([str(client_binary), '--exact', TEST, '--ignored',
                                       '--nocapture', '--test-threads=1'], cwd=ROOT,
                                      env=dict(os.environ, HOMEWARD_NHF010_READY=str(ready_path)),
                                      stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                      stderr=subprocess.STDOUT, bufsize=0)
            deadline = time.monotonic() + 240
            with selectors.DefaultSelector() as selector:
                selector.register(client.stdout, selectors.EVENT_READ)
                pending = b''
                while True:
                    if time.monotonic() > deadline:
                        raise RuntimeError('owned environment probe timed out')
                    if not selector.select(timeout=1):
                        if client.poll() is not None:
                            break
                        if server.poll() is not None:
                            raise RuntimeError('owned server ended while client was live')
                        continue
                    chunk = os.read(client.stdout.fileno(), 65536)
                    if not chunk:
                        break
                    pending += chunk
                    if len(pending) > 1 << 20:
                        raise RuntimeError('probe output exceeded bounded line buffer')
                    while b'\n' in pending:
                        raw, pending = pending.split(b'\n', 1)
                        line = raw.decode() + '\n'
                        client_log.write(line)
                        client_log.flush()
                        if 'NHF010_CONTROL ' in line:
                            command = json.loads(line.split('NHF010_CONTROL ', 1)[1])
                            action = Control(command['action'])
                            if (set(command) != {'sequence', 'action'}
                                    or command['sequence'] != len(controls) + 1
                                    or action != list(Control)[len(controls)]):
                                raise RuntimeError('unbound or out of order fixture control')
                            acknowledge(fixture, server, command)
                            controls.append(command)
                            client.stdin.write((json.dumps({'sequence': command['sequence']}) + '\n').encode())
                            client.stdin.flush()
                        if 'NHF010_RESULT ' in line:
                            if report is not None:
                                raise RuntimeError('duplicate result')
                            report = json.loads(line.split('NHF010_RESULT ', 1)[1])
            if client.wait(timeout=10) != 0 or report is None:
                raise RuntimeError('Rust environment probe failed; inspect ' + str(proof / 'client.log'))
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('isolated environment server did not stop cleanly')
            final = wait_file(fixture / 'result.json', server)
            if not (report['pass'] and report['guest_entities'] == 3 and report['member_entities'] == 4
                    and report['typed_host_cards'] and report['ordered_watch']
                    and report['profile_hot_reload'] and report['private_purge']
                    and report['no_real_devices'] and report['controls'] == len(Control)
                    and len(controls) == len(Control)):
                raise RuntimeError('incomplete actual Rust host evidence')
            if not (final['server_joined'] and final['real_device_writes'] == 0
                    and final['command_driver_entries'] == final['fake_driver_calls'] == 0
                    and not final['submit_ids'] and final['entity_count'] == 4
                    and final['native_login_calls'] == final['native_revokes'] == 2):
                raise RuntimeError('server/auth/readonly count or cleanup mismatch')
            return dict(client=report, server=final, controls=controls)
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
    proof = Path(config['frontend_cache']) / 'environment' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    before = source_inventory(config)
    (proof / 'sources.json').write_text(json.dumps(before, indent=2) + '\n')
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    server, client = PROTOCOL['build'](proof, '^TestNHF010(EnvironmentalFixture|FixtureControls)')
    result = qualify(proof, server, client)
    if source_inventory(config) != before:
        raise RuntimeError('sources changed during qualification')
    (proof / 'result.json').write_text(json.dumps(dict(
        status='PASS', result=result, harness_sha256=sha(proof / 'harness.py'),
        server_sha256=sha(server), client_sha256=sha(client),
        scope='Actual Rust TLS/session/cache/DeviceStore and isolated Go auth/policy/Host; '
              'headless host evidence, separate from native UI and production devices',
    ), indent=2) + '\n')
    print('Environment TLS qualification PASS: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
