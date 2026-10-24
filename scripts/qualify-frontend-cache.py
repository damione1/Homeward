#!/usr/bin/env python3
"""Real native TLS/cache qualification with 850 synthetic observation sources.

Must run inside frontend.py's storage/tool reservation. The standalone Android
ELF qualifies the cache/transport; APK lifecycle and OS storage have separate proofs.
"""
import argparse
import contextlib
from enum import Enum
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import queue
import subprocess
import threading
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('transport_qualification', ROOT / 'scripts/qualify-frontend-transport.py')
transport = importlib.util.module_from_spec(spec)
spec.loader.exec_module(transport)


class Scenario(Enum):
    LOAD = 'load'
    RECOVERY = 'recovery'
    SLOW_STREAM = 'slow-stream'
    LAST_PAGE_EXPIRED = 'last-page-expired'
    LAST_PAGE_DENIED = 'last-page-denied'

    def __str__(self):
        return self.value


class Action(Enum):
    ADVANCE = 'advance'
    LOAD = 'load'
    BURST = 'burst'
    OUTAGE = 'outage'
    RESTORE = 'restore'
    DENY = 'deny'
    ALLOW = 'allow'
    WITHDRAW = 'withdraw'
    RESTART = 'restart'


def wait_file(path, server, seconds=60):
    deadline = time.monotonic() + seconds
    while not path.is_file():
        if server.poll() is not None or time.monotonic() >= deadline:
            raise RuntimeError('Synthetic core/barrier did not become ready: ' + path.name)
        time.sleep(.025)
    return json.loads(path.read_text())


def qualify(proof, server_binary, client_binary, platform, scenario):
    directory = proof / (platform.value + '-' + scenario.value)
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    remote = '/data/local/tmp/homeward-nhf004-' + proof.name + '-' + scenario.value
    server = child = reader = None
    private = report = None
    lines = queue.Queue(maxsize=64)
    port = None
    remote_created = False
    cleanup_errors = []
    env = dict(os.environ, HOMEWARD_NHF004_FIXTURE=str(fixture), HOMEWARD_NHF004_APP='false')
    with (directory / 'server.log').open('wb') as log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF004CacheServer$', '-test.v', '-test.timeout=13m'], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
            ready = fixture / 'ready.json'
            private = wait_file(ready, server)
            origin = urlsplit(private['origin'])
            if private['mode'] != 'synthetic-only' or private['entity_count'] != 850 or origin.scheme != 'https' or origin.hostname != '127.0.0.1' or not origin.port:
                raise RuntimeError('Only isolated loopback 850-entity TLS fixtures accepted.')
            args = [str(client_binary), str(ready), scenario.value]
            if platform is transport.Platform.ANDROID:
                transport.device('reverse', '--no-rebind', f'tcp:{origin.port}', f'tcp:{origin.port}')
                port = origin.port
                transport.device('shell', 'mkdir', remote)
                remote_created = True
                transport.device('shell', 'chmod', '700', remote)
                transport.device('push', client_binary, remote + '/probe')
                transport.device('push', ready, remote + '/ready.json')
                transport.device('shell', 'chmod', '700', remote + '/probe')
                transport.device('shell', 'chmod', '600', remote + '/ready.json')
                args = [str(transport.ADB), '-s', transport.SERIAL, 'shell', '-T', remote + '/probe', remote + '/ready.json', scenario.value]
            child = subprocess.Popen(args, cwd=ROOT, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)

            def pump():
                for line in child.stdout:
                    lines.put(line)
                lines.put(None)

            reader = threading.Thread(target=pump, daemon=True)
            reader.start()
            deadline = time.monotonic() + 300
            last_command = 0
            with (directory / 'client.log').open('w') as client_log:
                while True:
                    if time.monotonic() >= deadline:
                        raise RuntimeError('Native cache probe deadline exceeded.')
                    try:
                        line = lines.get(timeout=.2)
                    except queue.Empty:
                        continue
                    if line is None:
                        break
                    for secret in [private['password'], private['login_name']]:
                        line = line.replace(secret, '<redacted>')
                    client_log.write(line)
                    client_log.flush()
                    if line.startswith('NHF004_CONTROL '):
                        command = json.loads(line.removeprefix('NHF004_CONTROL '))
                        command['action'] = Action(command['action']).value
                        if command['sequence'] != last_command + 1 or not 1 <= command['sequence'] <= 64:
                            raise RuntimeError('Out of order synthetic control.')
                        temporary = fixture / 'command.writing'
                        with open(temporary, 'x', opener=lambda p, flags: os.open(p, flags, 0o600)) as output:
                            json.dump(command, output)
                        temporary.replace(fixture / 'command.json')
                        ack = wait_file(fixture / f'ack-{command["sequence"]:02d}.json', server)
                        if ack['sequence'] != command['sequence'] or ack['action'] != command['action']:
                            raise RuntimeError('Wrong synthetic control acknowledgement.')
                        child.stdin.write(json.dumps(ack) + '\n')
                        child.stdin.flush()
                        last_command = command['sequence']
                    elif line.startswith('NHF004_RESULT '):
                        report = json.loads(line.removeprefix('NHF004_RESULT '))
            if child.wait(timeout=5) != 0 or report is None:
                raise RuntimeError('Native cache probe failed; inspect client.log.')
        finally:
            transport.stop(child)
            if reader is not None:
                deadline = time.monotonic() + 5
                while reader.is_alive() and time.monotonic() < deadline:
                    try:
                        lines.get(timeout=.1)
                    except queue.Empty:
                        pass
                    reader.join(timeout=.05)
                if reader.is_alive():
                    cleanup_errors.append('Native pipe reader did not join.')
            if platform is transport.Platform.ANDROID:
                for needed, command in [(remote_created, ('shell', 'rm', '-rf', remote)),
                                        (port is not None, ('reverse', '--remove', f'tcp:{port}'))]:
                    if needed:
                        try:
                            transport.device(*command)
                        except (OSError, subprocess.SubprocessError):
                            cleanup_errors.append('Owned emulator cleanup failed.')
            if server is not None:
                if fixture.exists():
                    (fixture / 'stop').touch(mode=0o600)
                try:
                    server.wait(timeout=25)
                except subprocess.TimeoutExpired:
                    transport.stop(server, timeout=15)
            if cleanup_errors:
                raise RuntimeError(' '.join(cleanup_errors))
    server_report = json.loads((fixture / 'result.json').read_text())
    if server.returncode != 0 or server_report['servers_started'] != server_report['servers_joined'] or server_report['connections_remaining'] != 0 or server_report['command_driver_entries'] != 0:
        raise RuntimeError('Synthetic server did not join cleanly or entered a driver.')
    if report['os'] != ('android' if platform is transport.Platform.ANDROID else 'macos') or report['scenario'] != scenario.value or not report['session_revoked']:
        raise RuntimeError('Wrong native execution or session cleanup.')
    result = {'status': 'PASS', 'client': report, 'server': server_report}
    if scenario is Scenario.LOAD:
        load = json.loads((fixture / 'load-done.json').read_text())
        if load['updates'] != 1000 or not 49 <= load['elapsed_seconds'] <= 70 or report['details']['ordered_updates'] != 1000:
            raise RuntimeError('20 updates/s native load not demonstrated.')
        result['load'] = load
    (directory / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result), flush=True)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', type=transport.PlatformSelection, choices=list(transport.PlatformSelection), default=transport.PlatformSelection.ALL)
    parser.add_argument('--scenario', type=Scenario, choices=list(Scenario), action='append')
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through scripts/frontend.py run --cargo.')
    proof = Path(transport.CONFIG['frontend_cache']) / 'cache' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    server = proof / 'server.test'
    transport.run(['go', 'test', '-c', '-race', '-o', server, './internal/server'])
    hashes = {'server': hashlib.sha256(server.read_bytes()).hexdigest()}
    results = []
    for platform in args.platform.platforms():
        target_args = ['--target', 'aarch64-linux-android'] if platform is transport.Platform.ANDROID else []
        transport.run(['cargo', 'build', '--locked', '-p', 'home-client-cache', '--example', 'nhf004_probe', *target_args])
        binary = Path(os.environ['CARGO_TARGET_DIR'])
        if platform is transport.Platform.ANDROID:
            binary /= 'aarch64-linux-android'
        binary /= 'debug/examples/nhf004_probe'
        hashes[platform.value] = hashlib.sha256(binary.read_bytes()).hexdigest()
        with transport.android(proof) if platform is transport.Platform.ANDROID else contextlib.nullcontext():
            for scenario in args.scenario or list(Scenario):
                results.append(qualify(proof, server, binary, platform, scenario))
    (proof / 'result.json').write_text(json.dumps({'status': 'PASS', 'hashes': hashes, 'cases': results}, indent=2) + '\n')
    print('Cache qualification PASS: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
