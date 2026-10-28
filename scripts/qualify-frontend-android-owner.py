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

class Reception(IntEnum):
    NOT_RECEIVED = 1
    RECEIVED = 2

class Permission(IntEnum):
    NOT_REQUESTED = 1
    DENIED = 2
    AUTHORIZED = 3
    PROVISIONAL = 4
    EPHEMERAL = 5
    UNKNOWN = 6

class Cadence(IntEnum):
    SECONDS_30 = 1
    MINUTE_1 = 2
    MINUTES_5 = 3

    def seconds(self):
        match self:
            case Cadence.SECONDS_30:
                return 30
            case Cadence.MINUTE_1:
                return 60
            case Cadence.MINUTES_5:
                return 300
        raise AssertionError('Unhandled companion cadence')

class Mode(Enum):
    FOREGROUND = 'foreground'
    BACKGROUND = 'background'
    BACKGROUND_REFRESH = 'background-refresh'
    BACKGROUND_PERMISSION = 'background-permission'

    def __str__(self):
        return self.value

    def background(self):
        match self:
            case Mode.FOREGROUND:
                return False
            case Mode.BACKGROUND | Mode.BACKGROUND_REFRESH | Mode.BACKGROUND_PERMISSION:
                return True
        raise AssertionError('Unhandled reception qualification mode')


class FailurePolicy(Enum):
    STOP = 'stop'
    INSPECT = 'inspect'

    def __str__(self):
        return self.value


def qualify(policy=FailurePolicy.STOP, mode=Mode.FOREGROUND):
    if not isinstance(mode, Mode):
        raise TypeError('Qualification mode enum required')
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
    (proof / 'mode.json').write_text(json.dumps(dict(mode=mode.value)) + '\n')

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

    def service(expected, name):
        deadline = time.monotonic() + 10
        while True:
            value = DEVICE('shell', 'dumpsys', 'activity', 'services', PACKAGE).stdout
            actual = 'HomewardReceptionService' in value and 'isForeground=true' in value
            if actual is expected and (expected or 'HomewardReceptionService' not in value):
                (proof / (name + '.txt')).write_text(value)
                return
            if time.monotonic() >= deadline:
                (proof / (name + '-failed.txt')).write_text(value)
                raise RuntimeError('Actual OS foreground service state missing: ' + name)
            time.sleep(.15)

    def stop_service_from_os():
        # HOME is asynchronous. Wait for that transition before asking SystemUI
        # to expand, otherwise the pending HOME can collapse the new shade.
        time.sleep(.5)
        DEVICE('shell', 'cmd', 'statusbar', 'expand-notifications')
        deadline = time.monotonic() + 10
        expanded = False
        while True:
            DEVICE('shell', 'uiautomator', 'dump', '/sdcard/homeward-reception.xml', check=False)
            raw = DEVICE('shell', 'cat', '/sdcard/homeward-reception.xml', check=False).stdout
            try:
                tree = ET.fromstring(raw)
            except ET.ParseError:
                tree = None
            nodes = [] if tree is None else list(tree.iter('node'))
            button = next((n for n in nodes if INPUT['normalized'](n.get('text', '')) == INPUT['normalized']('Arrêter l’arrière-plan') and n.get('package') == 'com.android.systemui'), None)
            if button is not None:
                bounds = list(map(int, re.findall(r'\d+', button.get('bounds', ''))))
                if len(bounds) != 4:
                    raise RuntimeError('Invalid OS stop action bounds')
                point = [(bounds[0] + bounds[2]) // 2, (bounds[1] + bounds[3]) // 2]
                if not (0 <= point[0] < 1280 and 0 <= point[1] < 800):
                    raise RuntimeError('OS stop action outside owned display')
                (proof / 'background-os-stop.xml').write_text(raw)
                ui.capture('background-os-stop')
                DEVICE('shell', 'input', 'tap', *point)
                (proof / 'background-os-stop.json').write_text(json.dumps(dict(bounds=bounds, point=point)) + '\n')
                return
            title = next((n for n in nodes if n.get('text') == 'Réception Homeward active' and n.get('package') == 'com.android.systemui'), None)
            if title is None:
                DEVICE('shell', 'cmd', 'statusbar', 'expand-notifications')
            if not expanded and title is not None:
                parents = {child: parent for parent in tree.iter() for child in parent}
                ancestor, node = title, None
                while ancestor in parents and node is None:
                    ancestor = parents[ancestor]
                    node = next((n for n in ancestor.iter('node') if n.get('resource-id') == 'android:id/expand_button' and n.get('content-desc') == 'Expand'), None)
                if node is not None:
                    bounds = list(map(int, re.findall(r'\d+', node.get('bounds', ''))))
                    if len(bounds) == 4:
                        DEVICE('shell', 'input', 'tap', (bounds[0] + bounds[2]) // 2, (bounds[1] + bounds[3]) // 2)
                        expanded = True
            if time.monotonic() >= deadline:
                (proof / 'background-os-stop-failed.xml').write_text(raw)
                ui.capture('background-os-stop-failed')
                raise RuntimeError('Actual SystemUI stop action missing')
            time.sleep(.15)

    def withdraw_permission():
        registered = control(Control.OBSERVE)
        before = DEVICE('shell', 'pidof', PACKAGE, check=False).stdout.strip()
        DEVICE('shell', 'pm', 'revoke', PACKAGE, 'android.permission.POST_NOTIFICATIONS')
        package = DEVICE('shell', 'dumpsys', 'package', PACKAGE).stdout
        (proof / 'permission-revoked-package.txt').write_text(package)
        if not re.search(r'android\.permission\.POST_NOTIFICATIONS:\s+granted=false', package):
            raise RuntimeError('Actual Android notification permission not revoked')
        service(False, 'service-stopped-after-permission-withdrawal')
        after = DEVICE('shell', 'pidof', PACKAGE, check=False).stdout.strip()
        (proof / 'permission-withdrawal-process.json').write_text(json.dumps(dict(pid_before=before, pid_after=after, process_survived=bool(after) and before == after), indent=2) + '\n')
        prior = {entry['id']['value'] for entry in control(Control.OBSERVE)['deliveries']}
        control(Control.SEED_ENTITY)
        time.sleep(6)
        untouched = state('permission-withdrawal-no-reception', lambda s: any(e['id']['value'] not in prior and Reception(e['reception']) is Reception.NOT_RECEIVED and Presentation(e['presentation']) is Presentation.NOT_ATTEMPTED for e in s['deliveries']))
        new_ids = {entry['id']['value'] for entry in untouched['deliveries']} - prior
        center = DEVICE('shell', 'dumpsys', 'notification', '--noredact').stdout
        posted = re.split(r'(?m)^  (?=\S)', center.partition('  Notification List:\n')[2], maxsplit=1)[0]
        if any('homeward.' + value in posted for value in new_ids):
            raise RuntimeError('A new message was posted after permission withdrawal')
        (proof / 'permission-withdrawal-notification-manager.txt').write_text(center)
        service(False, 'permission-withdrawal-no-service-restart')
        DEVICE('shell', 'cmd', 'statusbar', 'collapse')
        DEVICE('shell', 'am', 'start', '-W', '-n', PACKAGE + '/' + SESSION['ACTIVITY'])
        ui.find('Session sécurisée active', match=INPUT['LabelMatch'].CONTAINS, timeout=45)
        service(False, 'permission-withdrawal-foreground-no-service')
        settings()
        ui.find('Collecte arrêtée localement', scroll=True, timeout=30)
        recovered = state('permission-withdrawal-registration-restored', lambda s:
            len(s['installations']) == 1 and len(s['destinations']) == 1
            and s['installations'][0]['id'] == registered['installations'][0]['id']
            and s['destinations'][0]['id'] == registered['destinations'][0]['id']
            and s['counts']['registrations'] == registered['counts']['registrations']
            and s['companion_counts']['registrations'] == registered['companion_counts']['registrations']
            and Permission(s['destinations'][0]['permission']['permission']) is Permission.DENIED
            and not s['destinations'][0]['native_delivery'])
        ui.capture('permission-withdrawal-foreground')
        hold = Cadence(recovered['installations'][0]['consent']['cadence']).seconds() + 5
        time.sleep(hold)
        state('permission-withdrawal-no-automatic-telemetry', lambda s:
            s['companion_counts']['publications'] == registered['companion_counts']['publications']
            and s['counts']['registrations'] == registered['counts']['registrations']
            and s['companion_counts']['registrations'] == registered['companion_counts']['registrations'])
        service(False, 'permission-withdrawal-restored-stays-stopped')

    try:
        with (proof / 'server.log').open('wb') as log:
            server = subprocess.Popen([str(binary), '-test.run=^TestNHF022NotificationServer$', '-test.v', '-test.timeout=11m'],
                cwd=ROOT / 'internal/server', env=dict(os.environ, HOMEWARD_NHF022_FIXTURE=str(fixture), HOMEWARD_NHF022_SYNTHETIC_ONLY='1', HOMEWARD_NHF023_BACKGROUND_REFRESH='1' if mode is Mode.BACKGROUND_REFRESH else '0'),
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
                    if mode.background():
                        top()
                        tap('Recevoir en arrière-plan')
                        tap('Confirmer')
                        service(True, 'background-service-running')
                        DEVICE('shell', 'input', 'keyevent', 'KEYCODE_HOME')
                        time.sleep(.5)
                        activities = DEVICE('shell', 'dumpsys', 'activity', 'activities').stdout
                        if re.search(r'mResumedActivity:.*' + re.escape(PACKAGE), activities):
                            raise RuntimeError('Homeward remained the resumed Activity')
                        (proof / 'background-activity-hidden.txt').write_text(activities)
                        before_background = control(Control.OBSERVE)
                        background_started = time.monotonic()
                        cadence = Cadence(before_background['installations'][0]['consent']['cadence'])
                        if mode is Mode.BACKGROUND_REFRESH and (fixture / 'background-refresh-observed.json').exists():
                            raise RuntimeError('Credentials refreshed before hiding the Activity; no background refresh proof')
                        # Observe at least the actual selected cadence; default
                        # UI registration uses60s, not the protocol minimum30s.
                        while time.monotonic() - background_started < cadence.seconds() + 5:
                            time.sleep(.25)
                        if mode is Mode.BACKGROUND_REFRESH:
                            SHARED['wait_file'](fixture / 'background-refresh-observed.json', server, timeout=210)
                            refreshed = json.loads((fixture / 'background-refresh-observed.json').read_text())
                            if refreshed['native_refresh_http_200'] != 1:
                                raise RuntimeError('Unexpected native refresh call count')
                            (proof / 'background-native-refresh.json').write_text(json.dumps(refreshed) + '\n')
                        after_background = control(Control.OBSERVE)
                        if after_background['companion_counts']['publications'] != before_background['companion_counts']['publications']:
                            raise RuntimeError('Companion collected while Activity was suspended')
                        service(True, 'background-service-after-cadence')
                        (proof / 'background-telemetry-stopped.json').write_text(json.dumps(dict(before=before_background['companion_counts'], after=after_background['companion_counts'], cadence_seconds=cadence.seconds(), observed_seconds=time.monotonic() - background_started), indent=2) + '\n')
                    seeded_at = time.monotonic()
                    control(Control.SEED_NOMINAL)
                    delivered = state('native-os-accepted', lambda s: any(e['title'] == 'Notification synthétique' and Presentation(e['presentation']) is Presentation.OS_ACCEPTED for e in s['deliveries']))
                    message = next(e for e in delivered['deliveries'] if e['title'] == 'Notification synthétique')
                    if mode.background():
                        (proof / 'background-reception.json').write_text(json.dumps(dict(synthetic_seed_to_os_receipt_seconds=time.monotonic() - seeded_at, activity_hidden=True, foreground_service=True, hardware_budget=False), indent=2) + '\n')
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
                    if mode.background():
                        DEVICE('shell', 'input', 'keyevent', 'KEYCODE_HOME')
                        service(True, 'background-service-before-os-stop')
                        if mode is Mode.BACKGROUND_PERMISSION:
                            time.sleep(.5)
                            withdraw_permission()
                        else:
                            stop_service_from_os()
                            service(False, 'background-service-stopped-by-os-action')
                            control(Control.SEED_ENTITY)
                            time.sleep(6)
                            state('background-stop-no-reception', lambda s: any(e['title'] == 'Ressource synthétique' and Reception(e['reception']) is Reception.NOT_RECEIVED and Presentation(e['presentation']) is Presentation.NOT_ATTEMPTED for e in s['deliveries']))
                            service(False, 'background-service-not-restarted')
                            DEVICE('shell', 'cmd', 'statusbar', 'collapse')
                            DEVICE('shell', 'am', 'start', '-W', '-n', PACKAGE + '/' + SESSION['ACTIVITY'])
                            state('foreground-reception-resumes-without-service', lambda s: any(e['title'] == 'Ressource synthétique' and Reception(e['reception']) is Reception.RECEIVED for e in s['deliveries']))
                            service(False, 'foreground-does-not-restart-service')
                    settings()
                    if mode is not Mode.BACKGROUND_PERMISSION:
                        top()
                        tap('Arrêter la présentation')
                        prior_ids = {e['id']['value'] for e in control(Control.OBSERVE)['deliveries']}
                        control(Control.SEED_ENTITY)
                        state('local-stop-no-new-presentation', lambda s: any(e['id']['value'] not in prior_ids and e['title'] == 'Ressource synthétique' and Presentation(e['presentation']) is Presentation.NOT_ATTEMPTED and Reception(e['reception']) is Reception.RECEIVED for e in s['deliveries']))
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
                local_stop_and_revocation=mode is not Mode.BACKGROUND_PERMISSION,
                actual_android_permission_withdrawal=mode is Mode.BACKGROUND_PERMISSION,
                registration_restored_after_process_restart=mode is Mode.BACKGROUND_PERMISSION,
                destination_revocation=True,
                physical_device=False, native_os_click_qualified=True,
                background_qualified=mode.background(), native_background_refresh_qualified=mode is Mode.BACKGROUND_REFRESH,
                background_scope='Owned API35 emulator, live process, user-started foreground service; no persistent delivery/Doze/physical hardware claim' if mode.background() else None,
                isolation=isolation, children_joined=True)
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
    parser.add_argument('--mode', type=Mode, choices=list(Mode), default=Mode.FOREGROUND)
    args = parser.parse_args()
    qualify(args.failure_policy, args.mode)
