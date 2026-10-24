#!/usr/bin/env python3
"""Opt-in native auth/session proof; invoke through frontend.py run --cargo.

Fresh native Go SQL/auth/TLS fixtures only. macOS uses the actual Keychain;
Android uses the product APK and a task-owned API35 VM, never a native ELF or a
physical device. GUI input requires Homebrew cliclick and macOS Accessibility.
Screenshots supplement RPC/storage assertions; inspect them before delivery.
"""
import argparse
import base64
import contextlib
from dataclasses import dataclass
from enum import Enum
import hashlib
import json
import os
from pathlib import Path
import plistlib
import runpy
import secrets
import shlex
import shutil
import subprocess
import sys
import time
from urllib.parse import urlsplit
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
CONFIG = json.loads((ROOT / 'frontend.local.json').read_text())
CACHE = Path(CONFIG['frontend_cache']) / 'session'
TARGET = Path(CONFIG['backend_cache']) / 'workspace/target'
TRANSPORT = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-transport.py'))
Platform = TRANSPORT['Platform']
PlatformSelection = TRANSPORT['PlatformSelection']
device = TRANSPORT['device']
PACKAGE = 'ca.homeward.frontend.dev'


class FixtureMode(Enum):
    PASSWORD = 'password'
    RECOVERY = 'recovery'
    ENROLLMENT = 'enrollment'
    RATE_LIMIT = 'rate-limit'


class AppStep(Enum):
    LOGIN = 'login'
    RESTORE_READ_LOGOUT = 'restore-read-logout'
    STORE_FAILURE = 'store-unavailable'
    SIGNED_OUT = 'signed-out-restart'


@dataclass(frozen=True)
class Case:
    label: str
    mode: FixtureMode
    inputs: tuple[str, ...]
    faults: tuple[str, ...]
    counters: dict[str, int]


# External examples of the Rust SessionScenario CLI contract, not string-based
# business dispatch. The probe parses its closed enum before reading a fixture.
CASES = [
    Case('login', FixtureMode.PASSWORD, ('login',), (), {'start_login_calls': 1, 'revoke_ok': 1}),
    Case('wrong-password', FixtureMode.PASSWORD, ('wrong-password',), (), {'start_login_calls': 1, 'revoke_calls': 0}),
    Case('rate-limit', FixtureMode.RATE_LIMIT, ('rate-limit',), (), {'start_login_calls': 1, 'revoke_calls': 0}),
    Case('enrollment', FixtureMode.ENROLLMENT, ('enrollment',), (), {'start_login_calls': 1, 'revoke_calls': 0}),
    Case('recovery', FixtureMode.RECOVERY, ('recovery',), (), {'start_login_calls': 1, 'complete_login_calls': 1, 'revoke_ok': 1}),
    Case('restart', FixtureMode.PASSWORD, ('persist', 'restore'), (), {'start_login_calls': 1, 'revoke_ok': 1}),
    Case('ten-waiters', FixtureMode.PASSWORD, ('concurrent',), ('short-access',), {'start_login_calls': 1, 'refresh_calls': 1, 'revoke_ok': 1}),
    Case('lost-response-restart', FixtureMode.PASSWORD, ('lose', 'restore-uncertain'), ('short-access', 'lose-refresh'), {'start_login_calls': 1, 'refresh_calls': 1, 'revoke_calls': 0, 'lost_refresh_responses': 1}),
]


def run(argv, **options):
    return subprocess.run(list(map(str, argv)), cwd=ROOT, check=True, timeout=300, **options)


def write(path, value):
    with os.fdopen(os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600), 'w') as output:
        json.dump(value, output, indent=2)
        output.write('\n')


def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


@contextlib.contextmanager
def fixture(directory, server_binary, mode=FixtureMode.PASSWORD):
    directory.mkdir(mode=0o700)
    control = directory / 'fixture'
    env = os.environ.copy()
    env.update(HOMEWARD_NHF003_FIXTURE=str(control), HOMEWARD_NHF003_MODE=mode.value)
    with (directory / 'server.log').open('wb') as log:
        server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF003SessionServer$', '-test.v', '-test.timeout=8m'], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
        try:
            ready = control / 'ready.json'
            deadline = time.monotonic() + 60
            while not ready.exists():
                if server.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError('Synthetic native fixture did not become ready: ' + str(directory))
                time.sleep(.05)
            private = json.loads(ready.read_text())
            origin = urlsplit(private['origin'])
            if private['mode'] != 'synthetic-only' or origin.scheme != 'https' or origin.hostname != '127.0.0.1' or not origin.port:
                raise RuntimeError('Refuse a non-synthetic or non-loopback fixture')
            yield private, control
        finally:
            if control.is_dir():
                (control / 'stop').touch(mode=0o600)
            try:
                server.wait(timeout=20)
            except subprocess.TimeoutExpired:
                TRANSPORT['stop'](server)
        if server.returncode != 0:
            raise RuntimeError('Synthetic native server failed to join cleanly')
        report = json.loads((control / 'result.json').read_text())
        if not report['server_joined'] or report['command_driver_entries'] != 0:
            raise RuntimeError('Synthetic fixture violated its shutdown/read-only invariant')


def protocol(proof, server_binary):
    run(['cargo', 'build', '--locked', '-p', 'home-client-platform', '--examples'])
    binary = TARGET / 'debug/examples/native_session_probe'
    refused = subprocess.run([str(binary), '/nonexistent/fixture', 'unknown-scenario'], capture_output=True, text=True, timeout=10)
    assert refused.returncode != 0 and 'unknown variant' in refused.stderr
    rows = []
    for case in CASES:
        directory = proof / case.label
        reports = []
        with fixture(directory, server_binary, case.mode) as (private, control):
            private['namespace_nonce'] = secrets.token_hex(32)
            path = directory / 'client.json'
            write(path, private)
            try:
                for fault in case.faults:
                    (control / fault).touch(mode=0o600)
                for selector in case.inputs:
                    result = subprocess.run([str(binary), str(path), selector], cwd=ROOT, capture_output=True, text=True, timeout=40)
                    diagnostic = result.stdout + result.stderr
                    for field in ('password', 'recovery_code'):
                        if private.get(field):
                            diagnostic = diagnostic.replace(private[field], '<redacted>')
                    (directory / (selector + '.log')).write_text(diagnostic)
                    assert result.returncode == 0, 'native session scenario failed: ' + case.label
                    marker = next(line for line in result.stdout.splitlines() if line.startswith('NHF003_RESULT '))
                    reports.append(json.loads(marker.removeprefix('NHF003_RESULT ')))
            finally:
                cleanup = subprocess.run([str(binary), str(path), 'cleanup'], cwd=ROOT, capture_output=True, text=True, timeout=20)
                assert cleanup.returncode == 0, 'synthetic Keychain cleanup was not confirmed'
        report = json.loads((control / 'result.json').read_text())
        for key, expected in case.counters.items():
            assert report[key] == expected, case.label + ': ' + key
        rows.append({'case': case.label, 'client': reports, 'server': report})
        print('Native session scenario PASS: ' + case.label, flush=True)
    write(proof / 'protocol.json', {'status': 'PASS', 'client_sha256': digest(binary), 'cases': rows})


def namespace(private):
    der = base64.b64decode(''.join(line for line in private['ca_pem'].splitlines() if not line.startswith('-----')), validate=True)
    trust = hashlib.sha256(len(der).to_bytes(8, 'little') + der).digest()
    origin = private['origin'].encode()
    return hashlib.sha256(b'Homeward-secure-session-v1\0' + len(origin).to_bytes(8, 'little') + origin + trust).hexdigest()


def public_profile(private):
    return {'version': '1', 'origin': private['origin'], 'certificates': private['ca_pem']}


def validate_app_report(control, login_count):
    report = json.loads((control / 'result.json').read_text())
    for key, expected in {'start_login_calls': login_count, 'refresh_calls': 0, 'state_read_calls': 1, 'state_read_ok': 1, 'revoke_calls': 1, 'revoke_ok': 1}.items():
        assert report[key] == expected, 'native application counter: ' + key
    assert report['login_observation']['name_matches'] and report['login_observation']['password_matches']
    return report


def desktop(proof, server_binary):
    cliclick = shutil.which('cliclick')
    if cliclick is None:
        raise RuntimeError('Homebrew cliclick is required for native GUI qualification')
    run(['cargo', 'build', '--locked', '-p', 'homeward-app', '--bin', 'homeward-desktop'])
    helper = proof / 'native-ui'
    # Reuse the frontend's existing native SDK cache from the first UI proof.
    modules = Path(CONFIG['frontend_cache']) / 'session-spike/swift-modules'
    modules.mkdir(parents=True, exist_ok=True)
    run(['xcrun', 'swiftc', '-module-cache-path', modules, ROOT / 'qualification/frontend/native-ui.swift', '-o', helper])
    public = Path.home() / 'Library/Application Support/Homeward/endpoint.json'
    if public.exists():
        raise RuntimeError('Preserve an existing product public profile')
    directory = proof / 'desktop'
    with fixture(directory, server_binary) as (private, control):
        key = namespace(private)
        def present():
            found = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', key], capture_output=True, timeout=10)
            if found.returncode not in (0, 44):
                raise RuntimeError('Keychain metadata unavailable')
            return found.returncode == 0
        if present():
            raise RuntimeError('Preserve an existing synthetic credential namespace')
        public.parent.mkdir(parents=True, exist_ok=True)
        written = False
        try:
            write(public, public_profile(private))
            written = True
            bundle = directory / 'Homeward Session.app/Contents'
            (bundle / 'MacOS').mkdir(parents=True)
            binary = bundle / 'MacOS/homeward-desktop'
            shutil.copyfile(TARGET / 'debug/homeward-desktop', binary)
            binary.chmod(0o755)
            with (bundle / 'Info.plist').open('wb') as output:
                plistlib.dump({'CFBundleExecutable': binary.name, 'CFBundleIdentifier': PACKAGE, 'CFBundleName': 'Homeward Session', 'CFBundlePackageType': 'APPL', 'NSHighResolutionCapable': True}, output)
            for step in [AppStep.LOGIN, AppStep.RESTORE_READ_LOGOUT, AppStep.SIGNED_OUT]:
                with (directory / (step.value + '.log')).open('wb') as log:
                    child = subprocess.Popen([str(binary)], stdout=log, stderr=subprocess.STDOUT)
                    action = directory / 'action.json'
                    def native(command, **fields):
                        action.write_text(json.dumps({'pid': child.pid, 'command': command, **fields}))
                        return subprocess.run([str(helper), str(action)], capture_output=True, text=True, timeout=5)
                    def keyboard(commands):
                        run([cliclick, '-w', '150', '-f', '-'], input=commands, text=True, capture_output=True)
                    def snapshot(name):
                        run(['screencapture', '-x', '-o', '-l', str(info['window_id']), directory / (name + '.png')], capture_output=True)
                    try:
                        deadline = time.monotonic() + 10
                        while True:
                            result = native('info')
                            if result.returncode == 0:
                                info = json.loads(result.stdout)
                                break
                            if child.poll() is not None or time.monotonic() >= deadline:
                                raise RuntimeError('Owned native application window did not start')
                            time.sleep(.1)
                        assert info['accessibility_trusted'] and info['post_events_allowed']
                        time.sleep(.5)
                        snapshot(step.value)
                        match step:
                            case AppStep.LOGIN:
                                assert native('click', x=90, y=130).returncode == 0
                                keyboard('kp:tab\nt:' + private['login_name'] + '\nkp:tab\nt:' + private['password'] + '\nkp:return\n')
                                deadline = time.monotonic() + 10
                                while not present() and time.monotonic() < deadline:
                                    time.sleep(.1)
                                assert present(), 'native application did not save its admitted session'
                                snapshot('authenticated')
                            case AppStep.RESTORE_READ_LOGOUT:
                                assert present()
                                assert native('click', x=90, y=130).returncode == 0
                                keyboard('kp:tab\nkp:tab\nkp:return\n')
                                time.sleep(1)
                                snapshot('protected-read')
                                keyboard('kd:shift\nkp:tab\nku:shift\nkp:return\n')
                                deadline = time.monotonic() + 10
                                while present() and time.monotonic() < deadline:
                                    time.sleep(.1)
                                assert not present(), 'native client logout retained its Keychain item'
                                snapshot('client-logout')
                            case AppStep.SIGNED_OUT:
                                assert not present()
                            case AppStep.STORE_FAILURE:
                                raise AssertionError('Android-only storage fault')
                            case _:
                                raise AssertionError('Unhandled desktop application step')
                        assert native('close').returncode == 0
                        child.wait(timeout=10)
                        assert child.returncode == 0
                    finally:
                        TRANSPORT['stop'](child)
            write(directory / 'keychain.json', {'restored_across_normal_exit': True, 'absent_after_client_logout_before_harness_cleanup': not present(), 'normal_exits': 3})
        finally:
            cleanup_errors = []
            if written:
                try:
                    if public.exists():
                        if json.loads(public.read_text())['origin'] == private['origin']:
                            public.unlink()
                        else:
                            cleanup_errors.append('public profile changed outside qualification; preserved')
                except (OSError, ValueError):
                    cleanup_errors.append('public profile unreadable during cleanup; preserved')
            deleted = subprocess.run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', key], capture_output=True, timeout=10)
            if deleted.returncode not in (0, 44):
                cleanup_errors.append('synthetic Keychain cleanup unconfirmed')
            if cleanup_errors:
                raise RuntimeError('; '.join(cleanup_errors))
    write(directory / 'result.json', {'status': 'AUTOMATED_CHECKS_PASS', 'server': validate_app_report(control, 1), 'binary_sha256': digest(binary), 'screenshots_require_visual_inspection': True})


def android(proof, server_binary):
    run(['python3', 'scripts/build-frontend-android.py'])
    apk = proof / 'Homeward.apk'
    shutil.copyfile(TARGET / 'debug/apk/homeward-dev.apk', apk)
    directory = proof / 'android'
    with fixture(directory, server_binary) as (private, control):
        with TRANSPORT['android'](directory):
            port = urlsplit(private['origin']).port
            forwarded = owned = False
            try:
                device('install', '-r', apk)
                existing = device('shell', 'run-as', PACKAGE, 'sh', '-c', shlex.quote('test -e files/endpoint.json'), check=False)
                if existing.returncode != 1:
                    raise RuntimeError('Preserve existing or unreadable application profile')
                device('reverse', '--no-rebind', f'tcp:{port}', f'tcp:{port}')
                forwarded = True
                device('shell', 'run-as', PACKAGE, 'mkdir', '-p', 'files')
                run([TRANSPORT['ADB'], '-s', TRANSPORT['SERIAL'], 'shell', 'run-as', PACKAGE, 'sh', '-c', shlex.quote('cat > files/endpoint.json')], input=json.dumps(public_profile(private)), text=True, capture_output=True)
                owned = True
                prefs = 'shared_prefs/ca.homeward.session.v1.' + namespace(private) + '.xml'
                def records():
                    result = device('shell', 'run-as', PACKAGE, 'cat', prefs)
                    return ET.fromstring(result.stdout).findall('string[@name="record"]')
                def snapshot(name):
                    result = run([TRANSPORT['ADB'], '-s', TRANSPORT['SERIAL'], 'exec-out', 'screencap', '-p'], capture_output=True)
                    (directory / (name + '.png')).write_bytes(result.stdout)
                def login():
                    device('shell', 'input', 'tap', '160', '344')
                    for command in [('text', shlex.quote(private['login_name'].replace(' ', '%s'))), ('keyevent', '61'), ('text', shlex.quote(private['password'].replace(' ', '%s'))), ('keyevent', '66')]:
                        # Never include submitted values in CalledProcessError/logs.
                        result = device('shell', 'input', *command, check=False)
                        assert result.returncode == 0, 'native synthetic input failed'
                    time.sleep(1)
                before = time.time_ns()
                device_clock = device('shell', 'date', '+%s.%N').stdout.strip()
                write(directory / 'clock.json', {'host_before_ns': before, 'device_epoch_seconds': device_clock, 'host_after_ns': time.time_ns()})
                for step in [AppStep.LOGIN, AppStep.RESTORE_READ_LOGOUT, AppStep.STORE_FAILURE, AppStep.SIGNED_OUT]:
                    device('shell', 'am', 'start', '-n', PACKAGE + '/android.app.NativeActivity')
                    time.sleep(2)
                    snapshot(step.value)
                    match step:
                        case AppStep.LOGIN:
                            login()
                            deadline = time.monotonic() + 10
                            while not records() and time.monotonic() < deadline:
                                time.sleep(.1)
                            stored = records()
                            assert len(stored) == 1
                            envelope = base64.b64decode(stored[0].text, validate=True)
                            assert envelope.startswith(b'HWStore1') and len(envelope) > 36
                            write(directory / 'encrypted-store.json', {'credential_records': 1, 'envelope': 'HWStore1', 'ciphertext_bytes': len(envelope)})
                            snapshot('authenticated')
                        case AppStep.RESTORE_READ_LOGOUT:
                            assert len(records()) == 1
                            device('shell', 'input', 'tap', '80', '100')
                            device('shell', 'input', 'keyevent', '61', '61', '66')
                            time.sleep(1)
                            snapshot('protected-read')
                            device('shell', 'input', 'keycombination', '59', '61')
                            device('shell', 'input', 'keyevent', '66')
                            time.sleep(1)
                            assert not records(), 'native client logout retained encrypted credentials'
                            write(directory / 'client-logout.json', {'credential_records': 0, 'before_harness_cleanup': True})
                            snapshot('client-logout')
                        case AppStep.STORE_FAILURE:
                            assert not records(), 'logged-out restart restored credentials'
                            device('shell', 'run-as', PACKAGE, 'chmod', '500', 'shared_prefs')
                            try:
                                login()
                                assert not records(), 'failed native commit created a credential file'
                                snapshot('native-store-unavailable')
                            finally:
                                device('shell', 'run-as', PACKAGE, 'chmod', '700', 'shared_prefs')
                        case AppStep.SIGNED_OUT:
                            assert not records()
                        case _:
                            raise AssertionError('Unhandled Android application step')
                    device('shell', 'am', 'force-stop', PACKAGE)
            finally:
                errors = []
                device('shell', 'am', 'force-stop', PACKAGE, check=False)
                if owned:
                    cleared = device('shell', 'pm', 'clear', PACKAGE, check=False)
                    if cleared.returncode != 0 or cleared.stdout.strip() != 'Success':
                        errors.append('owned synthetic application cleanup unconfirmed')
                if forwarded:
                    removed = device('reverse', '--remove', f'tcp:{port}', check=False)
                    if removed.returncode != 0:
                        errors.append('owned port forwarding cleanup unconfirmed')
                if errors:
                    raise RuntimeError('; '.join(errors))
    write(directory / 'result.json', {'status': 'AUTOMATED_CHECKS_PASS', 'server': validate_app_report(control, 2), 'apk_sha256': digest(apk), 'actual_native_commit_failure_without_fallback': True, 'screenshots_require_visual_inspection': True})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', type=PlatformSelection, choices=list(PlatformSelection), default=PlatformSelection.ALL)
    args = parser.parse_args()
    if Path(os.environ.get('CARGO_TARGET_DIR', '')).resolve() != TARGET.resolve():
        raise SystemExit('Use the reserved shared target through scripts/frontend.py run --cargo')
    CACHE.mkdir(mode=0o700, parents=True, exist_ok=True)
    proof = CACHE / str(time.time_ns())
    proof.mkdir(mode=0o700)
    shutil.copyfile(__file__, proof / 'runner.py')
    server_binary = proof / 'server.test'
    run(['go', 'test', '-c', '-race', '-o', server_binary, './internal/server'])
    for platform in args.platform.platforms():
        match platform:
            case Platform.DESKTOP:
                protocol(proof, server_binary)
                desktop(proof, server_binary)
            case Platform.ANDROID:
                android(proof, server_binary)
            case _:
                raise AssertionError('Unhandled native qualification platform')
    write(proof / 'result.json', {'status': 'AUTOMATED_CHECKS_PASS', 'platform': args.platform.value, 'server_sha256': digest(server_binary), 'client_fido2_qualified': False, 'physical_devices_qualified': False, 'screenshots_require_visual_inspection': True})
    print('Native session qualification: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
