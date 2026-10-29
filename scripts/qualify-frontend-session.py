#!/usr/bin/env python3
"""Opt-in native auth/session proof; invoke through frontend.py run --cargo.

Fresh native Go SQL/auth/TLS fixtures only. macOS uses the actual Keychain;
Android uses the product APK and a task-owned API35 VM, never a native ELF or a
physical device. macOS input requires Accessibility for the owned process.
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
import re
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
ANDROID_INPUT = runpy.run_path(str(ROOT / 'scripts/frontend-android-input.py'))
Platform = TRANSPORT['Platform']
PlatformSelection = TRANSPORT['PlatformSelection']
device = TRANSPORT['device']
PACKAGE = 'ca.homeward.frontend.dev'
ACTIVITY = ANDROID_INPUT['ACTIVITY_NAME']


class FixtureMode(Enum):
    PASSWORD = 'password'
    RECOVERY = 'recovery'
    ENROLLMENT = 'enrollment'
    RATE_LIMIT = 'rate-limit'


class FixtureKind(Enum):
    SESSION = 'session'
    CACHE = 'cache'
    WORKLOAD = 'workload'


class UIProfile(Enum):
    CACHE = 'cache'
    HOME = 'home'

    def entity_count(self):
        match self:
            case UIProfile.CACHE:
                return 850
            case UIProfile.HOME:
                return 851
        raise AssertionError('Unhandled UI profile')


class CacheAction(Enum):
    LOAD = 'load'


class CacheImageExpectation(Enum):
    READY = 'ready'
    CLEARED = 'cleared'


class NativeAction(Enum):
    TREE = 'tree'
    FILL_LOGIN = 'fill-login'
    READ = 'read'
    SIGN_OUT = 'sign-out'
    SCROLL_END = 'scroll-end'
    CHECK_ROW = 'check-row'
    NAVIGATE = 'navigate'


class NativeNavigationSection(Enum):
    INVENTORY = 'Inventaire'


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
def fixture(directory, server_binary, mode=FixtureMode.PASSWORD, kind=FixtureKind.SESSION, profile=UIProfile.CACHE):
    directory.mkdir(mode=0o700)
    control = directory / 'fixture'
    env = os.environ.copy()
    match kind:
        case FixtureKind.SESSION:
            assert profile is UIProfile.CACHE
            env.update(HOMEWARD_NHF003_FIXTURE=str(control), HOMEWARD_NHF003_MODE=mode.value)
            test = 'TestNHF003SessionServer'
        case FixtureKind.CACHE | FixtureKind.WORKLOAD:
            assert mode is FixtureMode.PASSWORD
            env.update(HOMEWARD_NHF004_FIXTURE=str(control), HOMEWARD_NHF004_APP='true', HOMEWARD_FRONTEND_UI_PROFILE=profile.value)
            test = 'TestNHF004CacheServer'
            if profile is UIProfile.HOME:
                dashboard = directory / 'server-dashboard'
                dashboard.mkdir(mode=0o700)
                shutil.copyfile(ROOT / 'qualification/frontend/dashboards/home.yaml', dashboard / 'dashboard.yaml')
                env['HOMEWARD_NHF005_DASHBOARD'] = str(dashboard / 'dashboard.yaml')
            if kind is FixtureKind.WORKLOAD:
                assert profile is UIProfile.HOME
                source = directory/'workload-source.json'
                shutil.copyfile(ROOT/'qualification/frontend/workload/source.json',source)
                env['HOMEWARD_NHF023_WORKLOAD_SOURCE'] = str(source)
    with (directory / 'server.log').open('wb') as log:
        server = subprocess.Popen([str(server_binary), '-test.run=^' + test + '$', '-test.v', '-test.timeout=8m'], cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT)
        try:
            ready = control / 'ready.json'
            deadline = time.monotonic() + (240 if kind is FixtureKind.WORKLOAD else 60)
            while not ready.exists():
                if server.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError('Synthetic native fixture did not become ready: ' + str(directory))
                time.sleep(.05)
            private = json.loads(ready.read_text())
            origin = urlsplit(private['origin'])
            if private['mode'] != 'synthetic-only' or origin.scheme != 'https' or origin.hostname != '127.0.0.1' or not origin.port:
                raise RuntimeError('Refuse a non-synthetic or non-loopback fixture')
            if kind in (FixtureKind.CACHE,FixtureKind.WORKLOAD) and (private['entity_count'] != profile.entity_count() or private['ui_profile'] != profile.value):
                raise RuntimeError('Native UI fixture profile/count mismatch')
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


def validate_app_report(control, login_count, read_count=1):
    report = json.loads((control / 'result.json').read_text())
    for key, expected in {'start_login_calls': login_count, 'refresh_calls': 0, 'state_read_calls': read_count, 'state_read_ok': read_count, 'revoke_calls': 1, 'revoke_ok': 1}.items():
        assert report[key] == expected, 'native application counter: ' + key
    assert report['login_observation']['name_matches'] and report['login_observation']['password_matches']
    return report


def cache_load(control, alive):
    temporary = control / 'command.writing'
    write(temporary, {'sequence': 1, 'action': CacheAction.LOAD.value})
    temporary.replace(control / 'command.json')
    print('Native UI: 850 entities, starting 1,000 updates at 20/s', flush=True)
    deadline = time.monotonic() + 70
    while not (control / 'load-done.json').exists():
        if not alive() or time.monotonic() >= deadline:
            raise RuntimeError('Native loaded UI stopped or fixture load deadline exceeded')
        time.sleep(.5)
    report = json.loads((control / 'load-done.json').read_text())
    assert report['updates'] == 1000 and 49 <= report['elapsed_seconds'] <= 70
    return report


def cache_image_helper(proof):
    helper = proof / 'cache-image'
    if not helper.exists():
        modules = Path(CONFIG['frontend_cache']) / 'session-spike/swift-modules'
        modules.mkdir(parents=True, exist_ok=True)
        run(['xcrun', 'swiftc', '-module-cache-path', modules, ROOT / 'qualification/frontend/cache-image.swift', '-o', helper])
    return helper


def inspect_cache_image(helper, path, expectation):
    if expectation is not None:
        result = run([helper, path, expectation.value], capture_output=True, text=True)
        write(path.with_suffix('.inspection.json'), json.loads(result.stdout))


def desktop(proof, server_binary, lifecycle=False, kind=FixtureKind.SESSION):
    load_report = None
    image_helper = cache_image_helper(proof) if kind is FixtureKind.CACHE else None
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
    with fixture(directory, server_binary, kind=kind) as (private, control):
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
                    def accessible(command, **fields):
                        return subprocess.run([str(helper), '-'], input=json.dumps({'pid': child.pid, 'command': command.value, 'syntheticOnly': True, **fields}), capture_output=True, text=True, timeout=20)
                    def wait_native(label, editors=None, contains=False):
                        deadline = time.monotonic() + 10
                        while True:
                            response = accessible(NativeAction.TREE)
                            rows = json.loads(response.stdout) if response.returncode == 0 else []
                            found = any(any(label in str(row[key]) if contains else label == row[key]
                                            for key in ['title','description','text']) for row in rows)
                            fields_ready = editors is None or sum(row['role'] == 'AXTextField' for row in rows) == editors
                            if found and fields_ready:
                                return
                            if child.poll() is not None or time.monotonic() >= deadline:
                                raise AssertionError('Owned native surface did not become ready: ' + label)
                            time.sleep(.1)
                    def snapshot(name, expectation=None):
                        run(['screencapture', '-x', '-o', '-l', str(info['window_id']), directory / (name + '.png')], capture_output=True)
                        inspect_cache_image(image_helper, directory / (name + '.png'), expectation)
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
                        if step is AppStep.RESTORE_READ_LOGOUT:
                            wait_native('États à jour', contains=True)
                        else:
                            wait_native('Se connecter', editors=2)
                        snapshot(step.value)
                        match step:
                            case AppStep.LOGIN:
                                assert accessible(NativeAction.FILL_LOGIN, loginName=private['login_name'], password=private['password']).returncode == 0
                                deadline = time.monotonic() + 10
                                while not present() and time.monotonic() < deadline:
                                    time.sleep(.1)
                                assert present(), 'native application did not save its admitted session'
                                snapshot('authenticated')
                            case AppStep.RESTORE_READ_LOGOUT:
                                assert present()
                                # Startup restores the OS session and obtains a
                                # fresh authorized view without a second click.
                                assert accessible(NativeAction.NAVIGATE, section=NativeNavigationSection.INVENTORY.value).returncode == 0
                                snapshot('protected-read')
                                if kind is FixtureKind.CACHE:
                                    scroll = accessible(NativeAction.SCROLL_END, expectedRowLabel=private['expected_last_alias'])
                                    assert scroll.returncode == 0, 'last entity was not projected after native scrolling'
                                    write(directory / 'last-row-projected.json', json.loads(scroll.stdout))
                                    time.sleep(.7)
                                    snapshot('last-viewport-before-load', CacheImageExpectation.READY)
                                    load_report = cache_load(control, lambda: child.poll() is None)
                                    snapshot('loaded-850', CacheImageExpectation.READY)
                                    assert accessible(NativeAction.SCROLL_END, expectedRowLabel=private['expected_last_alias']).returncode == 0
                                    time.sleep(.7)
                                    snapshot('last-viewport', CacheImageExpectation.READY)
                                if lifecycle:
                                    assert native('minimize').returncode == 0
                                    time.sleep(.4)
                                    assert native('restore').returncode == 0
                                    info = json.loads(native('info').stdout)
                                    time.sleep(1)
                                    snapshot('resumed-read', CacheImageExpectation.READY if kind is FixtureKind.CACHE else None)
                                    if kind is FixtureKind.CACHE:
                                        check = accessible(NativeAction.CHECK_ROW, expectedRowLabel=private['expected_first_alias'])
                                        assert check.returncode == 0, 'resumed table did not reproject its first row'
                                        write(directory / 'resumed-first-row.json', json.loads(check.stdout))
                                assert accessible(NativeAction.SIGN_OUT).returncode == 0
                                deadline = time.monotonic() + 10
                                while present() and time.monotonic() < deadline:
                                    time.sleep(.1)
                                assert not present(), 'native client logout retained its Keychain item'
                                snapshot('client-logout', CacheImageExpectation.CLEARED if kind is FixtureKind.CACHE else None)
                            case AppStep.SIGNED_OUT:
                                assert not present()
                            case AppStep.STORE_FAILURE:
                                raise AssertionError('Android-only storage fault')
                            case _:
                                raise AssertionError('Unhandled desktop application step')
                        assert native('close').returncode == 0
                        child.wait(timeout=10)
                        assert child.returncode == 0
                    except BaseException:
                        if child.poll() is None:
                            snapshot('failure')
                        raise
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
    write(directory / 'result.json', {'status': 'AUTOMATED_CHECKS_PASS', 'server': validate_app_report(control, 1, 2 if lifecycle else 1), 'binary_sha256': digest(binary), 'native_minimize_restore_cycle': lifecycle, 'fixture_kind': kind.value, 'input_interaction': 'native-accessibility' if kind is FixtureKind.CACHE else 'keyboard', 'cache_load': load_report, 'screenshots_require_visual_inspection': True})


def android(proof, server_binary, lifecycle=False, kind=FixtureKind.SESSION):
    load_report = None
    image_helper = cache_image_helper(proof) if kind is FixtureKind.CACHE else None
    apk, ocr = ANDROID_INPUT['prepare_apk'](ROOT, proof, run)
    directory = proof / 'android'
    with fixture(directory, server_binary, kind=kind) as (private, control):
        with TRANSPORT['android'](directory):
            port = urlsplit(private['origin']).port
            forwarded = owned = installed = False
            rotation = {key: device('shell', 'settings', 'get', 'system', key).stdout.strip()
                        for key in ['accelerometer_rotation', 'user_rotation']}
            try:
                existing = device('shell', 'pm', 'path', PACKAGE, check=False)
                if existing.stdout.strip() or existing.returncode not in (0, 1) or existing.stderr.strip():
                    raise RuntimeError('Preserve existing or unreadable emulator application')
                device('install', apk)
                installed = True
                device('reverse', '--no-rebind', f'tcp:{port}', f'tcp:{port}')
                forwarded = True
                device('shell', 'run-as', PACKAGE, 'mkdir', '-p', 'files')
                run([TRANSPORT['ADB'], '-s', TRANSPORT['SERIAL'], 'shell', 'run-as', PACKAGE, 'sh', '-c', shlex.quote('cat > files/endpoint.json')], input=json.dumps(public_profile(private)), text=True, capture_output=True)
                owned = True
                device('shell', 'settings', 'put', 'system', 'accelerometer_rotation', '0')
                device('shell', 'settings', 'put', 'system', 'user_rotation', '0')
                device('shell', 'wm', 'density', '160')
                ui = ANDROID_INPUT['AndroidInput'](directory, ocr, TRANSPORT['ADB'], TRANSPORT['SERIAL'], device)
                contains = ANDROID_INPUT['LabelMatch'].CONTAINS
                ui.format(1280, 800, 'owned-display')
                prefs = 'shared_prefs/ca.homeward.session.v1.' + namespace(private) + '.xml'
                def records():
                    result = device('shell', 'run-as', PACKAGE, 'cat', prefs, check=False)
                    if result.returncode:
                        if 'No such file' in result.stderr:
                            return []
                        raise RuntimeError('Owned credential metadata unavailable')
                    return ET.fromstring(result.stdout).findall('string[@name="record"]')
                def snapshot(name, expectation=None):
                    result = run([TRANSPORT['ADB'], '-s', TRANSPORT['SERIAL'], 'exec-out', 'screencap', '-p'], capture_output=True)
                    (directory / (name + '.png')).write_bytes(result.stdout)
                    inspect_cache_image(image_helper, directory / (name + '.png'), expectation)
                def login():
                    ui.tap('Identifiant', offset_y=40)
                    for command in [('text', shlex.quote(private['login_name'].replace(' ', '%s'))), ('keyevent', '61'), ('text', shlex.quote(private['password'].replace(' ', '%s'))), ('keyevent', '66')]:
                        # Never include submitted values in CalledProcessError/logs.
                        result = device('shell', 'input', *command, check=False)
                        assert result.returncode == 0, 'native synthetic input failed'
                def first_visible_alias():
                    labels, _, _, _ = ui.labels()
                    aliases = [row for row in labels if row['confidence'] >= .5
                               and re.fullmatch(r'[a-z_]+\.[a-z0-9_]+', row['label'])]
                    assert aliases, 'Expected an observed complete inventory alias'
                    return min(aliases, key=lambda row: row['y'])['label']
                before = time.time_ns()
                device_clock = device('shell', 'date', '+%s.%N').stdout.strip()
                write(directory / 'clock.json', {'host_before_ns': before, 'device_epoch_seconds': device_clock, 'host_after_ns': time.time_ns()})
                for step in [AppStep.LOGIN, AppStep.RESTORE_READ_LOGOUT, AppStep.STORE_FAILURE, AppStep.SIGNED_OUT]:
                    device('shell', 'am', 'start', '-n', PACKAGE + '/' + ACTIVITY)
                    ui.find('États à jour', match=contains) if step is AppStep.RESTORE_READ_LOGOUT else ui.find('Identifiant')
                    snapshot(step.value)
                    match step:
                        case AppStep.LOGIN:
                            login()
                            ui.find('Afficher ma maison', match=contains)
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
                            if kind is FixtureKind.CACHE:
                                ui.find('850 entités', match=contains)
                            ui.tap('Inventaire')
                            ui.find('Élément')
                            snapshot('protected-read')
                            if kind is FixtureKind.CACHE:
                                pid = device('shell', 'pidof', PACKAGE).stdout.strip()
                                assert pid.isdecimal(), 'expected one owned native app process'
                                load_report = cache_load(control, lambda: device('shell', 'pidof', PACKAGE).stdout.strip() == pid)
                                snapshot('loaded-850', CacheImageExpectation.READY)
                                ui.tap(first_visible_alias(), match=contains)
                                ui.find('Retour aux éléments')
                                ui.tap('Retour aux éléments')
                                device('shell', 'input', 'keyevent', '123')
                                time.sleep(.7)
                                device('shell', 'input', 'keyevent', '66')
                                ui.find('Retour aux éléments')
                                ui.find(private['expected_last_alias'], match=contains)
                                ui.tap('Retour aux éléments')
                                time.sleep(.7)
                                snapshot('last-viewport', CacheImageExpectation.READY)
                            if lifecycle:
                                before_pid = device('shell', 'pidof', PACKAGE).stdout.strip()
                                device('shell', 'input', 'keyevent', '3')
                                time.sleep(.7)
                                device('shell', 'am', 'start', '--activity-reorder-to-front', '-n', PACKAGE + '/' + ACTIVITY)
                                time.sleep(2)
                                assert device('shell', 'pidof', PACKAGE).stdout.strip() == before_pid, 'foreground restarted instead of resuming the owned app'
                                snapshot('resumed-read', CacheImageExpectation.READY if kind is FixtureKind.CACHE else None)
                            ui.tap('Préférences')
                            ui.tap('Se déconnecter', scroll=True)
                            ui.find('Se connecter')
                            deadline = time.monotonic() + 10
                            while records() and time.monotonic() < deadline:
                                time.sleep(.1)
                            assert not records(), 'native client logout retained encrypted credentials'
                            write(directory / 'client-logout.json', {'credential_records': 0, 'before_harness_cleanup': True})
                            snapshot('client-logout', CacheImageExpectation.CLEARED if kind is FixtureKind.CACHE else None)
                        case AppStep.STORE_FAILURE:
                            assert not records(), 'logged-out restart restored credentials'
                            device('shell', 'run-as', PACKAGE, 'chmod', '500', 'shared_prefs')
                            try:
                                login()
                                ui.find('Stockage sécurisé indisponible', match=contains)
                                assert not records(), 'failed native commit created a credential file'
                                snapshot('native-store-unavailable')
                            finally:
                                device('shell', 'run-as', PACKAGE, 'chmod', '700', 'shared_prefs')
                        case AppStep.SIGNED_OUT:
                            assert not records()
                        case _:
                            raise AssertionError('Unhandled Android application step')
                    device('shell', 'am', 'force-stop', PACKAGE)
            except BaseException as error:
                if installed and 'ui' in locals():
                    ui.capture('failure')
                write(directory / 'failure.json', {'status': 'FAIL', 'failure': str(error)})
                raise
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
                device('shell', 'wm', 'size', 'reset', check=False)
                device('shell', 'wm', 'density', 'reset', check=False)
                for key, value in rotation.items():
                    if value == 'null':
                        device('shell', 'settings', 'delete', 'system', key)
                    else:
                        device('shell', 'settings', 'put', 'system', key, value)
                if installed:
                    device('uninstall', PACKAGE)
                if errors:
                    raise RuntimeError('; '.join(errors))
    write(directory / 'result.json', {'status': 'AUTOMATED_CHECKS_PASS', 'server': validate_app_report(control, 2, 2 if lifecycle else 1), 'apk_sha256': digest(apk), 'native_background_foreground_cycle': lifecycle, 'fixture_kind': kind.value, 'cache_load': load_report, 'actual_native_commit_failure_without_fallback': True, 'screenshots_require_visual_inspection': True})


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
