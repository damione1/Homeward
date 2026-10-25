#!/usr/bin/env python3
"""Native action cards on the isolated four-family Go/TLS server.

Run through frontend.py run --cargo. The fixture attaches only its final fake
driver. This harness never accepts a production origin or a physical device.
The controller fault suite remains separate. Captures require inspection before
visual acceptance; an emulator is not physical-device qualification.
"""
import argparse
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import plistlib
import runpy
import shlex
import shutil
import subprocess
import time
from urllib.parse import urlsplit
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
ANDROID = runpy.run_path(str(ROOT / 'scripts/frontend-android-input.py'))
TRANSPORT = SESSION['TRANSPORT']
Platform = SESSION['Platform']
PlatformSelection = SESSION['PlatformSelection']
run, write, digest = SESSION['run'], SESSION['write'], SESSION['digest']


class Scenario(Enum):
    ACK_ONLY = 'ack-only'
    LOST_SUBMIT = 'lost-submit'
    EXPIRE = 'expire'
    REFUSAL = 'refusal'
    RECONNECT = 'reconnect'
    SERVER_RESTART = 'server-restart'
    WRITE_WITHDRAWAL = 'write-withdrawal'
    CAPABILITY_WITHDRAWAL = 'capability-withdrawal'
    LAYOUT_CHANGE = 'layout-change'
    CONFIRM = 'confirm'
    LIGHT_SLIDER = 'light-slider'

    def server_scenario(self):
        match self:
            case Scenario.ACK_ONLY | Scenario.LOST_SUBMIT | Scenario.EXPIRE | Scenario.REFUSAL:
                return self.value
            case (Scenario.RECONNECT | Scenario.SERVER_RESTART | Scenario.WRITE_WITHDRAWAL
                  | Scenario.CAPABILITY_WITHDRAWAL | Scenario.LAYOUT_CHANGE | Scenario.CONFIRM | Scenario.LIGHT_SLIDER):
                return Scenario.ACK_ONLY.value
        raise AssertionError('Unhandled native action scenario')

    def unsent(self):
        return self in (Scenario.WRITE_WITHDRAWAL, Scenario.CAPABILITY_WITHDRAWAL, Scenario.LAYOUT_CHANGE)


class Control(Enum):
    OUTAGE = 'outage'
    RESTORE = 'restore'
    DENY_WRITE = 'deny-write'
    ALLOW_WRITE = 'allow-write'
    WITHDRAW_LIGHT = 'withdraw-light'
    REDISCOVER = 'rediscover'
    CONFIRM = 'confirm-last'
    RESTART = 'restart'


class TouchPhase(Enum):
    DOWN = 'DOWN'
    UP = 'UP'


class NativeCommand(Enum):
    INFO = 'info'
    FILL_LOGIN = 'fill-login'
    TAP = 'tap'
    RESIZE = 'resize'
    TREE = 'tree'
    CLOSE = 'close'
    SIGN_OUT = 'sign-out'
    PRESS = 'press'
    RELEASE = 'release'
    SCROLL = 'scroll'


@contextlib.contextmanager
def fixture(directory, binary, scenario):
    directory.mkdir(mode=0o700)
    control = directory / 'fixture'
    dashboard = directory / 'server-dashboard'
    dashboard.mkdir(mode=0o700)
    shutil.copyfile(ROOT / 'qualification/frontend/dashboards/actions.yaml', dashboard / 'dashboard.yaml')
    env = dict(os.environ, HOMEWARD_NHF006_FIXTURE=str(control),
               HOMEWARD_NHF006_SCENARIO=scenario.server_scenario(),
               HOMEWARD_NHF006_DEVICES=str(ROOT / 'qualification/frontend/action-devices.json'),
               HOMEWARD_NHF005_DASHBOARD=str(dashboard / 'dashboard.yaml'))
    server = None
    with (directory / 'server.log').open('wb') as output:
        try:
            server = subprocess.Popen([str(binary), '-test.run=^TestNHF006ActionServer$',
                                       '-test.v', '-test.timeout=10m'], cwd=ROOT, env=env,
                                      stdout=output, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 60
            while not (control / 'ready.json').is_file():
                if server.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError('Isolated action server did not become ready')
                time.sleep(.05)
            private = json.loads((control / 'ready.json').read_text())
            origin = urlsplit(private['origin'])
            if (private['mode'] != 'synthetic-only' or origin.scheme != 'https'
                    or origin.hostname != '127.0.0.1' or not origin.port):
                raise RuntimeError('Only isolated loopback TLS is accepted')
            sequence = 0
            def send(action):
                nonlocal sequence
                if not isinstance(action, Control):
                    raise TypeError('Synthetic control requires its enum')
                sequence += 1
                temporary = control / 'command.writing'
                write(temporary, dict(sequence=sequence, action=action.value))
                temporary.replace(control / 'command.json')
                ack = control / ('ack-%02d.json' % sequence)
                deadline = time.monotonic() + 25
                while not ack.exists():
                    if server.poll() is not None or time.monotonic() >= deadline:
                        raise RuntimeError('Isolated control did not complete: ' + action.value)
                    time.sleep(.05)
                observed = json.loads(ack.read_text())
                assert observed['sequence'] == sequence and observed['action'] == action.value
            yield private, send
            (control / 'stop').touch(mode=0o600)
            if server.wait(timeout=25) != 0:
                raise RuntimeError('Synthetic server failed to join')
        finally:
            TRANSPORT['stop'](server, timeout=20)
            with contextlib.suppress(FileNotFoundError):
                (control / 'ready.json').unlink()


def validate_server(directory, scenario):
    report = json.loads((directory / 'fixture/result.json').read_text())
    ids = report['submit_ids'] or []
    submitted = 0 if scenario.unsent() else 1
    dispatched = 0 if scenario.unsent() or scenario is Scenario.REFUSAL else 1
    if (not report['server_joined'] or report['entity_count'] != 4
            or report['fake_driver_calls'] != dispatched or report['command_driver_entries'] != dispatched
            or len(ids) != submitted or report['native_login_calls'] != 1
            or report['native_revokes'] != 1):
        raise AssertionError('Native gesture did not produce exactly one isolated command')
    reads = (report['get_ids'] or []) + (report['watch_ids'] or [])
    if dispatched and (not report['get_ids'] or not report['watch_ids'] or any(value != ids[0] for value in reads)):
        raise AssertionError('Recovery did not read the original command ID')
    if not dispatched and reads:
        raise AssertionError('Unsent/refused command was tracked')
    if scenario is Scenario.RECONNECT and (len(report['get_ids']) < 2 or len(report['watch_ids']) < 2):
        raise AssertionError('Connection recovery did not reopen Get/Watch')
    if report['lost_responses'] != int(scenario is Scenario.LOST_SUBMIT):
        raise AssertionError('Lost Submit response fault count differs')
    if report['refusal_faults'] != int(scenario is Scenario.REFUSAL):
        raise AssertionError('Configuration-race fault count differs')
    expected_servers = 2 if scenario is Scenario.SERVER_RESTART else 1
    if report['servers_started'] != expected_servers or report['servers_joined'] != expected_servers:
        raise AssertionError('Restart did not join every isolated server')
    if report['dashboard_get_calls'] < 2 or report['dashboard_watch_calls'] < 1:
        raise AssertionError('Native cards did not load/watch the server dashboard')
    return report


def desktop(directory, server, scenario, helper, binary):
    data = Path.home() / 'Library/Application Support/Homeward'
    public, preferences = data / 'endpoint.json', data / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve an existing product profile/preferences')
    receipt = dict(status='FAIL', platform=Platform.DESKTOP.value, scenario=scenario.value)
    with fixture(directory, server, scenario) as (private, control):
        namespace = SESSION['namespace'](private)
        def present():
            result = subprocess.run(['security', 'find-generic-password', '-s',
                                     'ca.homeward.session.v1', '-a', namespace],
                                    capture_output=True, timeout=10)
            if result.returncode not in (0, 44):
                raise RuntimeError('Keychain metadata unavailable')
            return result.returncode == 0
        if present():
            raise RuntimeError('Preserve existing synthetic credentials')
        data.mkdir(parents=True, exist_ok=True)
        write(public, SESSION['public_profile'](private))
        child = None
        pressed = None
        def native(command, **fields):
            result = subprocess.run([str(helper), '-'], input=json.dumps(dict(
                pid=child.pid, command=command.value, syntheticOnly=True, **fields)),
                capture_output=True, text=True, timeout=10)
            if result.returncode:
                raise RuntimeError('Owned native command failed: ' + command.value)
            return json.loads(result.stdout) if result.stdout.strip() else None
        def tree():
            return native(NativeCommand.TREE)
        def wait(label, timeout=20):
            deadline = time.monotonic() + timeout
            while not any(label in (row['title'], row['description'], row['text']) for row in tree()):
                if child.poll() is not None or time.monotonic() >= deadline:
                    raise AssertionError('Missing native label: ' + label)
                time.sleep(.1)
        def navigate(label):
            if not any(label in (row['title'], row['description']) for row in tree()):
                native(NativeCommand.TAP, label='Navigation')
                wait(label)
            native(NativeCommand.TAP, label=label)
        def capture(name):
            info = native(NativeCommand.INFO)
            time.sleep(.2)
            run(['screencapture', '-x', '-o', '-l', str(info['window_id']), directory / (name + '.png')], capture_output=True)
            write(directory / (name + '.tree.json'), tree())
        def start(name):
            nonlocal child
            with (directory / (name + '.log')).open('wb') as output:
                child = subprocess.Popen([str(binary)], stdout=output, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 15
            while True:
                try:
                    native(NativeCommand.INFO)
                    break
                except RuntimeError:
                    if child.poll() is not None or time.monotonic() >= deadline:
                        raise
                    time.sleep(.1)
            native(NativeCommand.RESIZE, width=1280, height=800)
        def close():
            native(NativeCommand.CLOSE)
            if child.wait(timeout=15) != 0:
                raise RuntimeError('Owned application failed to close')
        try:
            start('login')
            wait('Se connecter')
            native(NativeCommand.FILL_LOGIN, loginName=private['login_name'], password=private['password'])
            wait('Afficher ma maison')
            assert present()
            native(NativeCommand.TAP, label='Afficher ma maison')
            wait('Relais synthétique · Allumer')
            if scenario is Scenario.ACK_ONLY:
                navigate('Pièces')
                wait('Choisissez une pièce')
                room = next(row['title'] for row in tree() if row['role'] == 'AXButton' and row['title'].startswith('Pièce synthétique, '))
                native(NativeCommand.TAP, label=room)
                wait('light.basement_lights · Allumer')
                capture('room-cards')
                native(NativeCommand.TAP, label='light.basement_lights · Détails')
                wait('Retour aux éléments')
                wait('light.basement_lights · Luminosité')
                capture('light-detail-capabilities')
                native(NativeCommand.TAP, label='Retour aux éléments')
                native(NativeCommand.SCROLL, delta=-1000)
                wait('Liste complète des éléments')
                native(NativeCommand.TAP, label='Liste complète des éléments')
                assert any(row['role'] == 'AXTable' for row in tree())
                capture('room-inventory-secondary')
                native(NativeCommand.TAP, label='Retour')
                native(NativeCommand.SCROLL, delta=1000)
                navigate('Accueil')
                wait('Relais synthétique · Allumer')
            capture('before-command')
            if scenario.unsent():
                pressed = native(NativeCommand.PRESS, label='Lumière synthétique · Luminosité', fraction=.65)
                capture('gesture-before-withdrawal')
                match scenario:
                    case Scenario.WRITE_WITHDRAWAL:
                        control(Control.DENY_WRITE)
                    case Scenario.CAPABILITY_WITHDRAWAL:
                        control(Control.WITHDRAW_LIGHT)
                    case Scenario.LAYOUT_CHANGE:
                        dashboard = directory / 'server-dashboard/dashboard.yaml'
                        text = dashboard.read_text().replace('Maison synthétique', 'Maison modifiée').replace('Lumière synthétique', 'Lampe remaniée')
                        temporary = dashboard.with_suffix('.writing')
                        temporary.write_text(text)
                        temporary.replace(dashboard)
                    case (Scenario.ACK_ONLY | Scenario.LOST_SUBMIT | Scenario.EXPIRE | Scenario.REFUSAL
                          | Scenario.RECONNECT | Scenario.SERVER_RESTART | Scenario.CONFIRM | Scenario.LIGHT_SLIDER):
                        raise AssertionError('Guard scenario expected')
                deadline = time.monotonic() + 20
                while True:
                    sliders = [row for row in tree() if row['role'] == 'AXSlider' and row['title'] == 'Lumière synthétique · Luminosité']
                    if not sliders or not sliders[0]['enabled']:
                        break
                    if time.monotonic() >= deadline:
                        raise AssertionError('Held gesture did not lose its original control')
                    time.sleep(.1)
                capture('gesture-after-withdrawal')
                native(NativeCommand.RELEASE, **pressed)
                pressed = None
                match scenario:
                    case Scenario.WRITE_WITHDRAWAL:
                        control(Control.ALLOW_WRITE)
                    case Scenario.CAPABILITY_WITHDRAWAL:
                        control(Control.REDISCOVER)
                    case Scenario.LAYOUT_CHANGE:
                        pass
                    case (Scenario.ACK_ONLY | Scenario.LOST_SUBMIT | Scenario.EXPIRE | Scenario.REFUSAL
                          | Scenario.RECONNECT | Scenario.SERVER_RESTART | Scenario.CONFIRM | Scenario.LIGHT_SLIDER):
                        raise AssertionError('Guard scenario expected')
                time.sleep(1)
                capture('gesture-released-no-replay')
            else:
                if scenario is Scenario.LIGHT_SLIDER:
                    pressed = native(NativeCommand.PRESS, label='Lumière synthétique · Luminosité', fraction=.65)
                    capture('slider-draft')
                    native(NativeCommand.RELEASE, **pressed)
                    pressed = None
                else:
                    native(NativeCommand.TAP, label='Relais synthétique · Allumer')
                if scenario is Scenario.REFUSAL:
                    wait('Non admise')
                    wait('La configuration a changé. Reprenez le geste sur la nouvelle vue.')
                    capture('refused-before-commit')
                else:
                    wait('Acquittée · confirmation en attente')
                    capture('ack-observation-unchanged')
                    match scenario:
                        case Scenario.EXPIRE:
                            wait('Résultat indéterminé', timeout=40)
                            capture('expired-after-dispatch')
                        case Scenario.RECONNECT:
                            control(Control.OUTAGE)
                            time.sleep(1.5)
                            capture('connection-cut')
                            control(Control.RESTORE)
                            wait('Acquittée · confirmation en attente')
                            capture('reconnected-same-command')
                        case Scenario.SERVER_RESTART:
                            control(Control.RESTART)
                            wait('Suivi en direct indisponible')
                            capture('no-live-after-server-restart')
                        case Scenario.CONFIRM:
                            control(Control.CONFIRM)
                            wait('Effet confirmé')
                            wait('Allumé')
                            capture('correlated-confirmation')
                        case Scenario.ACK_ONLY | Scenario.LOST_SUBMIT | Scenario.LIGHT_SLIDER:
                            pass
                        case Scenario.REFUSAL | Scenario.WRITE_WITHDRAWAL | Scenario.CAPABILITY_WITHDRAWAL | Scenario.LAYOUT_CHANGE:
                            raise AssertionError('Dispatched scenario expected')
                    if scenario in (Scenario.ACK_ONLY, Scenario.LOST_SUBMIT, Scenario.RECONNECT, Scenario.LIGHT_SLIDER):
                        title = 'Lumière synthétique' if scenario is Scenario.LIGHT_SLIDER else 'Relais synthétique'
                        native(NativeCommand.TAP, label=title + ' · Arrêter le suivi')
                        wait('Suivi arrêté sur cet appareil')
                        capture('tracking-stopped')
            if scenario is Scenario.ACK_ONLY:
                for width, height in [(1024, 600), (1280, 800), (800, 1280)]:
                    native(NativeCommand.RESIZE, width=width, height=height)
                    capture(f'cards-light-{width}x{height}')
                native(NativeCommand.RESIZE, width=1280, height=800)
                navigate('Préférences')
                native(NativeCommand.TAP, label='Thème sombre')
                native(NativeCommand.TAP, label='Texte à 200 %')
                navigate('Accueil')
                for width, height in [(1024, 600), (1280, 800), (800, 1280)]:
                    native(NativeCommand.RESIZE, width=width, height=height)
                    capture(f'cards-dark-200-{width}x{height}')
                native(NativeCommand.RESIZE, width=1280, height=800)
            close()
            start('restart')
            wait('Afficher ma maison')
            native(NativeCommand.TAP, label='Afficher ma maison')
            wait('En un regard')
            if scenario is Scenario.CONFIRM:
                wait('Relais synthétique · Éteindre')
            time.sleep(1)
            capture('restart-no-replay')
            native(NativeCommand.SIGN_OUT)
            wait('Se connecter')
            assert not present()
            capture('signed-out')
            close()
            receipt.update(status='AUTOMATED_CHECKS_PASS', keychain_absent_after_logout=True,
                           client_restart_without_replay=True, binary_sha256=digest(binary))
        except BaseException as error:
            receipt['failure'] = str(error)
            if child is not None and child.poll() is None:
                with contextlib.suppress(Exception):
                    capture('failure')
            raise
        finally:
            if pressed is not None and child is not None and child.poll() is None:
                with contextlib.suppress(Exception):
                    native(NativeCommand.RELEASE, **pressed)
            TRANSPORT['stop'](child, timeout=15)
            if present():
                run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True)
            if public.exists():
                assert json.loads(public.read_text()) == SESSION['public_profile'](private)
                public.unlink()
            if preferences.exists():
                preferences.unlink()
            write(directory / 'interaction-result.json', receipt)
    receipt['server'] = validate_server(directory, scenario)
    write(directory / 'result.json', receipt)


def android(directory, server, scenario, apk, ocr):
    device, package = SESSION['device'], SESSION['PACKAGE']
    receipt = dict(status='FAIL', platform=Platform.ANDROID.value, scenario=scenario.value)
    with fixture(directory, server, scenario) as (private, control):
        existing = device('shell', 'pm', 'path', package, check=False)
        if existing.stdout.strip() or existing.returncode not in (0, 1) or existing.stderr.strip():
            raise RuntimeError('Preserve an existing emulator application')
        installed = forwarded = False
        pressed = None
        port = urlsplit(private['origin']).port
        rotation = {key: device('shell', 'settings', 'get', 'system', key).stdout.strip()
                    for key in ['accelerometer_rotation', 'user_rotation']}
        ui = ANDROID['AndroidInput'](directory, ocr, TRANSPORT['ADB'], TRANSPORT['SERIAL'], device)
        contains = ANDROID['LabelMatch'].CONTAINS
        credential = 'shared_prefs/ca.homeward.session.v1.' + SESSION['namespace'](private) + '.xml'
        def records():
            result = device('shell', 'run-as', package, 'cat', credential, check=False)
            if result.returncode:
                if 'No such file' in result.stderr:
                    return []
                raise RuntimeError('Credential metadata unavailable')
            return ET.fromstring(result.stdout).findall('string[@name="record"]')
        def text(value):
            device('shell', 'input', 'text', shlex.quote(value.replace(' ', '%s')))
        def start():
            device('shell', 'am', 'start', '-n', package + '/android.app.NativeActivity')
        def navigate(label):
            rows, _, _, _ = ui.labels()
            if not any(ANDROID['normalized'](row['label']) == ANDROID['normalized'](label) for row in rows):
                ui.navigation()
            ui.tap(label)
        def card_button(rows, title, label):
            headings = [row for row in rows if ANDROID['normalized'](title) == ANDROID['normalized'](row['label'])]
            if len(headings) != 1:
                return None
            heading = headings[0]
            center = heading['x'] + heading['width'] / 2
            candidates = [row for row in rows if ANDROID['normalized'](row['label']) == ANDROID['normalized'](label)
                          and row['y'] > heading['y']
                          and abs(row['x'] + row['width'] / 2 - center) < 120]
            return min(candidates, key=lambda row: row['y']) if candidates else None
        def tap_card(title, label):
            # OCR sees visual button text. Resolve its column from the observed
            # card heading, never from installation IDs or assumed grid indexes.
            deadline = time.monotonic() + 20
            while True:
                rows, width, height, path = ui.labels()
                target = card_button(rows, title, label)
                if target is not None:
                    x, y = round(target['x'] + target['width'] / 2), round(target['y'] + target['height'] / 2)
                    assert 0 <= x < width and 0 <= y < height
                    write(path.with_suffix('.card-tap.json'), dict(title=title, button=target, x=x, y=y))
                    device('shell', 'input', 'tap', str(x), str(y))
                    return
                if time.monotonic() >= deadline:
                    raise AssertionError('Missing observed card/button: ' + title + '/' + label)
                time.sleep(.15)
        def touch(phase, point):
            if not isinstance(phase, TouchPhase):
                raise TypeError('Touch phase requires its enum')
            device('shell', 'input', 'motionevent', phase.value, str(point['x']), str(point['y']))
        def slider_point():
            rows, width, height, path = ui.labels()
            power = card_button(rows, 'Lumière synthétique', 'Allumer')
            details = card_button(rows, 'Lumière synthétique', 'Détails')
            if power is None or details is None:
                raise AssertionError('Observe the light controls before locating its slider')
            power_y, details_y = power['y'] + power['height'] / 2, details['y'] + details['height'] / 2
            # The public card puts its 40dp slider between these two observed
            # 48dp buttons. Use their actual centers, at the declared 160dpi.
            gap = details_y - power_y
            if not 80 <= gap <= 140:
                raise AssertionError('Unexpected observed slider/button geometry')
            point = dict(x=round(power['x'] + power['width'] / 2), y=round((power_y + details_y) / 2), gap=gap)
            assert 0 <= point['x'] < width and 0 <= point['y'] < height
            write(path.with_suffix('.slider-down.json'), dict(point=point, power=power, details=details))
            return point
        try:
            device('install', apk)
            installed = True
            device('reverse', '--no-rebind', f'tcp:{port}', f'tcp:{port}')
            forwarded = True
            device('shell', 'run-as', package, 'mkdir', '-p', 'files')
            run([TRANSPORT['ADB'], '-s', TRANSPORT['SERIAL'], 'shell', 'run-as', package, 'sh', '-c',
                 shlex.quote('cat > files/endpoint.json')], input=json.dumps(SESSION['public_profile'](private)), text=True, capture_output=True)
            device('shell', 'settings', 'put', 'system', 'accelerometer_rotation', '0')
            device('shell', 'settings', 'put', 'system', 'user_rotation', '0')
            device('shell', 'wm', 'density', '160')
            device('shell', 'wm', 'size', '1280x800')
            start()
            ui.tap('Identifiant', offset_y=40)
            text(private['login_name'])
            device('shell', 'input', 'keyevent', '61')
            text(private['password'])
            device('shell', 'input', 'keyevent', '66')
            ui.find('Afficher ma maison', match=contains)
            assert len(records()) == 1
            ui.tap('Afficher ma maison', match=contains)
            ui.find('Relais synthétique')
            if scenario is Scenario.ACK_ONLY:
                navigate('Pièces')
                ui.find('Choisissez une pièce')
                ui.tap('Pièce synthétique')
                ui.find('light.basement_lights', match=contains)
                ui.capture('room-cards')
                tap_card('light.basement_lights', 'Détails')
                ui.find('Retour aux éléments')
                ui.capture('light-detail-capabilities')
                device('shell', 'input', 'keyevent', '4')
                ui.tap('Liste complète des éléments', scroll=True)
                ui.capture('room-inventory-secondary')
                device('shell', 'input', 'keyevent', '4')
                ui.find('Liste complète des éléments', scroll=True)
                device('shell', 'input', 'keyevent', '4')
                ui.find('Choisissez une pièce')
                navigate('Accueil')
                ui.find('Relais synthétique')
            ui.capture('before-command')
            if scenario.unsent() or scenario is Scenario.LIGHT_SLIDER:
                help_text = device('shell', 'input', check=False)
                if 'motionevent' not in help_text.stdout + help_text.stderr:
                    raise RuntimeError('Owned API35 input must expose synchronous DOWN/UP')
                pressed = slider_point()
                touch(TouchPhase.DOWN, pressed)
                ui.capture('slider-held')
            if scenario.unsent():
                match scenario:
                    case Scenario.WRITE_WITHDRAWAL:
                        control(Control.DENY_WRITE)
                    case Scenario.CAPABILITY_WITHDRAWAL:
                        control(Control.WITHDRAW_LIGHT)
                    case Scenario.LAYOUT_CHANGE:
                        dashboard = directory / 'server-dashboard/dashboard.yaml'
                        temporary = dashboard.with_suffix('.writing')
                        temporary.write_text(dashboard.read_text().replace('Maison synthétique', 'Maison modifiée').replace('Lumière synthétique', 'Lampe remaniée'))
                        temporary.replace(dashboard)
                    case (Scenario.ACK_ONLY | Scenario.LOST_SUBMIT | Scenario.EXPIRE | Scenario.REFUSAL
                          | Scenario.RECONNECT | Scenario.SERVER_RESTART | Scenario.CONFIRM | Scenario.LIGHT_SLIDER):
                        raise AssertionError('Guard scenario expected')
                deadline = time.monotonic() + 20
                while True:
                    rows, _, _, _ = ui.labels()
                    labels = [ANDROID['normalized'](row['label']) for row in rows]
                    missing_old_card = ANDROID['normalized']('Lumière synthétique') not in labels
                    disabled = any(ANDROID['normalized']('Lecture seule') in label for label in labels)
                    power = card_button(rows, 'Lumière synthétique', 'Allumer')
                    details = card_button(rows, 'Lumière synthétique', 'Détails')
                    slider_gone = (power is not None and details is not None
                                   and details['y'] - power['y'] < pressed['gap'] - 15)
                    if missing_old_card or disabled or slider_gone:
                        break
                    if time.monotonic() >= deadline:
                        raise AssertionError('Held native slider did not lose its original capability/context')
                    time.sleep(.1)
                ui.capture('gesture-after-withdrawal')
                touch(TouchPhase.UP, pressed)
                pressed = None
                match scenario:
                    case Scenario.WRITE_WITHDRAWAL:
                        control(Control.ALLOW_WRITE)
                    case Scenario.CAPABILITY_WITHDRAWAL:
                        control(Control.REDISCOVER)
                    case Scenario.LAYOUT_CHANGE:
                        pass
                    case (Scenario.ACK_ONLY | Scenario.LOST_SUBMIT | Scenario.EXPIRE | Scenario.REFUSAL
                          | Scenario.RECONNECT | Scenario.SERVER_RESTART | Scenario.CONFIRM | Scenario.LIGHT_SLIDER):
                        raise AssertionError('Guard scenario expected')
                time.sleep(1)
                ui.capture('gesture-released-no-replay')
            else:
                if scenario is Scenario.LIGHT_SLIDER:
                    touch(TouchPhase.UP, pressed)
                    pressed = None
                else:
                    tap_card('Relais synthétique', 'Allumer')
                if scenario is Scenario.REFUSAL:
                    ui.find('Non admise', match=contains)
                    ui.find('La configuration a changé', match=contains)
                    ui.capture('refused-before-commit')
                else:
                    ui.find('confirmation en attente', match=contains)
                    ui.capture('ack-observation-unchanged')
                    match scenario:
                        case Scenario.EXPIRE:
                            ui.find('Résultat indéterminé', match=contains, timeout=40)
                            ui.capture('expired-after-dispatch')
                        case Scenario.RECONNECT:
                            control(Control.OUTAGE)
                            time.sleep(1.5)
                            ui.capture('connection-cut')
                            control(Control.RESTORE)
                            ui.find('confirmation en attente', match=contains)
                            ui.capture('reconnected-same-command')
                        case Scenario.SERVER_RESTART:
                            control(Control.RESTART)
                            ui.find('Suivi en direct indisponible', match=contains)
                            ui.capture('no-live-after-server-restart')
                        case Scenario.CONFIRM:
                            control(Control.CONFIRM)
                            ui.find('Effet confirmé', match=contains)
                            ui.find('Allumé')
                            ui.capture('correlated-confirmation')
                        case Scenario.ACK_ONLY | Scenario.LOST_SUBMIT | Scenario.LIGHT_SLIDER:
                            pass
                        case Scenario.REFUSAL | Scenario.WRITE_WITHDRAWAL | Scenario.CAPABILITY_WITHDRAWAL | Scenario.LAYOUT_CHANGE:
                            raise AssertionError('Dispatched scenario expected')
                    if scenario in (Scenario.ACK_ONLY, Scenario.LOST_SUBMIT, Scenario.RECONNECT, Scenario.LIGHT_SLIDER):
                        title = 'Lumière synthétique' if scenario is Scenario.LIGHT_SLIDER else 'Relais synthétique'
                        tap_card(title, 'Arrêter le suivi')
                        ui.find('Suivi arrêté', match=contains)
                        ui.capture('tracking-stopped')
            if scenario is Scenario.ACK_ONLY:
                for width, height in [(1024, 600), (1280, 800), (800, 1280)]:
                    ui.format(width, height, f'cards-light-{width}x{height}')
                ui.format(1280, 800, 'cards-before-preferences')
                navigate('Préférences')
                ui.tap('Thème sombre')
                ui.tap('Texte à 200', match=contains, scroll=True)
                navigate('Accueil')
                for width, height in [(1024, 600), (1280, 800), (800, 1280)]:
                    ui.format(width, height, f'cards-dark-200-{width}x{height}')
                ui.format(1280, 800, 'cards-before-restart')
                device('shell', 'input', 'keyevent', '4')
                deadline = time.monotonic() + 15
                while True:
                    activities = device('shell', 'dumpsys', 'activity', 'activities').stdout
                    resumed = [line for line in activities.splitlines()
                               if 'mResumedActivity:' in line or 'topResumedActivity=' in line]
                    if not resumed:
                        raise RuntimeError('Native foreground Activity metadata unavailable')
                    if not any(package in line for line in resumed):
                        break
                    if time.monotonic() >= deadline:
                        raise AssertionError('Back at home did not leave the owned Activity')
                    time.sleep(.1)
                assert len(records()) == 1, 'leaving the Activity preserves the session for restart'
                receipt['native_back_at_home_left_activity'] = True
            device('shell', 'am', 'force-stop', package)
            start()
            ui.find('Afficher ma maison', match=contains)
            ui.tap('Afficher ma maison', match=contains)
            ui.find('En un regard')
            if scenario is Scenario.CONFIRM:
                ui.find('Allumé')
            time.sleep(1)
            ui.capture('restart-no-replay')
            navigate('Préférences')
            ui.tap('Se déconnecter', scroll=True)
            ui.find('Se connecter')
            deadline = time.monotonic() + 8
            while records() and time.monotonic() < deadline:
                time.sleep(.1)
            assert not records()
            ui.capture('signed-out')
            receipt.update(status='AUTOMATED_CHECKS_PASS', keystore_record_absent_after_logout=True,
                           client_restart_without_replay=True, apk_sha256=digest(apk))
        except BaseException as error:
            receipt['failure'] = str(error)
            if installed:
                with contextlib.suppress(Exception):
                    ui.capture('failure')
            raise
        finally:
            if pressed is not None:
                with contextlib.suppress(Exception):
                    touch(TouchPhase.UP, pressed)
            write(directory / 'interaction-result.json', receipt)
            if installed:
                device('shell', 'am', 'force-stop', package, check=False)
            if forwarded:
                device('reverse', '--remove', f'tcp:{port}')
            device('shell', 'wm', 'size', 'reset', check=False)
            device('shell', 'wm', 'density', 'reset', check=False)
            for key, value in rotation.items():
                if value == 'null':
                    device('shell', 'settings', 'delete', 'system', key)
                else:
                    device('shell', 'settings', 'put', 'system', key, value)
            if installed:
                device('uninstall', package)
    receipt['server'] = validate_server(directory, scenario)
    write(directory / 'result.json', receipt)


def reset_closed_android(directory, vm):
    """Retire only this completed case's stopped, task-owned synthetic VM data.

    APKs, input captures and server receipts live outside the AVD and remain
    intact. Each next case boots the same API/ABI/tablet with fresh local data,
    keeping temporary Android disk growth inside the frontend storage budget.
    """
    report = json.loads((directory / 'result.json').read_text())
    assert report['status'] == 'AUTOMATED_CHECKS_PASS'
    assert report['keystore_record_absent_after_logout'] and report['server']['server_joined']
    identity = json.loads((vm / 'device.json').read_text())
    name = 'homeward-dev-api35'
    assert identity['serial'] == 'emulator-5558' and identity['avd'] == name
    assert identity['ro.build.version.sdk'] == '35' and identity['ro.product.cpu.abi'] == 'arm64-v8a'
    home = Path(SESSION['CONFIG']['frontend_cache']) / 'android-avd'
    assert Path(os.environ['ANDROID_AVD_HOME']).resolve() == home.resolve()
    avd, ini = home / (name + '.avd'), home / (name + '.ini')
    assert avd.is_dir() and not avd.is_symlink() and avd.resolve().parent == home.resolve()
    assert 'path=' + str(avd) + '\n' in ini.read_text()
    assert 'image.sysdir.1=system-images/android-35/google_apis/arm64-v8a/' in (avd / 'config.ini').read_text()
    closed = subprocess.run(['lsof', '-t', '+D', str(avd)], capture_output=True, timeout=30)
    assert closed.returncode == 1 and not closed.stdout
    shutil.copyfile(ini, vm / 'previous-avd.ini')
    shutil.copyfile(avd / 'config.ini', vm / 'previous-config.ini')
    before = sum(path.stat().st_blocks * 512 for path in avd.rglob('*') if path.is_file() and not path.is_symlink())
    run(['avdmanager', 'delete', 'avd', '--name', name])
    assert not avd.exists() and not ini.exists()
    run(['avdmanager', 'create', 'avd', '--name', name,
         '--package', 'system-images;android-35;google_apis;arm64-v8a',
         '--device', '10.1in WXGA (Tablet)'], input='no\n', text=True)
    after = sum(path.stat().st_blocks * 512 for path in avd.rglob('*') if path.is_file() and not path.is_symlink())
    write(vm / 'reset-result.json', dict(status='PASS', released_vm_bytes=before - after,
                                       stopped_owned_avd=str(avd), completed_native_case=str(directory),
                                       captures_and_apk_preserved=True, logout_proved_before_vm_reset=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', type=PlatformSelection, choices=list(PlatformSelection), default=PlatformSelection.DESKTOP)
    parser.add_argument('--scenario', type=Scenario, choices=list(Scenario), action='append')
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through scripts/frontend.py run --cargo.')
    platforms = args.platform.platforms()
    scenarios = args.scenario or list(Scenario)
    proof = Path(SESSION['CONFIG']['frontend_cache']) / 'action-ui' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    print('Native action cards proof: ' + str(proof), flush=True)
    server = proof / 'server.test'
    run(['go', 'test', '-c', '-race', '-o', server, './internal/server'])
    for platform in platforms:
        match platform:
            case Platform.DESKTOP:
                helper = proof / 'native-ui'
                modules = Path(SESSION['CONFIG']['frontend_cache']) / 'session-spike/swift-modules'
                modules.mkdir(parents=True, exist_ok=True)
                run(['xcrun', 'swiftc', '-module-cache-path', modules, ROOT / 'qualification/frontend/native-ui.swift', '-o', helper])
                run(['cargo', 'build', '--locked', '-p', 'homeward-app', '--bin', 'homeward-desktop'])
                bundle = proof / 'Homeward Actions.app/Contents'
                (bundle / 'MacOS').mkdir(parents=True)
                binary = bundle / 'MacOS/homeward-desktop'
                shutil.copy2(SESSION['TARGET'] / 'debug/homeward-desktop', binary)
                with (bundle / 'Info.plist').open('wb') as output:
                    plistlib.dump(dict(CFBundleExecutable=binary.name, CFBundleIdentifier=SESSION['PACKAGE'],
                                       CFBundleName='Homeward Actions', CFBundlePackageType='APPL', NSHighResolutionCapable=True), output)
                for scenario in scenarios:
                    desktop(proof / (platform.value + '-' + scenario.value), server, scenario, helper, binary)
            case Platform.ANDROID:
                apk, ocr = ANDROID['prepare_apk'](ROOT, proof, run)
                for scenario in scenarios:
                    vm = proof / ('emulator-' + scenario.value)
                    vm.mkdir(mode=0o700)
                    directory = proof / (platform.value + '-' + scenario.value)
                    with TRANSPORT['android'](vm):
                        android(directory, server, scenario, apk, ocr)
                    reset_closed_android(directory, vm)
    write(proof / 'result.json', dict(status='AUTOMATED_CHECKS_PASS', server_sha256=digest(server),
                                    platforms=[platform.value for platform in args.platform.platforms()],
                                    scenarios=[scenario.value for scenario in scenarios],
                                    synthetic_only=True, physical_device_qualified=False,
                                    captures_require_visual_inspection=True))


if __name__ == '__main__':
    main()
