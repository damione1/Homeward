#!/usr/bin/env python3
"""Real controller/TLS/SQL/Host proof on fake devices, never a production origin.

Run inside frontend.py's storage/tool reservation. Native ELF is a transport and
controller proof; APK cards and hardware validation remain separate gates.
"""
import argparse
import contextlib
from enum import Enum
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('transport_qualification', ROOT / 'scripts/qualify-frontend-transport.py')
transport = importlib.util.module_from_spec(spec)
spec.loader.exec_module(transport)

class Scenario(Enum):
    LOST_SUBMIT = 'lost-submit'
    ACK_ONLY = 'ack-only'
    EXPIRE = 'expire'
    REFUSAL = 'refusal'
    WITHDRAWN = 'withdrawn'


def wait_file(path, server):
    deadline = time.monotonic() + 60
    while not path.is_file():
        if server.poll() is not None or time.monotonic() >= deadline:
            raise RuntimeError('Isolated action fixture did not become ready')
        time.sleep(.025)
    return json.loads(path.read_text())


def qualify(proof, server_binary, client_binary, platform, scenario):
    directory = proof / (platform.value + '-' + scenario.value)
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    env = dict(os.environ, HOMEWARD_NHF006_FIXTURE=str(fixture), HOMEWARD_NHF006_SCENARIO=scenario.value)
    server = None
    remote = '/data/local/tmp/homeward-nhf006-' + proof.name + '-' + scenario.value
    port = None
    remote_created = False
    with (directory / 'server.log').open('wb') as log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF006ActionServer$', '-test.v', '-test.timeout=4m'], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
            ready = wait_file(fixture / 'ready.json', server)
            origin = urlsplit(ready['origin'])
            if ready['mode'] != 'synthetic-only' or origin.scheme != 'https' or origin.hostname != '127.0.0.1' or not origin.port:
                raise RuntimeError('Only isolated loopback TLS accepted')
            command = [str(client_binary), str(fixture / 'ready.json')]
            if platform is transport.Platform.ANDROID:
                transport.device('reverse', '--no-rebind', f'tcp:{origin.port}', f'tcp:{origin.port}')
                port = origin.port
                transport.device('shell', 'mkdir', remote)
                remote_created = True
                transport.device('shell', 'chmod', '700', remote)
                transport.device('push', client_binary, remote + '/probe')
                transport.device('push', fixture / 'ready.json', remote + '/ready.json')
                transport.device('shell', 'chmod', '700', remote + '/probe')
                transport.device('shell', 'chmod', '600', remote + '/ready.json')
                command = [str(transport.ADB), '-s', transport.SERIAL, 'shell', '-T', remote + '/probe', remote + '/ready.json']
            child = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, timeout=40)
            (directory / 'client.log').write_text(child.stdout + child.stderr)
            child.check_returncode()
            reports = [json.loads(line.removeprefix('NHF006_RESULT ')) for line in child.stdout.splitlines() if line.startswith('NHF006_RESULT ')]
            if len(reports) != 1 or not reports[0]['pass'] or not reports[0]['observed_unchanged']:
                raise RuntimeError('Missing exact native controller proof')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=25) != 0:
                raise RuntimeError('Synthetic server failed its shutdown gate')
            result = json.loads((fixture / 'result.json').read_text())
            submitted = 0 if scenario is Scenario.WITHDRAWN else 1
            dispatched = 0 if scenario in (Scenario.WITHDRAWN, Scenario.REFUSAL) else 1
            command_id = reports[0]['command_id']
            if not result['server_joined'] or result['fake_driver_calls'] != dispatched or result['command_driver_entries'] != dispatched:
                raise RuntimeError('Host/fake driver count or joined server mismatch')
            if (result['submit_ids'] or []) != [command_id] * submitted:
                raise RuntimeError('Command was resubmitted or identity changed')
            reads = (result['get_ids'] or []) + (result['watch_ids'] or [])
            if any(value != command_id for value in reads):
                raise RuntimeError('Recovery read a different command ID')
            if dispatched and not result['get_ids']:
                raise RuntimeError('Missing actual GetAction recovery')
            if scenario in (Scenario.LOST_SUBMIT, Scenario.ACK_ONLY, Scenario.EXPIRE) and not result['watch_ids']:
                raise RuntimeError('Missing actual WatchAction')
            if scenario in (Scenario.REFUSAL, Scenario.WITHDRAWN) and reads:
                raise RuntimeError('Unsent/refused action unexpectedly tracked')
            if result['lost_responses'] != int(scenario is Scenario.LOST_SUBMIT):
                raise RuntimeError('Lost-Submit fault was not observed exactly once')
            return dict(platform=platform.value, scenario=scenario.value, client=reports[0], server=result)
        finally:
            transport.stop(server, timeout=20)
            if remote_created:
                transport.device('shell', 'rm', '-rf', remote)
            if port is not None:
                transport.device('reverse', '--remove', f'tcp:{port}')
            # Synthetic passwords/tokens are ephemeral, including on failures.
            for name in ('ready.json',):
                with contextlib.suppress(FileNotFoundError):
                    (fixture / name).unlink()


def qualify_native_fixture(proof, server_binary):
    """Server readiness only: this does not run an APK or qualify a UI gesture."""
    directory = proof / 'four-family-server'
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    env = dict(os.environ, HOMEWARD_NHF006_FIXTURE=str(fixture),
               HOMEWARD_NHF006_SCENARIO=Scenario.ACK_ONLY.value,
               HOMEWARD_NHF006_DEVICES=str(ROOT / 'qualification/frontend/action-devices.json'),
               HOMEWARD_NHF005_DASHBOARD=str(ROOT / 'qualification/frontend/dashboards/actions.yaml'))
    server = None
    with (directory / 'server.log').open('wb') as log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF006ActionServer$',
                                       '-test.v', '-test.timeout=4m'], cwd=ROOT, env=env,
                                      stdout=log, stderr=subprocess.STDOUT)
            ready = wait_file(fixture / 'ready.json', server)
            if ready['mode'] != 'synthetic-only' or urlsplit(ready['origin']).hostname != '127.0.0.1':
                raise RuntimeError('Only the loopback synthetic server is accepted')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=25) != 0:
                raise RuntimeError('Four-family server did not join')
            result = json.loads((fixture / 'result.json').read_text())
            if (not result['server_joined'] or result['entity_count'] != 4
                    or result['command_driver_entries'] != 0 or result['fake_driver_calls'] != 0
                    or result['dashboard_get_calls'] != 1 or result['submit_ids']):
                raise RuntimeError('Four-family registry/dashboard readiness gate failed')
            return dict(status='PASS', server=result, native_ui_qualified=False)
        finally:
            transport.stop(server, timeout=20)
            with contextlib.suppress(FileNotFoundError):
                (fixture / 'ready.json').unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', type=transport.PlatformSelection, choices=list(transport.PlatformSelection), default=transport.PlatformSelection.DESKTOP)
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through scripts/frontend.py run --cargo.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    target = Path(os.environ['CARGO_TARGET_DIR'])
    proof = Path(config['frontend_cache']) / 'action-controller' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    server = proof / 'server.test'
    transport.run(['go', 'test', '-c', '-race', '-o', server, './internal/server'])
    native_fixture = qualify_native_fixture(proof, server)
    transport.run(['cargo', 'build', '-p', 'home-client-actions', '--example', 'nhf006_probe', '--locked'])
    results = []
    client_hashes = {}
    for platform in args.platform.platforms():
        if platform is transport.Platform.ANDROID:
            transport.run(['cargo', 'build', '-p', 'home-client-actions', '--example', 'nhf006_probe', '--target', 'aarch64-linux-android', '--locked'])
            binary = target / 'aarch64-linux-android/debug/examples/nhf006_probe'
        else:
            binary = target / 'debug/examples/nhf006_probe'
        preserved = proof / ('nhf006-probe-' + platform.value)
        shutil.copy2(binary, preserved)
        binary = preserved
        client_hashes[platform.value] = hashlib.sha256(binary.read_bytes()).hexdigest()
        context = transport.android(proof) if platform is transport.Platform.ANDROID else contextlib.nullcontext()
        with context:
            for scenario in Scenario:
                results.append(qualify(proof, server, binary, platform, scenario))
    report = dict(status='PASS', results=results, client_sha256=client_hashes,
                  four_family_server=native_fixture,
                  server_sha256=hashlib.sha256(server.read_bytes()).hexdigest())
    (proof / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
    print('Action controller qualification PASS: ' + str(proof), flush=True)

if __name__ == '__main__':
    main()
