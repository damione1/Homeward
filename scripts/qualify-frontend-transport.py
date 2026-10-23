#!/usr/bin/env python3
"""Real TLS qualification on macOS and a task-owned Android API35 emulator.

Run inside frontend.py's storage/tool reservation. All servers, identities and
observations are synthetic. Android executes the same Rust probe as a native
ELF; this qualifies the transport, not an APK UI or a physical tablet.
"""
import argparse
import contextlib
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
CONFIG = json.loads((ROOT / 'frontend.local.json').read_text())
SDK = Path(CONFIG['android_sdk'])
ADB = SDK / 'platform-tools/adb'
SERIAL = 'emulator-5558'
AVD = 'homeward-dev-api35'
REMOTE = '/data/local/tmp/homeward-nhf002-probe'
REQUIRED = ['unary', 'present_false', 'cancelled', 'session_still_usable',
            'untrusted_ca_rejected', 'deadline', 'unary_deadline', 'normal_eof',
            'typed_revocation', 'network_cut', 'bounded_reconnect',
            'backoff_cancelled', 'zero_absent_future', 'authority_not_retried']


def run(argv, **kwargs):
    return subprocess.run([str(arg) for arg in argv], cwd=ROOT, check=True, timeout=240, **kwargs)


def device(*args, check=True):
    return subprocess.run([str(ADB), '-s', SERIAL, *map(str, args)], cwd=ROOT,
                          capture_output=True, text=True, check=check, timeout=15)


def stop(process, timeout=5):
    if process is None:
        return
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


@contextlib.contextmanager
def android(proof):
    existing = subprocess.check_output([str(ADB), 'devices'], text=True, timeout=15)
    if SERIAL in existing:
        raise RuntimeError('Preserve the existing emulator; the task serial is occupied.')
    with (proof / 'emulator.log').open('wb') as log:
        process = subprocess.Popen([str(SDK / 'emulator/emulator'), '-avd', AVD,
                                    '-port', '5558', '-no-window', '-no-audio',
                                    '-no-boot-anim', '-no-snapshot', '-camera-back',
                                    'none', '-camera-front', 'none', '-gpu', 'swiftshader'],
                                   stdout=log, stderr=subprocess.STDOUT)
        booted = False
        try:
            deadline = time.monotonic() + 120
            while process.poll() is None and time.monotonic() < deadline:
                result = device('shell', 'getprop', 'sys.boot_completed', check=False)
                if result.returncode == 0 and result.stdout.strip() == '1':
                    booted = True
                    break
                time.sleep(.5)
            if not booted or AVD not in device('emu', 'avd', 'name').stdout:
                raise RuntimeError('Owned emulator boot/identity failed.')
            identity = {key: device('shell', 'getprop', key).stdout.strip() for key in
                        ['ro.product.model', 'ro.build.version.sdk', 'ro.product.cpu.abi']}
            if identity['ro.build.version.sdk'] != '35' or identity['ro.product.cpu.abi'] != 'arm64-v8a':
                raise RuntimeError('Unexpected task emulator API/ABI.')
            (proof / 'device.json').write_text(json.dumps({'serial': SERIAL, 'avd': AVD, **identity}, indent=2) + '\n')
            yield
        finally:
            # Never address a USB/physical device; never kill another AVD.
            try:
                if booted and process.poll() is None and AVD in device('emu', 'avd', 'name', check=False).stdout:
                    device('emu', 'kill', check=False)
            finally:
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    stop(process)


def qualify(proof, server_binary, client_binary, platform, wrong_name=False):
    directory = proof / (platform + ('-wrong-name' if wrong_name else '-full'))
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    remote = REMOTE + '-' + proof.name + ('-wrong' if wrong_name else '-full')
    env = os.environ.copy()
    env['HOMEWARD_NHF002_FIXTURE'] = str(fixture)
    env['HOMEWARD_NHF002_WRONG_NAME'] = '1' if wrong_name else '0'
    server = child = reader = None
    ports, report, private = [], None, None
    remote_created = False
    cleanup_errors = []
    server_clean = False
    with (directory / 'server.log').open('wb') as log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF002TransportServer$',
                                       '-test.v', '-test.timeout=5m'], cwd=ROOT, env=env,
                                      stdout=log, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 40
            ready = fixture / 'ready.json'
            while not ready.exists():
                if server.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError('Synthetic core did not become ready; inspect server.log.')
                time.sleep(.05)
            private = json.loads(ready.read_text())
            if private['mode'] != 'synthetic-only':
                raise RuntimeError('Unexpected fixture mode.')
            args = [str(client_binary), str(ready)]
            if platform == 'android':
                for key in ['origin', 'wire_origin']:
                    origin = urlsplit(private[key])
                    if origin.scheme != 'https' or origin.hostname != '127.0.0.1' or not origin.port:
                        raise RuntimeError('Only isolated loopback TLS fixtures are accepted.')
                    device('reverse', '--no-rebind', f'tcp:{origin.port}', f'tcp:{origin.port}')
                    ports.append(origin.port)
                device('shell', 'mkdir', remote)
                remote_created = True
                device('shell', 'chmod', '700', remote)
                device('push', client_binary, remote + '/probe')
                device('push', ready, remote + '/ready.json')
                device('shell', 'chmod', '700', remote + '/probe')
                device('shell', 'chmod', '600', remote + '/ready.json')
                args = [str(ADB), '-s', SERIAL, 'shell', remote + '/probe', remote + '/ready.json']
            if wrong_name:
                args.append('wrong-name')
            child = subprocess.Popen(args, cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            lines = queue.Queue(maxsize=256)

            def pump():
                for line in child.stdout:
                    lines.put(line)
                lines.put(None)

            reader = threading.Thread(target=pump, daemon=True)
            reader.start()
            deadline = time.monotonic() + 100
            with (directory / 'client.log').open('w') as client_log:
                while True:
                    if time.monotonic() >= deadline:
                        raise RuntimeError('Native transport probe deadline exceeded.')
                    try:
                        line = lines.get(timeout=.25)
                    except queue.Empty:
                        continue
                    if line is None:
                        break
                    client_log.write(line.replace(private['access_token'], '<redacted>'))
                    client_log.flush()
                    line = line.strip()
                    if line.startswith('NHF002_ADVANCE '):
                        sequence = int(line.split()[1])
                        if not 1 <= sequence <= 1003:
                            raise RuntimeError('Invalid synthetic publication barrier.')
                        temporary = fixture / 'advance.writing'
                        temporary.write_text(str(sequence))
                        temporary.replace(fixture / 'advance')
                    elif line in {'NHF002_OUTAGE', 'NHF002_RESTORE', 'NHF002_REVOKE'}:
                        (fixture / line.removeprefix('NHF002_').lower()).touch(mode=0o600)
                    elif line.startswith('NHF002_RESULT '):
                        report = json.loads(line.removeprefix('NHF002_RESULT '))
            if child.wait(timeout=5) != 0 or report is None:
                raise RuntimeError('Native transport probe failed; inspect redacted client.log.')
        finally:
            stop(child)
            if reader is not None:
                # Drain a failed client's queue so its pipe reader can join too.
                join_deadline = time.monotonic() + 3
                while reader.is_alive() and time.monotonic() < join_deadline:
                    try:
                        lines.get(timeout=.1)
                    except queue.Empty:
                        reader.join(timeout=.1)
                    if child.poll() is None:
                        stop(child)
                if reader.is_alive():
                    cleanup_errors.append('Native pipe reader did not join.')
            if platform == 'android' and private is not None:
                try:
                    if remote_created:
                        device('shell', 'rm', '-rf', remote)
                    for port in ports:
                        device('reverse', '--remove', f'tcp:{port}')
                except (subprocess.SubprocessError, OSError):
                    cleanup_errors.append('Owned emulator cleanup failed.')
            if server is not None:
                if fixture.exists():
                    (fixture / 'stop').touch(mode=0o600)
                try:
                    server.wait(timeout=20)
                    server_clean = server.returncode == 0
                except subprocess.TimeoutExpired:
                    stop(server, timeout=15)
    if cleanup_errors:
        raise RuntimeError(' '.join(cleanup_errors))
    if not server_clean:
        raise RuntimeError('Synthetic server failed to stop and join cleanly.')
    server_report = json.loads((fixture / 'result.json').read_text())
    if not (server_report['server_joined'] and server_report['future_server_joined']
            and server_report['command_driver_entries'] == 0 and server_report['connections_remaining'] == 0):
        raise RuntimeError('Synthetic server ownership/driver invariant failed.')
    if report['os'] != ('android' if platform == 'android' else 'macos'):
        raise RuntimeError('Native probe ran on the wrong OS.')
    if wrong_name:
        valid = (report.get('wrong_name_rejected') is True and server_report['published'] == 0
                 and server_report['authority_calls'] == 0)
    else:
        valid = (all(report.get(key) is True for key in REQUIRED)
                 and report['stream_frames'] == 1000 and server_report['published'] == 1003
                 and server_report['authority_calls'] == 1
                 # Three exhausted attempts, then at most three reconnect
                 # attempts while the asynchronous restore is being applied.
                 and 3 <= server_report['outage_rejected'] <= 6)
    if not valid:
        raise RuntimeError('Incomplete native transport proof.')
    result = {'status': 'PASS', 'client': report, 'server': server_report,
              'execution': 'native ELF on API35 emulator' if platform == 'android' else 'native macOS executable'}
    (directory / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result), flush=True)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', choices=['desktop', 'android', 'all'], default='all')
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through scripts/frontend.py run --cargo to reserve shared tools/storage.')
    proof = Path(CONFIG['frontend_cache']) / 'transport' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    server = proof / 'server.test'
    run(['go', 'test', '-c', '-race', '-o', server, './internal/server'])
    platforms = ['desktop', 'android'] if args.platform == 'all' else [args.platform]
    results, hashes = [], {'server.test': hashlib.sha256(server.read_bytes()).hexdigest()}
    for platform in platforms:
        target_args = ['--target', 'aarch64-linux-android'] if platform == 'android' else []
        run(['cargo', 'build', '--locked', '-p', 'home-client-transport', '--example', 'nhf002_probe', *target_args])
        binary = Path(os.environ['CARGO_TARGET_DIR'])
        if platform == 'android':
            binary /= 'aarch64-linux-android'
        binary /= 'debug/examples/nhf002_probe'
        hashes[platform] = hashlib.sha256(binary.read_bytes()).hexdigest()
        owner = android(proof) if platform == 'android' else contextlib.nullcontext()
        with owner:
            results.append(qualify(proof, server, binary, platform))
            results.append(qualify(proof, server, binary, platform, wrong_name=True))
    (proof / 'result.json').write_text(json.dumps({'status': 'PASS', 'hashes': hashes, 'cases': results}, indent=2) + '\n')
    print('Transport qualification PASS: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
