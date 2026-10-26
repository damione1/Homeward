#!/usr/bin/env python3
"""Desktop TLS/profile2/typed-control qualification on isolated fake devices.

Run through frontend.py's four leases and storage reservation. This exercises
Homeward's actual host adapters; native rendering and physical devices remain
separate qualifications. No route or fake driver is included in homed.
"""
import contextlib
from enum import Enum
import hashlib
import json
import os
from pathlib import Path
import selectors
import shutil
import signal
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
TEST = 'device_view::tls_probe::real_tls_secure_controls_and_profile2_catalogue'


class Scenario(Enum):
    ACK = 'ack-only'
    LOST = 'lost-submit'


class Control(Enum):
    CONFIRM = 'confirm-last'
    OBSERVE = 'observe-last'
    WITHDRAW = 'withdraw-cover'
    REDISCOVER = 'rediscover'
    DENY = 'deny-write'
    ALLOW = 'allow-write'


class Operation(Enum):
    COVER_OPEN = 'cover-open'
    COVER_CLOSE = 'cover-close'
    COVER_STOP = 'cover-stop'
    COVER_POSITION = 'cover-position'
    LOCK = 'lock'
    UNLOCK = 'unlock'
    ALARM_HOME = 'alarm-home'
    ALARM_AWAY = 'alarm-away'
    ALARM_DISARM = 'alarm-disarm'

    def arguments(self):
        return {
            Operation.COVER_OPEN: {'cover': {'operation': 'COVER_OPERATION_OPEN'}},
            Operation.COVER_CLOSE: {'cover': {'operation': 'COVER_OPERATION_CLOSE'}},
            Operation.COVER_STOP: {'cover': {'operation': 'COVER_OPERATION_STOP'}},
            Operation.COVER_POSITION: {'cover': {'positionPercent': 40}},
            Operation.LOCK: {'lock': {'operation': 'LOCK_OPERATION_LOCK'}},
            Operation.UNLOCK: {'lock': {'operation': 'LOCK_OPERATION_UNLOCK'}},
            Operation.ALARM_HOME: {'alarm': {'operation': 'ALARM_OPERATION_ARM_HOME'}},
            Operation.ALARM_AWAY: {'alarm': {'operation': 'ALARM_OPERATION_ARM_AWAY'}},
            Operation.ALARM_DISARM: {'alarm': {'operation': 'ALARM_OPERATION_DISARM'}},
        }[self]


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def wait_file(path, process, timeout=40):
    deadline = time.monotonic() + timeout
    while not path.is_file():
        if process.poll() is not None or time.monotonic() > deadline:
            raise RuntimeError('Owned fixture did not produce ' + path.name)
        time.sleep(.025)
    return json.loads(path.read_text())


def stop(process):
    if process is None or process.poll() is not None:
        return
    process.send_signal(signal.SIGTERM)
    try:
        process.wait(timeout=25)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=10)
        raise RuntimeError('Owned fixture required forced termination')


def build(proof, fixture_pattern='^TestNHF009Fixture'):
    server = proof / 'server.test'
    subprocess.run(['go', 'test', '-c', '-race', '-o', str(server), './internal/server'],
                   cwd=ROOT, check=True)
    subprocess.run([str(server), '-test.run=' + fixture_pattern, '-test.v', '-test.timeout=60s'],
                   cwd=ROOT / 'internal/server', check=True)
    command = ['cargo', 'test', '--locked', '-p', 'homeward-app', '--lib', '--no-run',
               '--message-format=json']
    with (proof / 'build.jsonl').open('w') as output:
        subprocess.run(command, cwd=ROOT, stdout=output, check=True)
    artifacts = [json.loads(line) for line in (proof / 'build.jsonl').read_text().splitlines()
                 if line.startswith('{')]
    binaries = [Path(item['executable']) for item in artifacts
                if item.get('reason') == 'compiler-artifact' and item.get('executable')
                and item['target']['name'] == 'homeward_app']
    if len(binaries) != 1:
        raise RuntimeError('Exactly one current host test executable required')
    client = proof / 'client.test'
    try:
        os.link(binaries[0], client)
    except OSError:
        shutil.copy2(binaries[0], client)
    return server, client


def verify(report, final, scenario):
    if not (report['pass'] and report['profile'] == 'homeward-dashboard-2'
            and report['capability_withdrawal'] and report['readonly_host_and_server_refusal']
            and report['no_real_devices']):
        raise RuntimeError('Missing actual host qualification')
    commands = report['commands']
    ids = [item['id'] for item in commands]
    operations = [Operation(item['operation']) for item in commands]
    if (len(ids) != len(set(ids)) or len(operations) != len(Operation)
            or set(operations) != set(Operation)
            or not all(item['confirmed'] and item['observation_did_not_confirm']
                       for item in commands)):
        raise RuntimeError('Every distinct typed operation must be confirmed separately')
    count = len(Operation)
    if not (final['server_joined'] and final['entity_count'] == 4
            and final['real_device_writes'] == 0
            and final['fake_driver_calls'] == final['command_driver_entries']
            == final['protocol_confirmations'] == final['observed_effects'] == count):
        raise RuntimeError('Server/fake driver count or cleanup mismatch')
    if (final['submit_ids'] != ids + [report['denied_command_id']]
            or report['denied_command_id'] in ids
            or final['lost_responses'] != int(scenario is Scenario.LOST)
            or set(final['get_ids']) != set(ids) or set(final['watch_ids']) != set(ids)):
        raise RuntimeError('A command was replayed, recovered under another ID or not tracked')
    if final['device_arguments'] != [operation.arguments() for operation in operations]:
        raise RuntimeError('Driver arguments differ from the exact typed user intentions')


def qualify(proof, server_binary, client_binary, scenario):
    directory = proof / scenario.value
    directory.mkdir(mode=0o700)
    sources = directory / 'sources'
    shutil.copytree(ROOT / 'qualification/frontend/recipes', sources)
    fixture = directory / 'fixture'
    ready_path = fixture / 'ready.json'
    server = client = None
    reports, controls = [], []
    with (directory / 'server.log').open('wb') as server_log, \
            (directory / 'client.log').open('w') as client_log:
        try:
            env = dict(os.environ, HOMEWARD_NHF009_FIXTURE=str(fixture),
                       HOMEWARD_NHF009_SOURCES=str(sources),
                       HOMEWARD_NHF007_CATALOGUE=str(sources / 'catalogue.json'),
                       HOMEWARD_NHF009_SCENARIO=scenario.value)
            env.pop('HOMEWARD_NHF005_DASHBOARD', None)
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF009ControlServer$',
                                       '-test.v', '-test.timeout=4m'], cwd=ROOT, env=env,
                                      stdout=server_log, stderr=subprocess.STDOUT)
            ready = wait_file(ready_path, server)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and origin.scheme == 'https'
                    and origin.hostname == '127.0.0.1' and origin.port
                    and ready['scenario'] == scenario.value):
                raise RuntimeError('Only an explicit loopback TLS fake-device fixture is admitted')
            client = subprocess.Popen([str(client_binary), '--exact', TEST, '--ignored',
                                       '--nocapture', '--test-threads=1'], cwd=ROOT,
                                      env=dict(os.environ, HOMEWARD_NHF009_READY=str(ready_path)),
                                      stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                      stderr=subprocess.STDOUT, bufsize=0)
            deadline = time.monotonic() + 100
            with selectors.DefaultSelector() as selector:
                selector.register(client.stdout, selectors.EVENT_READ)
                pending_bytes = b''
                while True:
                    if time.monotonic() > deadline:
                        raise RuntimeError('Owned TLS probe timed out')
                    events = selector.select(timeout=1)
                    if not events:
                        if client.poll() is not None:
                            break
                        if server.poll() is not None:
                            raise RuntimeError('Owned server ended while client was live')
                        continue
                    chunk = os.read(client.stdout.fileno(), 65536)
                    if not chunk:
                        break
                    pending_bytes += chunk
                    if len(pending_bytes) > 1 << 20:
                        raise RuntimeError('Host probe output exceeded its bounded line buffer')
                    while b'\n' in pending_bytes:
                        raw, pending_bytes = pending_bytes.split(b'\n', 1)
                        line = raw.decode('utf-8') + '\n'
                        client_log.write(line)
                        client_log.flush()
                        if 'NHF009_CONTROL ' in line:
                            command = json.loads(line.split('NHF009_CONTROL ', 1)[1])
                            action = Control(command['action'])
                            expected = {'sequence', 'action'}
                            if action in (Control.CONFIRM, Control.OBSERVE):
                                expected.add('command_id')
                            if (set(command) != expected
                                    or command['sequence'] != len(controls) + 1):
                                raise RuntimeError('Unbound or out-of-order fixture control')
                            pending = fixture / 'command.writing'
                            pending.write_text(json.dumps(command) + '\n')
                            pending.chmod(0o600)
                            pending.replace(fixture / 'command.json')
                            ack = wait_file(fixture / f"ack-{command['sequence']:02d}.json",
                                            server, timeout=15)
                            if ack['sequence'] != command['sequence'] or ack['action'] != action.value:
                                raise RuntimeError('Fixture did not acknowledge the exact control')
                            controls.append(command)
                            client.stdin.write((json.dumps({'sequence': command['sequence']})
                                                + '\n').encode())
                            client.stdin.flush()
                        if 'NHF009_RESULT ' in line:
                            reports.append(json.loads(line.split('NHF009_RESULT ', 1)[1]))
            if client.wait(timeout=10) != 0 or len(reports) != 1:
                raise RuntimeError('Actual host probe failed; inspect its preserved client log')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=25) != 0:
                raise RuntimeError('Synthetic server failed its shutdown gate')
            final = json.loads((fixture / 'result.json').read_text())
            verify(reports[0], final, scenario)
            result = dict(scenario=scenario.value, client=reports[0], server=final,
                          controls=controls, children_joined=True)
            (directory / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
            return result
        finally:
            # Attempt both joins even if one process needed forced termination.
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
    proof = Path(config['frontend_cache']) / 'recipe-controls' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    server, client = build(proof)
    results = [qualify(proof, server, client, scenario) for scenario in Scenario]
    report = dict(status='PASS', harness_sha256=sha(proof / 'harness.py'),
                  server_sha256=sha(server), client_sha256=sha(client), results=results,
                  no_real_device_actions=True,
                  scope='Actual Rust TLS/session/cache/DeviceStore/ActionController and Go '
                        'auth/authorization/registry/Host/ledger with a fake final driver; '
                        'headless host evidence, separate from native rendering and production devices')
    (proof / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
    print('Recipe/control TLS qualification PASS: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
