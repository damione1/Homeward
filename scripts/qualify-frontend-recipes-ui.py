#!/usr/bin/env python3
"""Opt-in NHF009 native macOS UI qualification over real isolated TLS.

Run through scripts/frontend.py run --cargo. Only the final device driver is
fake. No provider URL or production endpoint is accepted. The default runs four
nominal theme/text cases and the security/lifecycle cases at normal light text.
Inspect retained screenshots before claiming visual qualification. Android and
physical-device effects remain separate final/manual qualifications.
"""
import argparse
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import platform
import plistlib
import runpy
import shutil
import subprocess
import time
from urllib.parse import urlsplit
ROOT = Path(__file__).resolve().parents[1]
PROTOCOL = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-recipes.py'))
Operation = PROTOCOL['Operation']
write = None
session = None

class CLIEnum(str, Enum):

    def __str__(self):
        return self.value

class Scenario(CLIEnum):
    NOMINAL = 'nominal'
    CAPABILITY = 'capability-withdrawal'
    WRITE = 'write-withdrawal'
    OFFLINE = 'offline'
    POPUP_DETAIL = 'popup-detail'

    def operations(self):
        match self:
            case Scenario.NOMINAL:
                return [Operation.UNLOCK, Operation.COVER_CLOSE, Operation.ALARM_AWAY]
            case Scenario.CAPABILITY | Scenario.WRITE | Scenario.OFFLINE:
                return [Operation.COVER_OPEN]
            case Scenario.POPUP_DETAIL:
                return []
        raise AssertionError('unhandled native scenario')

    def controls(self):
        return {Scenario.CAPABILITY: (Control.WITHDRAW, Control.REDISCOVER), Scenario.WRITE: (Control.DENY, Control.ALLOW), Scenario.OFFLINE: (Control.OUTAGE, Control.NETWORK)}[self]

class Appearance(CLIEnum):
    LIGHT = 'Light'
    DARK = 'Dark'

class TextSize(CLIEnum):
    STANDARD = 'Standard'
    DOUBLE = 'Double'

class Case(CLIEnum):
    LIGHT_STANDARD = 'light-standard'
    LIGHT_DOUBLE = 'light-double'
    DARK_STANDARD = 'dark-standard'
    DARK_DOUBLE = 'dark-double'

    def preferences(self):
        return {Case.LIGHT_STANDARD: (Appearance.LIGHT, TextSize.STANDARD), Case.LIGHT_DOUBLE: (Appearance.LIGHT, TextSize.DOUBLE), Case.DARK_STANDARD: (Appearance.DARK, TextSize.STANDARD), Case.DARK_DOUBLE: (Appearance.DARK, TextSize.DOUBLE)}[self]

class Command(Enum):
    INFO = 'info'
    TREE = 'tree'
    FILL = 'fill-login'
    RESIZE = 'resize'
    TAP = 'tap'
    SCROLL = 'scroll'
    SIGN_OUT = 'sign-out'
    CLOSE = 'close'
    GESTURE = 'gesture'
    BEGIN = 'gesture-begin'
    RELEASE = 'release'
    KEY = 'key'

class Gesture(Enum):
    TAP = 'tap'
    HOLD = 'hold'
    DOUBLE = 'double-tap'

class Control(Enum):
    DROP = 'drop-observations'
    RESTORE = 'restore-observations'
    WITHDRAW = 'withdraw-cover'
    REDISCOVER = 'rediscover'
    DENY = 'deny-write'
    ALLOW = 'allow-write'
    OUTAGE = 'outage'
    NETWORK = 'restore-network'

sha = PROTOCOL['sha']
wait_file = PROTOCOL['wait_file']
stop = PROTOCOL['stop']

def source_files(roots):
    result = {}
    for key, root in roots.items():
        paths = subprocess.check_output(['git', 'ls-files', '--cached', '--others', '--exclude-standard', '-z'], cwd=root).decode().split('\x00')
        result[key] = {p: sha(root / p) for p in paths if p and (root / p).is_file()}
    return result

def build(proof, config, bundle_label='Homeward Recipes'):
    server = proof / 'server.test'
    subprocess.run(['go', 'test', '-c', '-race', '-o', str(server), './internal/server'], cwd=ROOT, check=True)
    helper = proof / 'native-ui'
    modules = Path(config['frontend_cache']) / 'session-spike/swift-modules'
    subprocess.run(['xcrun', 'swiftc', '-module-cache-path', str(modules), str(ROOT / 'qualification/frontend/native-ui.swift'), '-o', str(helper)], check=True)
    subprocess.run(['cargo', 'build', '--locked', '-p', 'homeward-app', '--bin', 'homeward-desktop'], cwd=ROOT, check=True)
    bundle = proof / (bundle_label + '.app/Contents')
    (bundle / 'MacOS').mkdir(parents=True)
    binary = bundle / 'MacOS/homeward-desktop'
    os.link(Path(os.environ['CARGO_TARGET_DIR']) / 'debug/homeward-desktop', binary)
    with (bundle / 'Info.plist').open('wb') as output:
        plistlib.dump(dict(CFBundleExecutable=binary.name, CFBundleIdentifier='ca.homeward.frontend.dev', CFBundleName=bundle_label, CFBundlePackageType='APPL', NSHighResolutionCapable=True), output)
    record = dict(binary=str(binary), server=str(server), helper=str(helper), binaries={str(p.relative_to(proof)): sha(p) for p in [binary, server, helper]})
    (proof / 'build.json').write_text(json.dumps(record, indent=2) + '\n')
    return record

def run_case(proof, build, scenario, case):
    appearance, text_size = case.preferences()
    directory = proof / (scenario.value + '-' + case.value)
    directory.mkdir(mode=0o700)
    source = directory / 'sources'
    shutil.copytree(ROOT / 'qualification/frontend/recipes', source)
    fixture = directory / 'fixture'
    config = source / 'native-home.yaml'
    helper = Path(build['helper'])
    binary = Path(build['binary'])
    server = app = None
    private = None
    namespace = None
    pressed = None
    product = Path.home() / 'Library/Application Support/Homeward'
    public = product / 'endpoint.json'
    preferences = product / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('preserve existing product settings')
    if any((Path(p.strip()).name == 'homeward-desktop' for p in subprocess.check_output(['ps', '-axo', 'comm'], text=True).splitlines()[1:])):
        raise RuntimeError('preserve existing Homeward process')
    sequence = 0
    checks = []

    def present():
        result = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, timeout=10)
        assert result.returncode in (0, 44)
        return result.returncode == 0

    def native(command, **fields):
        if not isinstance(command, Command):
            raise TypeError('native command requires its enum')
        result = subprocess.run([str(helper), '-'], input=json.dumps(dict(pid=app.pid, command=command.value, syntheticOnly=True, **fields)), capture_output=True, text=True, timeout=10)
        result.check_returncode()
        return json.loads(result.stdout) if result.stdout.strip() else None

    def tree():
        return native(Command.TREE)

    def capture(name):
        info = native(Command.INFO)
        subprocess.run(['screencapture', '-x', '-o', '-l', str(info['window_id']), str(directory / (name + '.png'))], check=True, capture_output=True)
        write(directory / (name + '.tree.json'), tree())
        write(directory / (name + '.window.json'), info)

    def wait(label, timeout=25):
        deadline = time.monotonic() + timeout
        while not any((label in value for row in tree() for value in [row['title'], row['description'], row['text']])):
            if app.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('native label missing: ' + label)
            time.sleep(0.1)

    def control(action):
        nonlocal sequence
        assert isinstance(action, Control)
        sequence += 1
        temporary = fixture / 'command.writing'
        write(temporary, dict(sequence=sequence, action=action.value))
        temporary.replace(fixture / 'command.json')
        response = wait_file(fixture / f'ack-{sequence:02d}.json', server)
        assert response == dict(sequence=sequence, action=action.value)

    def find(label):
        for attempt in range(28):
            bounds = native(Command.INFO)['bounds']
            rows = [r for r in tree() if r['role'] == 'AXButton' and label in (r['title'], r['description']) and (r['width'] > 5) and (r['height'] > 5)]
            if rows:
                row = rows[0]
                center = row['y'] + row['height'] / 2 - bounds['Y']
                if 35 < center < bounds['Height'] - 35:
                    return row
                native(Command.SCROLL, delta=-300 if center > 35 else 300)
            else:
                native(Command.SCROLL, delta=-300 if attempt < 14 else 300)
        write(directory / 'failed-find.tree.json', tree())
        raise AssertionError('native button missing: ' + label)

    def gesture(label, kind=Gesture.TAP):
        assert isinstance(kind, Gesture)
        find(label)
        native(Command.GESTURE, label=label, gesture=kind.value)

    def absent(label, timeout=10):
        deadline = time.monotonic() + timeout
        while any((label in (r['title'], r['description'], r['text']) for r in tree())):
            if time.monotonic() > deadline:
                raise AssertionError('native label still present: ' + label)
            time.sleep(0.1)

    def focused(label):
        deadline = time.monotonic() + 5
        while not any((r['focused'] and label in (r['title'], r['description']) for r in tree())):
            if time.monotonic() > deadline:
                write(directory / 'failed-focus.tree.json', tree())
                raise AssertionError('native focus not restored: ' + label)
            time.sleep(0.1)

    def ack(card):
        button = find(card + ' · Arrêter le suivi')
        wait('Acquittée · confirmation en attente')
        assert button['height'] >= 48 and button['width'] >= 48
        gesture(card + ' · Arrêter le suivi')
        wait_until(lambda: card_message(card, 'Suivi arrêté sur cet appareil'), 'local tracking stop did not settle')

    def wait_until(predicate, description, timeout=25):
        deadline = time.monotonic() + timeout
        while not predicate():
            if app.poll() is not None or time.monotonic() > deadline:
                raise AssertionError(description)
            time.sleep(0.05)

    def enabled(label):
        return any((r['role'] == 'AXButton' and label in (r['title'], r['description']) and r['enabled'] for r in tree()))

    def card_message(card, message):
        rows = tree()
        groups = [r for r in rows if r['role'] == 'AXGroup' and card in (r['title'], r['description'])]
        return any((message in (r['title'], r['text']) and group['x'] - 1 <= r['x'] <= group['x'] + group['width'] + 1 and (group['y'] - 1 <= r['y'] <= group['y'] + group['height'] + 1) for group in groups for r in rows if r['role'] == 'AXStaticText'))

    def held_control(action, changed):
        nonlocal pressed
        find('Volet simulé')
        pressed = native(Command.BEGIN, label='Volet simulé')
        assert set(pressed) == {'x', 'y'}
        try:
            control(action)
            wait_until(changed, 'held gesture did not observe withdrawal')
        finally:
            native(Command.RELEASE, **pressed)
            pressed = None
        absent('Confirmer')

    def fresh_cover():
        deadline = time.monotonic() + 30
        while not enabled('Volet simulé · Ouvrir'):
            if enabled('Afficher ma maison'):
                gesture('Afficher ma maison')
            if app.poll() is not None or time.monotonic() > deadline:
                raise AssertionError('restored cover not available')
            native(Command.SCROLL, delta=300)
            time.sleep(0.1)
        find('Volet simulé · Ouvrir')

    def has_label(label):
        return any((label in (r['title'], r['description'], r['text']) for r in tree()))
    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            env = dict(os.environ, HOMEWARD_NHF009_FIXTURE=str(fixture), HOMEWARD_NHF009_SOURCES=str(source), HOMEWARD_NHF007_CATALOGUE=str(source / 'native-catalogue.json'), HOMEWARD_NHF009_SCENARIO=PROTOCOL['Scenario'].ACK.value)
            env.pop('HOMEWARD_NHF005_DASHBOARD', None)
            server = subprocess.Popen([build['server'], '-test.run=^TestNHF009ControlServer$', '-test.v', '-test.timeout=8m'], cwd=ROOT, env=env, stdout=server_log, stderr=subprocess.STDOUT)
            private = wait_file(fixture / 'ready.json', server)
            origin = urlsplit(private['origin'])
            if private['mode'] != 'synthetic-only' or origin.hostname != '127.0.0.1' or origin.scheme != 'https' or (not origin.port) or (private['entity_count'] != 4):
                raise RuntimeError('only isolated fake-device TLS fixture admitted')
            namespace = session['namespace'](private)
            if present():
                raise RuntimeError('preserve existing synthetic credential')
            product.mkdir(parents=True, exist_ok=True)
            write(public, session['public_profile'](private))
            write(preferences, dict(version='1', preferences=dict(appearance=appearance.value, surfaces='Opaque', text=text_size.value, motion='Reduced')))
            app = subprocess.Popen([str(binary)], cwd=ROOT, env=dict(os.environ, SLINT_BACKEND='winit-software'), stdout=app_log, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 20
            while True:
                try:
                    native(Command.INFO)
                    break
                except subprocess.CalledProcessError:
                    if app.poll() is not None or time.monotonic() > deadline:
                        raise
                    time.sleep(0.1)
            native(Command.RESIZE, width=1280, height=800)
            wait('Se connecter')
            native(Command.FILL, loginName=private['login_name'], password=private['password'])
            wait('Afficher ma maison')
            native(Command.TAP, label='Afficher ma maison')
            wait('Volet simulé')
            capture('home-loaded')
            match scenario:
                case Scenario.NOMINAL:
                    gesture('Volet simulé')
                    wait('Retour aux éléments')
                    capture('cover-details')
                    gesture('Retour aux éléments')
                    wait('Volet simulé')
                    focused('Volet simulé')
                    checks.append('tap opens actual details and Back restores exact header focus')
                    opener_before = find('Accès simulé')
                    gesture('Accès simulé')
                    wait('Serrure simulée')
                    capture('entry-popup')
                    gesture('Serrure simulée', Gesture.HOLD)
                    wait('Déverrouiller l’accès simulé ?')
                    capture('unlock-confirmation')
                    gesture('Annuler')
                    absent('Confirmer')
                    focused('Serrure simulée')
                    gesture('Serrure simulée', Gesture.HOLD)
                    wait('Déverrouiller l’accès simulé ?')
                    gesture('Confirmer')
                    ack('Serrure simulée')
                    capture('lock-ack')
                    gesture('Fermer')
                    wait('Volet simulé')
                    focused('Accès simulé')
                    opener_after = find('Accès simulé')
                    write(directory / 'popup-return-geometry.json', dict(before=opener_before, after=opener_after))
                    if appearance is Appearance.LIGHT and text_size is TextSize.STANDARD:
                        assert abs(opener_after['y'] - opener_before['y']) < 1, 'visible popup opener must preserve its saved scroll position'
                    checks.append('Bubble popup, cancel without Submit, confirmed Lock Unlock, local stop >=48 px, close restores opener')
                    gesture('Volet simulé', Gesture.HOLD)
                    wait('Fermer le volet simulé ?')
                    gesture('Confirmer')
                    ack('Volet simulé')
                    capture('cover-ack')
                    checks.append('hold stays distinct from tap and submits exactly the confirmed Cover Close')
                    gesture('Alarme simulée · Armer en absence')
                    ack('Alarme simulée')
                    capture('alarm-ack')
                    checks.append('native alarm operation uses the same typed TLS path; observed state stays distinct from ACK')
                    gesture('Volet simulé', Gesture.DOUBLE)
                    wait('Dashboard secondaire simulé')
                    capture('secondary-dashboard')
                    gesture('Retour')
                    wait('Volet simulé')
                    focused('Volet simulé')
                    gesture('Volet simulé', Gesture.DOUBLE)
                    wait('Dashboard secondaire simulé')
                    gesture('Retour à la maison simulée')
                    wait('Volet simulé')
                    checks.append('double tap, real Back with exact source focus, and explicit return link navigate the captured authorized dashboard cohort without a device command')
                    held_control(Control.DROP, lambda: has_label('Condition de visibilité non disponible dans la vue autorisée à jour.'))
                    capture('visibility-withdrawn')
                    control(Control.RESTORE)
                    wait('Volet simulé')
                    absent('Confirmer')
                    checks.append('observation-based visibility withdrawn after actual pointer down cancels held command, no replay on restoration')
                    updated = config.read_text().replace('name: Accès simulé', 'name: Entrées rechargées')
                    temporary = config.with_suffix('.writing')
                    temporary.write_text(updated)
                    temporary.replace(config)
                    wait('Entrées rechargées')
                    absent('Accès simulé')
                    capture('dashboard-hot-reloaded')
                    checks.append('server YAML changed while app lives; Dashboard Watch reloads labels without client rebuild')
                case Scenario.CAPABILITY | Scenario.WRITE | Scenario.OFFLINE:
                    find('Volet simulé · Ouvrir')
                    assert enabled('Volet simulé · Ouvrir')
                    withdraw, restore = scenario.controls()
                    held_control(withdraw, lambda: not enabled('Volet simulé · Ouvrir'))
                    capture('withdrawn-while-held')
                    control(restore)
                    fresh_cover()
                    absent('Confirmer')
                    capture('restored-without-replay')
                    gesture('Volet simulé · Ouvrir')
                    ack('Volet simulé')
                    capture('fresh-gesture-admitted')
                    checks.append('old held command fenced across withdrawal and restoration; only a fresh explicit Cover Open admitted')
                case Scenario.POPUP_DETAIL:
                    opener = find('Accès simulé')
                    gesture('Accès simulé')
                    wait('Serrure simulée')
                    gesture('Serrure simulée')
                    wait('Retour aux éléments')
                    capture('popup-detail')
                    gesture('Retour aux éléments')
                    wait('Serrure simulée')
                    focused('Serrure simulée')
                    capture('popup-detail-return')
                    gesture('Fermer')
                    focused('Accès simulé')
                    restored = find('Accès simulé')
                    assert abs(restored['y'] - opener['y']) < 1
                    checks.append('popup detail Back and popup close restore both exact native focus/scroll contexts without a command')
            native(Command.SIGN_OUT)
            wait('Se connecter')
            assert not present()
            native(Command.CLOSE)
            assert app.wait(timeout=15) == 0
            (fixture / 'stop').touch(mode=0o600)
            assert server.wait(timeout=25) == 0
            final = json.loads((fixture / 'result.json').read_text())
            expected = [operation.arguments() for operation in scenario.operations()]
            submitted = final['submit_ids'] or []
            assert final['server_joined'] and final['fake_driver_calls'] == final['command_driver_entries'] == len(submitted) == len(set(submitted)) == len(expected)
            assert final['device_arguments'] == expected and final['protocol_confirmations'] == final['observed_effects'] == final['real_device_writes'] == 0
            assert all((id in submitted for id in (final['get_ids'] or []) + (final['watch_ids'] or [])))
            assert final['native_login_calls'] == final['native_revokes'] == 1 and final['dashboard_get_calls'] >= 2 and (final['dashboard_watch_calls'] >= 1)
            report = dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', scenario=scenario.value, case=case.value, checks=checks, server=final, runtime_sources={p.name: sha(p) for p in source.iterdir() if p.is_file()}, children_joined=True, mode='synthetic-only')
            write(directory / 'result.json', report)
            return dict(proof=str(directory), result_sha256=sha(directory / 'result.json'), scenario=scenario.value, case=case.value)
        except BaseException:
            if app is not None and app.poll() is None:
                with contextlib.suppress(Exception):
                    capture('failure')
            raise
        finally:
            if pressed is not None and app is not None and (app.poll() is None):
                with contextlib.suppress(Exception):
                    native(Command.RELEASE, **pressed)
            try:
                stop(app)
            finally:
                try:
                    stop(server)
                finally:
                    if namespace is not None and present():
                        subprocess.run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, check=True)
                    if public.exists():
                        assert private is not None and json.loads(public.read_text()) == session['public_profile'](private)
                        public.unlink()
                    if preferences.exists():
                        preferences.unlink()
                    with contextlib.suppress(FileNotFoundError):
                        (fixture / 'ready.json').unlink()

def main():
    global session, write
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scenario', type=Scenario, choices=list(Scenario), action='append')
    parser.add_argument('--case', type=Case, choices=list(Case), action='append')
    args = parser.parse_args()
    if platform.system() != 'Darwin':
        parser.error('actual macOS native UI required; Android is final-stage')
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('run through scripts/frontend.py run --cargo')
    session = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
    write = session['write']
    config = session['CONFIG']
    assert Path(os.environ['CARGO_TARGET_DIR']) == Path(config['backend_cache']) / 'workspace/target'
    proof = Path(config['frontend_cache']) / 'recipe-ui' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    print('NHF009 native proof: ' + str(proof), flush=True)
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    runner = runpy.run_path(str(ROOT / 'scripts/frontend.py'))
    runner['sources'](config, proof)
    roots = dict(homeward=ROOT, free=Path(config['free_kit']), pro=Path(config['pro_kit']))
    before = source_files(roots)
    results = []
    try:
        artifacts = build(proof, config)
        for scenario in args.scenario or list(Scenario):
            for case in args.case or (list(Case) if scenario is Scenario.NOMINAL else [Case.LIGHT_STANDARD]):
                print('Native scenario ' + scenario.value + ' / ' + case.value, flush=True)
                results.append(run_case(proof, artifacts, scenario, case))
        assert source_files(roots) == before
        write(proof / 'result.json', dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', cases=results, build=artifacts, all_current_source_bytes_unchanged=True, platform=platform.platform(), backend='winit-software', no_real_device_actions=True, android_qualified=False))
    except BaseException:
        write(proof / 'result.json', dict(status='FAILED', completed_cases=results, current_source_bytes_unchanged=source_files(roots) == before))
        raise
    print('NHF009 native UI PASS (captures need inspection): ' + str(proof), flush=True)
if __name__ == '__main__':
    main()
