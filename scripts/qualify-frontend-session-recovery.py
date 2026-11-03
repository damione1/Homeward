#!/usr/bin/env python3
"""Opt-in real PostgreSQL/Go TLS ↔ Rust SessionCoordinator recovery proof.

Run through scripts/frontend.py run --cargo. Faults exist only in the isolated
Go test fixture. Synthetic durable stores remain in private proof directories;
this never reads the household profile, production Keychain or physical devices.
"""
import argparse
from enum import Enum
import hashlib
import json
import os
from pathlib import Path
import queue
import subprocess
import threading
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]


class Probe(str, Enum):
    LOSE = 'lose'
    RESTORE = 'restore'
    RESTORE_OUTAGE = 'restore-outage'
    RESTORE_BARE_RENEWAL = 'restore-bare-renewal'
    BARE_PREFLIGHT = 'bare-preflight'
    RESTORE_REVOKED = 'restore-revoked'
    RESTORE_EXPIRED = 'restore-expired'
    CLEANUP = 'cleanup'


class Scenario(str, Enum):
    RECOVERY = 'recovery'
    OUTAGE = 'outage'
    BARE_PREFLIGHT = 'bare-preflight'
    BARE_RENEWAL = 'bare-renewal'
    REVOKED = 'revoked'
    EXPIRED = 'expired'

    def probes(self):
        match self:
            case Scenario.RECOVERY:
                return (Probe.LOSE, Probe.RESTORE)
            case Scenario.OUTAGE:
                return (Probe.LOSE, Probe.RESTORE_OUTAGE)
            case Scenario.BARE_PREFLIGHT:
                return (Probe.BARE_PREFLIGHT,)
            case Scenario.BARE_RENEWAL:
                return (Probe.LOSE, Probe.RESTORE_BARE_RENEWAL)
            case Scenario.REVOKED:
                return (Probe.LOSE, Probe.RESTORE_REVOKED)
            case Scenario.EXPIRED:
                return (Probe.LOSE, Probe.RESTORE_EXPIRED)
        raise AssertionError('Unhandled typed session scenario')


class Action(str, Enum):
    DROP_REFRESH = 'drop-refresh'
    OUTAGE = 'outage'
    RESTORE = 'restore'
    BARE_PREFLIGHT = 'bare-preflight'
    BARE_RENEWAL = 'bare-renewal'
    REVOKE = 'revoke'
    EXPIRE = 'expire'


def write_private(path, value):
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as output:
        json.dump(value, output, indent=2)
        output.write('\n')


def wait_file(path, server, timeout=45):
    deadline = time.monotonic() + timeout
    while not path.is_file():
        if server.poll() is not None or time.monotonic() >= deadline:
            raise RuntimeError('Isolated session barrier failed: ' + path.name)
        time.sleep(.025)
    return json.loads(path.read_text())


def stop(child, timeout=15):
    if child is None or child.poll() is not None:
        return
    child.terminate()
    try:
        child.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        child.kill()
        child.wait(timeout=5)


def probe(binary, ready, scenario, sequence, server, control, directory):
    """One real process per phase: restart cannot reuse Rust in-memory state."""
    child = subprocess.Popen([str(binary), str(ready), scenario.value, str(sequence)], cwd=ROOT,
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                             text=True, bufsize=1)
    lines = queue.Queue(maxsize=64)

    def read_lines():
        for line in child.stdout:
            lines.put(line)
        lines.put(None)

    reader = threading.Thread(target=read_lines, daemon=True)
    reader.start()
    report = None
    deadline = time.monotonic() + 45
    try:
        with (directory / (scenario.value + '.log')).open('w') as output:
            while True:
                if time.monotonic() >= deadline:
                    raise RuntimeError('Native session probe exceeded deadline')
                try:
                    line = lines.get(timeout=.2)
                except queue.Empty:
                    continue
                if line is None:
                    break
                output.write(line)
                output.flush()
                if line.startswith('NHF032_CONTROL '):
                    command = json.loads(line.removeprefix('NHF032_CONTROL '))
                    command['action'] = Action(command['action']).value
                    if set(command) != {'sequence', 'action'} or command['sequence'] != sequence + 1:
                        raise RuntimeError('Out-of-order synthetic session control')
                    temporary = control / 'command.writing'
                    write_private(temporary, command)
                    temporary.replace(control / 'command.json')
                    ack = wait_file(control / f'ack-{command["sequence"]:02d}.json', server)
                    if ack != command:
                        raise RuntimeError('Wrong synthetic session acknowledgement')
                    child.stdin.write(json.dumps(ack) + '\n')
                    child.stdin.flush()
                    sequence += 1
                elif line.startswith('NHF032_RESULT '):
                    report = json.loads(line.removeprefix('NHF032_RESULT '))
            if child.wait(timeout=5) != 0 or report is None or report['scenario'] != scenario.value:
                raise RuntimeError('Native session probe failed; inspect ' + scenario.value + '.log')
    finally:
        stop(child)
        reader.join(timeout=5)
        if reader.is_alive():
            raise RuntimeError('Owned session probe reader did not join')
    return sequence, report


def qualify(proof, server_binary, client_binary, scenario):
    directory = proof / scenario.value
    directory.mkdir(mode=0o700)
    control = directory / 'fixture'
    env = dict(os.environ, HOMEWARD_NHF032_FIXTURE=str(control))
    server = None
    private = None
    sequence = 0
    reports = []
    try:
        with (directory / 'server.log').open('wb') as log:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF032SessionRecoveryServer$',
                                       '-test.v', '-test.timeout=6m'], cwd=ROOT, env=env,
                                      stdout=log, stderr=subprocess.STDOUT)
            ready = control / 'ready.json'
            private = wait_file(ready, server)
            origin = urlsplit(private['origin'])
            if private['mode'] != 'synthetic-only' or origin.scheme != 'https' or origin.hostname != '127.0.0.1' or not origin.port:
                raise RuntimeError('Only isolated loopback session fixtures accepted')
            for phase in scenario.probes():
                sequence, report = probe(client_binary, ready, phase, sequence, server, control, directory)
                reports.append(report)
            if (control / 'synthetic-session.bin').exists():
                raise RuntimeError('Successful session proof left a secret record')
    finally:
        cleanup_errors = []
        if private is not None and (control / 'synthetic-session.bin').exists():
            # Local-only cleanup does not authenticate or contact the server.
            try:
                probe(client_binary, control / 'ready.json', Probe.CLEANUP, sequence, server, control, directory)
            except (OSError, RuntimeError, subprocess.SubprocessError) as error:
                cleanup_errors.append('Synthetic credential cleanup failed: ' + str(error))
            if (control / 'synthetic-session.bin').exists():
                cleanup_errors.append('Synthetic credential record remains')
        if server is not None:
            if control.is_dir():
                (control / 'stop').touch(mode=0o600)
            try:
                server.wait(timeout=25)
            except subprocess.TimeoutExpired:
                stop(server)
        if cleanup_errors:
            raise RuntimeError('; '.join(cleanup_errors))
    server_report = json.loads((control / 'result.json').read_text())
    if server.returncode != 0 or not server_report['server_joined'] or server_report['action_requests'] != 0:
        raise RuntimeError('Isolated session server failed to join cleanly')
    expected_lost = 0 if scenario is Scenario.BARE_PREFLIGHT else 1
    expected_refresh = 1 if scenario is Scenario.BARE_PREFLIGHT else 2
    if (server_report['start_login_calls'] != 1 or server_report['lost_refresh_responses'] != expected_lost
            or server_report['refresh_calls'] != expected_refresh
            or server_report['keyed_refresh_calls'] != expected_refresh or not server_report['same_renewal_key']):
        raise RuntimeError('Login, committed response loss or persisted operation reuse was not demonstrated')
    if scenario in (Scenario.RECOVERY, Scenario.OUTAGE, Scenario.BARE_PREFLIGHT, Scenario.BARE_RENEWAL):
        if (not server_report['sliding_deadline_advanced'] or not server_report['seven_day_inactivity']
                or server_report['current_generation'] != 2 or server_report['retained_generations'] != 2):
            raise RuntimeError('Native recovery did not preserve one rotation and advance the seven-day deadline')
        if not reports[-1]['protected_read'] or not reports[-1]['server_revoked']:
            raise RuntimeError('Recovered session did not support protected read and cleanup')
    elif not reports[-1]['typed_terminal_stopped']:
        raise RuntimeError('Definitive terminal session error did not stop recovery')
    result = {'status': 'PASS', 'scenario': scenario.value, 'client_processes': reports, 'server': server_report}
    write_private(directory / 'result.json', result)
    print(json.dumps(result), flush=True)
    return result


def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scenario', type=Scenario, choices=list(Scenario), action='append')
    parser.add_argument('--proof', type=Path)
    parser.add_argument('--server-binary', type=Path)
    parser.add_argument('--client-binary', type=Path)
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through scripts/frontend.py run --cargo')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = args.proof.resolve() if args.proof else Path(config['frontend_cache']) / 'session-recovery' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    server = args.server_binary or proof / 'server.test'
    client = args.client_binary or Path(os.environ['CARGO_TARGET_DIR']) / 'debug/examples/native_session_recovery_probe'
    if args.server_binary is None:
        subprocess.run(['go', 'test', '-c', '-race', '-o', str(server), './internal/server'], cwd=ROOT, check=True, timeout=300)
    if args.client_binary is None:
        subprocess.run(['cargo', 'build', '--locked', '-p', 'home-client-platform', '--example', 'native_session_recovery_probe'], cwd=ROOT, check=True, timeout=300)
    results = [qualify(proof, server.resolve(), client.resolve(), scenario) for scenario in args.scenario or list(Scenario)]
    write_private(proof / 'result.json', {'status': 'PASS', 'server_sha256': digest(server), 'client_sha256': digest(client), 'cases': results})
    print('Native session recovery qualification PASS: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
