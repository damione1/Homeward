"""Actual Android app, OS collectors/notifications and isolated Go-race TLS.

Only the owned API35 emulator receives input. No real provider is attached.
"""
from enum import Enum, IntEnum
import argparse
import contextlib
import hashlib
import json
import os
import re
from pathlib import Path
import runpy
import shlex
import shutil
import subprocess
import sys
import time
from urllib.parse import urlsplit
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'scripts'))
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
TRANSPORT = SESSION['TRANSPORT']
INPUT = runpy.run_path(str(ROOT / 'scripts/frontend-android-input.py'))
OS = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-android-notifications.py'))
CONFIG = json.loads((ROOT / 'frontend.local.json').read_text())
CACHE = Path(CONFIG['frontend_cache']) / 'nhf023'
PACKAGE = SESSION['PACKAGE']
DEVICE = TRANSPORT['device']

class Control(Enum):
    OBSERVE = 'capture-state'
    SEED_NOMINAL = 'seed-nominal'
    SEED_ENTITY = 'seed-entity'

class Platform(IntEnum):
    MAC_OS = 1
    ANDROID = 2

class Source(IntEnum):
    ANDROID_BATTERY_MANAGER = 5
    ANDROID_CONNECTIVITY_MANAGER = 6

class Presentation(IntEnum):
    NOT_ATTEMPTED = 1
    OS_ACCEPTED = 2
    PRESENT_IN_OS_CENTER = 3
    OUTCOME_UNKNOWN = 4
    PERMISSION_DENIED = 5
    UNAVAILABLE = 6


class FailurePolicy(Enum):
    STOP = 'stop'
    INSPECT = 'inspect'

    def __str__(self):
        return self.value


def qualify(policy=FailurePolicy.STOP):
    proof = CACHE / ('android-owner-' + str(time.time_ns()))
    proof.mkdir(mode=0o700)
    shutil.copy2(__file__, proof / 'driver.py')
    apk = proof / 'Homeward.apk'
    shutil.copy2(Path(os.environ['CARGO_TARGET_DIR']) / 'debug/apk/homeward-dev.apk', apk)
    binary = proof / 'server.test'
    shutil.copy2(CACHE / 'android-companion-notifications-server.test', binary)
    fixture = proof / 'fixture'
    server = None
    installed = forwarded = False
    ui = None
    sequence = 0
    (proof / 'artifacts.json').write_text(json.dumps(dict(apk_sha256=SHARED['sha'](apk), server_sha256=SHARED['sha'](binary)), indent=2) + '\n')

    def control(value):
        if not isinstance(value, Control):
            raise TypeError('Control enum required')
        done = fixture / (value.value + '-done.json')
        done.unlink(missing_ok=True)
        if value is Control.OBSERVE:
            (fixture / 'observed-state.json').unlink(missing_ok=True)
        (fixture / value.value).touch(mode=0o600)
        SHARED['wait_file'](done, server, timeout=10)
        if value is Control.OBSERVE:
            return json.loads((fixture / 'observed-state.json').read_text())

    def state(name, predicate, timeout=25):
        deadline = time.monotonic() + timeout
        while True:
            value = control(Control.OBSERVE)
            if predicate(value):
                (proof / (name + '.json')).write_text(json.dumps(value, indent=2) + '\n')
                print('STATE_PASS ' + name, flush=True)
                return value
            if time.monotonic() >= deadline:
                (proof / (name + '-failed.json')).write_text(json.dumps(value, indent=2) + '\n')
                raise RuntimeError('Actual server state missing: ' + name)
            time.sleep(.25)

    def top():
        nonlocal sequence
        sequence += 1
        _, width, height = ui.capture('outer-scroll-anchor-' + str(sequence))
        # On this declared 1280x800 / 160dpi Fluent tablet surface, the outer
        # scrollbar is inside the 16px shell margin. Drag its track to the top;
        # dragging a nested table or the fixed heading cannot move this view.
        if (width, height) != (1280, 800):
            raise RuntimeError('Unexpected qualified tablet geometry')
        DEVICE('shell', 'input', 'touchscreen', 'swipe', width - 22, height * 7 // 8,
               width - 22, height // 3, '350')
        time.sleep(.25)

    def tap(label, scroll=True):
        deadline = time.monotonic() + 35
        while True:
            row, width, height, capture = ui.find(label, scroll=scroll, timeout=max(.1, deadline - time.monotonic()))
            x = round(row['x'] + row['width'] / 2)
            y = round(row['y'] + row['height'] / 2)
            if not scroll or row['y'] + row['height'] < height - 64:
                break
            if time.monotonic() >= deadline:
                raise RuntimeError('Observed Android label remains clipped at viewport edge: ' + label)
            # OCR can recognize the first pixels of a clipped button. Move it
            # inside the viewport before tapping; preserve every actual gesture.
            DEVICE('shell', 'input', 'swipe', width * 3 // 4, height * 4 // 5,
                   width * 3 // 4, height * 4 // 5 - 96, '700')
            time.sleep(.35)
        if not (0 <= x < width and 0 <= y < height):
            raise RuntimeError('Input outside observed emulator')
        capture.with_suffix('.tap.json').write_text(json.dumps(dict(requested=label, observed=row, x=x, y=y), indent=2) + '\n')
        DEVICE('shell', 'input', 'tap', x, y)
        time.sleep(.25)

    def settings():
        rows, _, _, _ = ui.labels()
        if not any(INPUT['normalized'](row['label']) == INPUT['normalized']('Préférences') for row in rows):
            ui.navigation()
        tap('Préférences', scroll=False)

    def text(value):
        DEVICE('shell', 'input', 'text', shlex.quote(value.replace(' ', '%s')))

    try:
        with (proof / 'server.log').open('wb') as log:
            server = subprocess.Popen([str(binary), '-test.run=^TestNHF022NotificationServer$', '-test.v', '-test.timeout=11m'],
                cwd=ROOT / 'internal/server', env=dict(os.environ, HOMEWARD_NHF022_FIXTURE=str(fixture), HOMEWARD_NHF022_SYNTHETIC_ONLY='1'),
                stdout=log, stderr=subprocess.STDOUT)
            private = SHARED['wait_file'](fixture / 'ready.json', server, timeout=120)
            origin = urlsplit(private['origin'])
            if not (private['mode'] == 'synthetic-only' and private['production_configuration'] is False and private['no_physical_providers'] is True
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Only the fresh isolated loopback server is allowed')
            with TRANSPORT['android'](proof):
                ui = INPUT['AndroidInput'](proof, CACHE / 'android-native-ocr', TRANSPORT['ADB'], TRANSPORT['SERIAL'], DEVICE)
                try:
                    existing = DEVICE('shell', 'pm', 'path', PACKAGE, check=False)
                    if 'package:' in existing.stdout:
                        raise RuntimeError('Preserve existing emulator app')
                    DEVICE('install', '--no-streaming', apk)
                    installed = True
                    if DEVICE('shell', 'pm', 'clear', PACKAGE).stdout.strip() != 'Success':
                        raise RuntimeError('Fresh owned app not reset')
                    DEVICE('shell', 'pm', 'revoke', PACKAGE, 'android.permission.POST_NOTIFICATIONS', check=False)
                    DEVICE('shell', 'pm', 'clear-permission-flags', PACKAGE, 'android.permission.POST_NOTIFICATIONS', 'user-set', 'user-fixed')
                    DEVICE('reverse', 'tcp:' + str(origin.port), 'tcp:' + str(origin.port))
                    forwarded = True
                    DEVICE('shell', 'run-as', PACKAGE, 'mkdir', '-p', 'files')
                    subprocess.run([str(TRANSPORT['ADB']), '-s', TRANSPORT['SERIAL'], 'shell', 'run-as', PACKAGE,
                        'sh', '-c', shlex.quote('cat > files/endpoint.json')],
                        input=json.dumps(SESSION['public_profile'](private)), text=True, capture_output=True, check=True)
                    DEVICE('logcat', '-c')
                    DEVICE('shell', 'am', 'start', '-W', '-n', PACKAGE + '/' + SESSION['ACTIVITY'])
                    ui.find('Homeward', timeout=45)
                    ui.tap('Identifiant', offset_y=40)
                    text(private['user']['login_name'])
                    DEVICE('shell', 'input', 'keyevent', '61')
                    text(private['user']['password'])
                    DEVICE('shell', 'input', 'keyevent', '66')
                    ui.find('Session sécurisée active', match=INPUT['LabelMatch'].CONTAINS, timeout=30)
                    ui.capture('authenticated')
                    settings()
                    tap('Inscrire ce compagnon')
                    tap('Confirmer')
                    registered = state('companion-default-off', lambda s: len(s['installations']) == 1)
                    record = registered['installations'][0]
                    if Platform(record['device']['platform']) is not Platform.ANDROID:
                        raise RuntimeError('Native Android identity not used')
                    expected_model = DEVICE('shell', 'getprop', 'ro.product.model').stdout.strip()
                    if record['device']['model'] != expected_model:
                        raise RuntimeError('Actual Android model differs from registered model')
                    if any(record.get('consent', {}).get(key, False) for key in ('battery', 'connectivity', 'activity')) or registered['companion_counts']['publications'] != 0:
                        raise RuntimeError('Collection occurred without consent')
                    ui.capture('companion-registered')
                    top()
                    tap('Modifier le partage')
                    tap('Batterie : partager')
                    tap('Réseau local : partager')
                    tap('Visibilité de l’application : partager')
                    tap('Confirmer')
                    sampled = state('actual-android-sample', lambda s: s['companion_counts']['publications'] >= 1, timeout=40)
                    latest = sampled['installations'][0].get('latest', {})
                    if not all('value' in latest.get(key, {}) for key in ('battery', 'connectivity', 'activity')):
                        raise RuntimeError('Actual Android value missing despite known emulator battery/route/foreground')
                    battery = DEVICE('shell', 'dumpsys', 'battery').stdout
                    (proof / 'independent-os-battery.txt').write_text(battery)
                    level = int(re.search(r'^\s*level: (\d+)$', battery, re.M).group(1))
                    scale = int(re.search(r'^\s*scale: (\d+)$', battery, re.M).group(1))
                    if latest['battery']['value']['percent'] != (level * 100 + scale // 2) // scale:
                        raise RuntimeError('Rust published battery differs from independent OS measurement')
                    top()
                    tap('Inscrire cette installation')
                    tap('Confirmer')
                    state('notification-default-off', lambda s: len(s['destinations']) == 1 and not s['destinations'][0].get('native_delivery', False))
                    top()
                    tap('Configurer les notifications Android')
                    tap('Confirmer')
                    deadline = time.monotonic() + 15
                    allowed = False
                    while not allowed and time.monotonic() < deadline:
                        allowed = OS['allow_permission'](proof)
                        time.sleep(.15)
                    if not allowed:
                        raise RuntimeError('Actual Android OS permission prompt missing')
                    time.sleep(1)
                    # The permission Activity can suspend Homeward. Its privacy
                    # reset returns to Home; reopen settings through navigation.
                    ui.capture('after-os-permission')
                    settings()
                    top()
                    tap('Activer les notifications natives')
                    tap('Confirmer')
                    state('native-enabled', lambda s: s['destinations'][0].get('native_delivery', False))
                    control(Control.SEED_NOMINAL)
                    delivered = state('native-os-accepted', lambda s: any(e['title'] == 'Notification synthétique' and Presentation(e['presentation']) is Presentation.OS_ACCEPTED for e in s['deliveries']))
                    message = next(e for e in delivered['deliveries'] if e['title'] == 'Notification synthétique')
                    tag = 'homeward.' + message['id']['value']
                    # getActiveNotifications includes queued/snoozed records.
                    # The owned-emulator diagnostic must observe the posted list,
                    # not merely find the tag anywhere in a dumpsys archive.
                    deadline = time.monotonic() + 12
                    while True:
                        center = DEVICE('shell', 'dumpsys', 'notification', '--noredact').stdout
                        body = center.partition('  Notification List:\n')[2]
                        posted = re.split(r'(?m)^  (?=\S)', body, maxsplit=1)[0]
                        if tag in posted:
                            break
                        if time.monotonic() >= deadline:
                            (proof / 'native-notification-unposted.txt').write_text(center)
                            raise RuntimeError('OS accepted notification did not enter posted list')
                        time.sleep(.15)
                    (proof / 'native-notification-posted.txt').write_text(center)
                    DEVICE('shell', 'cmd', 'statusbar', 'expand-notifications')
                    deadline = time.monotonic() + 12
                    while True:
                        DEVICE('shell', 'uiautomator', 'dump', '/sdcard/homeward-notification.xml', check=False)
                        result = DEVICE('shell', 'cat', '/sdcard/homeward-notification.xml', check=False)
                        try:
                            tree = ET.fromstring(result.stdout)
                        except ET.ParseError:
                            tree = None
                        node = None if tree is None else next((n for n in tree.iter('node') if n.get('package') == 'com.android.systemui' and n.get('text') == message['title']), None)
                        if node is not None:
                            break
                        if time.monotonic() >= deadline:
                            ui.capture('native-notification-not-visible')
                            (proof / 'native-notification-not-visible.xml').write_text(result.stdout)
                            raise RuntimeError('Posted message absent from actual OS notification view')
                        time.sleep(.15)
                    (proof / 'native-notification.xml').write_text(result.stdout)
                    ui.capture('actual-native-notification')
                    bounds = list(map(int, re.findall(r'\d+', node.get('bounds', ''))))
                    if len(bounds) != 4:
                        raise RuntimeError('Invalid observed OS notification bounds')
                    point = ((bounds[0] + bounds[2]) // 2, (bounds[1] + bounds[3]) // 2)
                    if not (0 <= point[0] < 1280 and 0 <= point[1] < 800):
                        raise RuntimeError('Observed notification outside owned display')
                    DEVICE('shell', 'input', 'tap', *point)
                    (proof / 'native-notification-click.json').write_text(json.dumps(dict(title=message['title'], bounds=bounds, point=point), indent=2) + '\n')
                    state('native-click-authorized', lambda s: s['counts']['opens'] == 1)
                    deadline = time.monotonic() + 10
                    while True:
                        log = DEVICE('logcat', '-d').stdout
                        if 'homeward: notification navigation authorized' in log:
                            (proof / 'native-grant-dispatch.json').write_text(json.dumps(dict(debug_marker_observed=True, fresh_ui_grant_consumed=True)) + '\n')
                            break
                        if time.monotonic() >= deadline:
                            raise RuntimeError('Fresh native navigation grant did not reach the UI')
                        time.sleep(.15)
                    settings()
                    top()
                    tap('Arrêter la présentation')
                    control(Control.SEED_ENTITY)
                    state('local-stop-no-new-presentation', lambda s: any(e['title'] == 'Ressource synthétique' and Presentation(e['presentation']) is Presentation.NOT_ATTEMPTED and e['reception'] == 2 for e in s['deliveries']))
                    top()
                    tap('Révoquer la destination')
                    tap('Confirmer')
                    state('destination-revoked', lambda s: s['destinations'][0]['state'] == 2)
                    top()
                    tap('Se déconnecter')
                    ui.find('Se connecter', timeout=20)
                    ui.capture('signed-out')
                    (proof / 'logcat.txt').write_text(DEVICE('logcat', '-d').stdout)
                    print('ANDROID_OWNER_FLOW_PASS', flush=True)
                except BaseException:
                    if installed:
                        with contextlib.suppress(Exception):
                            ui.capture('failure')
                            (proof / 'logcat.txt').write_text(DEVICE('logcat', '-d').stdout)
                    if policy is FailurePolicy.INSPECT:
                        print('HELD_FOR_INSPECTION ' + str(proof), flush=True)
                        deadline = time.monotonic() + 120
                        while time.monotonic() < deadline and not (proof / 'end-inspection').exists():
                            time.sleep(.2)
                    raise
                finally:
                    if installed:
                        DEVICE('shell', 'am', 'force-stop', PACKAGE, check=False)
                        DEVICE('uninstall', PACKAGE, check=False)
                    if forwarded:
                        DEVICE('reverse', '--remove', 'tcp:' + str(origin.port), check=False)
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30):
                raise RuntimeError('Isolated Go Run did not join')
            isolation = json.loads((fixture / 'result.json').read_text())
            if not (isolation['actual_run_joined'] and isolation['no_physical_providers'] and isolation['configuration_unchanged'] and not isolation['production_configuration']):
                raise RuntimeError('Isolation failed')
            result = dict(status='PASS_PENDING_VISUAL_INSPECTION', actual_rust_go_tls=True, native_android_collectors=True,
                actual_os_permission=True, os_submission_receipt=True, native_center_visibility_qualified=True, default_consents_off=True,
                local_stop_and_revocation=True, physical_device=False, native_os_click_qualified=True,
                background_qualified=False, isolation=isolation, children_joined=True)
            (proof / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
            print('PASS ' + str(proof), flush=True)
    finally:
        if server is not None and server.poll() is None:
            (fixture / 'stop').touch(mode=0o600)
            with contextlib.suppress(subprocess.TimeoutExpired):
                server.wait(timeout=30)
        SHARED['stop'](server)

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--failure-policy', type=FailurePolicy, choices=list(FailurePolicy), default=FailurePolicy.STOP)
    qualify(parser.parse_args().failure_policy)
