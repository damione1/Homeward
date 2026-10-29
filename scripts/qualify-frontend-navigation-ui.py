#!/usr/bin/env python3
"""Real Homeward navigation UI against the opt-in typed synthetic TLS profile.

Run through frontend.py run --cargo. All input addresses only the owned app or
task emulator; no real integration driver is attached. Inspect captures before
using the automated interaction receipt as visual acceptance evidence.
"""
import argparse
from enum import Enum
import json
import os
from pathlib import Path
import plistlib
import re
import runpy
import shlex
import shutil
import subprocess
import time
from urllib.parse import urlsplit
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
session = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
PlatformSelection = session['PlatformSelection']
Platform = session['Platform']
UIProfile = session['UIProfile']
run = session['run']
digest = session['digest']
write = session['write']
TARGET = session['TARGET']
android_input = runpy.run_path(str(ROOT / 'scripts/frontend-android-input.py'))


class Command(Enum):
    INFO = 'info'
    FILL_LOGIN = 'fill-login'
    TAP = 'tap'
    RESIZE = 'resize'
    KEY = 'key'
    SCROLL = 'scroll'
    TREE = 'tree'
    CLOSE = 'close'
    SIGN_OUT = 'sign-out'


class NativeKey(Enum):
    TAB = 'tab'
    BACKTAB = 'backtab'
    UP = 'up'
    DOWN = 'down'
    HOME = 'home'
    END = 'end'
    ENTER = 'enter'
    ESCAPE = 'escape'
    SPACE = 'space'
    LETTER_K = 'letter-k'
    LETTER_I = 'letter-i'
    LETTER_T = 'letter-t'
    LETTER_C = 'letter-c'
    LETTER_H = 'letter-h'
    LETTER_E = 'letter-e'
    LETTER_N = 'letter-n'


class FixtureAction(Enum):
    DENY = 'deny'
    ALLOW = 'allow'


class RoomCase(Enum):
    LIVING = 'Salon'
    KITCHEN = 'Cuisine'
    BEDROOM = 'Chambre principale'
    OUTDOOR = 'Extérieur'


def desktop(proof, server):
    directory = proof / 'desktop'
    helper = proof / 'native-ui'
    modules = Path(session['CONFIG']['frontend_cache']) / 'session-spike/swift-modules'
    modules.mkdir(parents=True, exist_ok=True)
    run(['xcrun', 'swiftc', '-module-cache-path', modules, ROOT / 'qualification/frontend/native-ui.swift', '-o', helper])
    run(['cargo', 'build', '--locked', '-p', 'homeward-app', '--bin', 'homeward-desktop'])
    data = Path.home() / 'Library/Application Support/Homeward'
    public, preferences = data / 'endpoint.json', data / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve an existing product profile/preferences')
    with session['fixture'](directory, server, kind=session['FixtureKind'].CACHE, profile=UIProfile.HOME) as (private, control):
        namespace = session['namespace'](private)
        def present():
            result = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, timeout=10)
            if result.returncode not in (0, 44):
                raise RuntimeError('Keychain metadata unavailable')
            return result.returncode == 0
        if present():
            raise RuntimeError('Preserve an existing synthetic credential namespace')
        data.mkdir(parents=True, exist_ok=True)
        write(public, session['public_profile'](private))
        bundle = directory / 'Homeward Navigation.app/Contents'
        (bundle / 'MacOS').mkdir(parents=True)
        binary = bundle / 'MacOS/homeward-desktop'
        shutil.copyfile(TARGET / 'debug/homeward-desktop', binary)
        binary.chmod(0o755)
        with (bundle / 'Info.plist').open('wb') as output:
            plistlib.dump({'CFBundleExecutable': binary.name, 'CFBundleIdentifier': session['PACKAGE'], 'CFBundleName': 'Homeward Navigation', 'CFBundlePackageType': 'APPL', 'NSHighResolutionCapable': True}, output)
        child = None
        receipt = {'status': 'FAIL', 'fixture_profile': UIProfile.HOME.value, 'entity_count': 851, 'captures_require_visual_inspection': True}
        def native(command, **fields):
            result = subprocess.run([str(helper), '-'], input=json.dumps({'pid': child.pid, 'command': command.value, 'syntheticOnly': True, **fields}), capture_output=True, text=True, timeout=10)
            if result.returncode:
                raise AssertionError('Owned native action failed: ' + command.value)
            return json.loads(result.stdout) if result.stdout.strip() else None
        def tree():
            return native(Command.TREE)
        def has(label):
            return any(label in (row['title'], row['description'], row['text']) for row in tree())
        def wait_label(label):
            deadline = time.monotonic() + 8
            while not has(label) and time.monotonic() < deadline:
                assert child.poll() is None
                time.sleep(.1)
            assert has(label), 'Missing native label: ' + label
        def search_ready():
            rows = tree()
            aliases = {value for row in rows for value in [row['title'], row['text']] if re.fullmatch(r'[a-z_]+\.[a-z0-9_]+', value)}
            return any(row['role'] == 'AXTable' for row in rows) and bool(aliases) and all('kitchen' in alias for alias in aliases)
        def capture(name):
            info = native(Command.INFO)
            time.sleep(.25)
            run(['screencapture', '-x', '-o', '-l', str(info['window_id']), directory / (name + '.png')], capture_output=True)
            write(directory / (name + '.tree.json'), tree())
            return info
        def start(name):
            nonlocal child
            with (directory / (name + '.log')).open('wb') as output:
                child = subprocess.Popen([str(binary)], stdout=output, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 12
            while True:
                try:
                    native(Command.INFO)
                    break
                except AssertionError:
                    assert child.poll() is None
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(.1)
            native(Command.RESIZE, width=1280, height=800)
        def close():
            native(Command.CLOSE)
            child.wait(timeout=15)
            assert child.returncode == 0
        try:
            start('login')
            wait_label('Se connecter')
            native(Command.FILL_LOGIN, loginName=private['login_name'], password=private['password'])
            wait_label('Afficher ma maison')
            assert present()
            native(Command.TAP, label='Afficher ma maison')
            capture('home-after-read')
            wait_label('21.5 °C')
            capture('home-initial')
            # Change only the isolated server's external YAML while the same
            # native process remains alive. No UI model/route is injected.
            dashboard = directory / 'server-dashboard/dashboard.yaml'
            pid_before = child.pid
            binary_before = digest(binary)
            replacement = dashboard.with_suffix('.writing')
            shutil.copyfile(ROOT / 'qualification/frontend/dashboards/alternate.yaml', replacement)
            replacement.replace(dashboard)
            wait_label('Atelier connecté de test')
            wait_label('Mesure réaffectée')
            wait_label('Vitesse faible')
            assert not any(value.startswith('Salon, ') for row in tree() for value in [row['title'], row['description']])
            capture('server-config-hot-reload')
            native(Command.TAP, label='Mesure réaffectée, Vitesse faible')
            wait_label('fan.living_room_ceiling_fan')
            native(Command.TAP, label='Retour aux éléments')
            zone = next(row['title'] for row in tree() if row['role'] == 'AXButton' and row['title'].startswith('Zone expérimentale, '))
            native(Command.TAP, label=zone)
            wait_label('light.kitchen_main_lights')
            native(Command.TAP, label='Retour')
            assert child.pid == pid_before and digest(binary) == binary_before
            for sequence, action in [(1, FixtureAction.DENY), (2, FixtureAction.ALLOW)]:
                temporary = control / 'command.writing'
                write(temporary, {'sequence': sequence, 'action': action.value})
                temporary.replace(control / 'command.json')
                deadline = time.monotonic() + 8
                while not (control / ('ack-%02d.json' % sequence)).exists():
                    assert child.poll() is None and time.monotonic() < deadline
                    time.sleep(.05)
                if action is FixtureAction.DENY:
                    wait_label('Afficher ma maison')
                    assert not has('Atelier connecté de test')
                    assert not any(value.startswith('Zone expérimentale, ') for row in tree() for value in [row['title'], row['description']])
                    capture('dashboard-rights-withdrawn')
            native(Command.TAP, label='Afficher ma maison')
            wait_label('Atelier connecté de test')
            wait_label('Mesure réaffectée')
            shutil.copyfile(ROOT / 'qualification/frontend/dashboards/home.yaml', replacement)
            replacement.replace(dashboard)
            wait_label('Ma maison')
            wait_label('Confort principal')
            receipt['server_hot_reload'] = {'same_pid': pid_before, 'same_binary': binary_before, 'title_rooms_and_binding_changed': True, 'exact_observation_and_room_bindings_opened': True, 'rights_withdrawal_cleared_private_layout': True, 'fresh_authorized_read_restored_current_definition': True, 'source_restored_with_new_revision': True}
            write(directory / 'hot-reload-result.json', receipt['server_hot_reload'])
            room_checks = []
            for case in RoomCase:
                room = case.value
                first_seen_y = None
                first_anchor = next(row['y'] for row in tree() if row['role'] == 'AXButton' and row['title'].startswith(RoomCase.LIVING.value + ', '))
                # Public room descriptions are observed in the actual AX tree;
                # no Rust token, route or UI model is injected by the harness.
                for attempt in range(12):
                    candidates = [row for row in tree() if row['role'] == 'AXButton' and any(value.startswith(room + ', ') for value in [row['title'], row['description']])]
                    if not candidates:
                        native(Command.SCROLL, delta=-250)
                        time.sleep(.15)
                        continue
                    assert len(candidates) == 1, 'Ambiguous room action: ' + room
                    if first_seen_y is None:
                        first_seen_y = candidates[0]['y']
                    label = next(value for value in [candidates[0]['title'], candidates[0]['description']] if value.startswith(room + ', '))
                    frame = native(Command.INFO)['bounds']
                    if candidates[0]['y'] + candidates[0]['height'] > frame['Y'] + frame['Height'] - 16:
                        native(Command.SCROLL, delta=-250)
                        continue
                    try:
                        native(Command.TAP, label=label)
                        break
                    except AssertionError:
                        if attempt == 11:
                            raise
                        native(Command.SCROLL, delta=-250)
                        time.sleep(.15)
                wait_label('Retour')
                capture('room-' + case.name.lower())
                native(Command.TAP, label='Retour')
                wait_label(label)
                restored = next(row for row in tree() if label in (row['title'], row['description']))
                assert abs(restored['y'] - candidates[0]['y']) <= 1, 'Back must preserve the home scroll position'
                # The behavioral check also covers platforms where the actual
                # child FocusScope has no separate AXFocused property.
                native(Command.KEY, key=NativeKey.ENTER.value)
                capture('room-restored-key-' + case.name.lower())
                assert has('Retour'), 'Back must restore a live keyboard room opener'
                native(Command.TAP, label='Retour')
                wait_label(label)
                anchor_after = next(row['y'] for row in tree() if row['role'] == 'AXButton' and row['title'].startswith(RoomCase.LIVING.value + ', '))
                scrolled_by = first_anchor - anchor_after
                if case is RoomCase.OUTDOOR:
                    assert scrolled_by > 0, 'the restoration test must cover a nonzero home scroll'
                room_checks.append({'room': room, 'scroll_delta': restored['y'] - candidates[0]['y'], 'scrolled_before_open': scrolled_by, 'keyboard_reopened_opener': True})
            native(Command.TAP, label='Rechercher dans ma maison')
            formats = []
            for width, height in [(1024, 600), (1280, 800), (800, 1280)]:
                native(Command.RESIZE, width=width, height=height)
                for _ in range(3):
                    native(Command.SCROLL, delta=1000)
                formats.append({'requested': [width, height], 'actual': capture('home-' + str(width) + 'x' + str(height))})
            native(Command.RESIZE, width=1280, height=800)
            native(Command.TAP, label='Rechercher dans ma maison')
            for key in [NativeKey.LETTER_K, NativeKey.LETTER_I, NativeKey.LETTER_T, NativeKey.LETTER_C, NativeKey.LETTER_H, NativeKey.LETTER_E, NativeKey.LETTER_N]:
                native(Command.KEY, key=key.value)
            deadline = time.monotonic() + 8
            while not search_ready() and time.monotonic() < deadline:
                time.sleep(.1)
            assert search_ready(), 'global search must render a table containing only matching aliases'
            capture('search-kitchen')
            first_alias = next(row['text'] for row in tree() if re.fullmatch(r'[a-z_]+\.[a-z0-9_]+', row['text']))
            native(Command.TAP, label=first_alias)
            wait_label('Retour aux éléments')
            wait_label(first_alias)
            capture('entity-detail')
            native(Command.TAP, label='Retour aux éléments')
            assert search_ready(), 'detail Back must retain the current search'
            native(Command.TAP, label='Effacer la recherche')
            capture('search-cleared')
            # Settings are selected through the real persistent navigation.
            native(Command.TAP, label='Préférences')
            wait_label('Thème sombre')
            native(Command.TAP, label='Thème sombre')
            native(Command.TAP, label='Surfaces translucides')
            wait_label('Surfaces opaques')
            capture('preferences-translucent')
            native(Command.TAP, label='Surfaces opaques')
            native(Command.TAP, label='Autoriser les animations')
            wait_label('Réduire les animations')
            native(Command.TAP, label='Réduire les animations')
            native(Command.TAP, label='Texte à 200 %')
            wait_label('Préférences enregistrées sur cet appareil.')
            capture('preferences-dark-200')
            saved = json.loads(preferences.read_text())
            assert saved['version'] == '1'
            assert saved['preferences']['appearance'] == 'Dark' and saved['preferences']['text'] == 'Double'
            assert saved['preferences']['surfaces'] == 'Opaque' and saved['preferences']['motion'] == 'Reduced'
            close()
            start('restored')
            wait_label('Navigation')
            native(Command.TAP, label='Navigation')
            wait_label('Préférences')
            native(Command.TAP, label='Préférences')
            wait_label('Thème clair')
            wait_label('Texte standard')
            capture('preferences-restored')
            # Return to ordinary text before finishing the read-only profile.
            native(Command.TAP, label='Texte standard')
            native(Command.SIGN_OUT)
            wait_label('Se connecter')
            assert not present()
            capture('signed-out')
            close()
            receipt.update(status='AUTOMATED_CHECKS_PASS', binary_sha256=digest(binary), formats=formats, native_room_navigation=room_checks, native_focus_restore=True, native_query=True, preferences_persisted_across_restart=True, private_view_cleared=True)
        except BaseException as error:
            receipt['failure'] = str(error)
            if child is not None and child.poll() is None:
                try:
                    capture('failure')
                except Exception as capture_error:
                    receipt['capture_failure'] = str(capture_error)
            raise
        finally:
            if child is not None and child.poll() is None:
                child.terminate()
                child.wait(timeout=15)
            write(directory / 'result.json', receipt)
            if present():
                subprocess.run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, check=True, timeout=10)
            assert json.loads(public.read_text()) == session['public_profile'](private)
            public.unlink()
            if preferences.exists():
                preferences.unlink()
    observed = json.loads((control / 'result.json').read_text())
    # Rights notifications can require additional coherent StateService snapshots.
    assert 2 <= observed['state_read_calls'] <= 8
    report = session['validate_app_report'](control, 1, observed['state_read_calls'])
    assert report['dashboard_list_calls'] >= 1 and report['dashboard_get_calls'] >= 2 and report['dashboard_watch_calls'] >= 1
    assert report['command_driver_entries'] == 0 and report['action_rpc_calls'] == 0 and report['server_joined'] and report['connections_remaining'] == 0
    write(directory / 'server-invariants.json', {'driver_entries': 0, 'action_rpc_calls': 0, 'server_joined': True, 'connections_remaining': 0, 'state_view_reads_including_rights_resync': report['state_read_calls']})


def android(proof, server):
    transport, device = session['TRANSPORT'], session['device']
    package = session['PACKAGE']
    apk, ocr = android_input['prepare_apk'](ROOT, proof, run)
    directory = proof / 'android'
    with session['fixture'](directory, server, kind=session['FixtureKind'].CACHE, profile=UIProfile.HOME) as (private, control):
        with transport['android'](directory):
            existing = device('shell', 'pm', 'path', package, check=False)
            if existing.stdout.strip():
                raise RuntimeError('Preserve an existing emulator application')
            if existing.returncode not in (0, 1) or existing.stderr.strip():
                raise RuntimeError('Cannot establish that the emulator application is absent')
            port = urlsplit(private['origin']).port
            installed = forwarded = owned = False
            receipt = {'status': 'FAIL', 'fixture_profile': UIProfile.HOME.value, 'entity_count': 851}
            original_rotation = {key: device('shell', 'settings', 'get', 'system', key).stdout.strip()
                                 for key in ['accelerometer_rotation', 'user_rotation']}
            try:
                device('install', apk)
                installed = True
                device('reverse', '--no-rebind', f'tcp:{port}', f'tcp:{port}')
                forwarded = True
                device('shell', 'run-as', package, 'mkdir', '-p', 'files')
                run([transport['ADB'], '-s', transport['SERIAL'], 'shell', 'run-as', package, 'sh', '-c', shlex.quote('cat > files/endpoint.json')], input=json.dumps(session['public_profile'](private)), text=True, capture_output=True)
                owned = True
                device('shell', 'settings', 'put', 'system', 'accelerometer_rotation', '0')
                device('shell', 'settings', 'put', 'system', 'user_rotation', '0')
                device('shell', 'wm', 'density', '160')
                device('shell', 'wm', 'size', '1280x800')
                ui = android_input['AndroidInput'](directory, ocr, transport['ADB'], transport['SERIAL'], device)
                contains = android_input['LabelMatch'].CONTAINS
                credential_file = 'shared_prefs/ca.homeward.session.v1.' + session['namespace'](private) + '.xml'
                def records():
                    result = device('shell', 'run-as', package, 'cat', credential_file, check=False)
                    if result.returncode:
                        if 'No such file' in result.stderr:
                            return []
                        raise RuntimeError('Credential metadata unavailable')
                    return ET.fromstring(result.stdout).findall('string[@name="record"]')
                def start():
                    device('shell', 'am', 'start', '-n', package + '/' + session['ACTIVITY'])
                def back():
                    device('shell', 'input', 'keyevent', '4')
                    time.sleep(.3)
                def text(value):
                    result = device('shell', 'input', 'text', shlex.quote(value.replace(' ', '%s')), check=False)
                    if result.returncode:
                        raise AssertionError('Synthetic Android text input failed')
                def query_alias(query):
                    # Registry IDs and their ordering are private to each fresh
                    # fixture. Select a complete alias actually visible now.
                    deadline = time.monotonic() + 20
                    while True:
                        labels, _, _, _ = ui.labels()
                        aliases = [row for row in labels if row['confidence'] >= .5
                                   and re.fullmatch(r'[a-z_]+\.[a-z0-9_]+', row['label'])]
                        if aliases and all(query in row['label'] for row in aliases):
                            return min(aliases, key=lambda row: row['y'])['label']
                        if time.monotonic() >= deadline:
                            raise AssertionError('Visible inventory aliases do not match the native query')
                        time.sleep(.15)
                start()
                ui.tap('Identifiant', offset_y=40)
                text(private['login_name'])
                device('shell', 'input', 'keyevent', '61')
                text(private['password'])
                device('shell', 'input', 'keyevent', '66')
                ui.find('Afficher ma maison', match=contains)
                deadline = time.monotonic() + 8
                while len(records()) != 1 and time.monotonic() < deadline:
                    time.sleep(.1)
                assert len(records()) == 1
                ui.tap('Afficher ma maison', match=contains)
                ui.find('Confort principal', match=contains)
                ui.capture('home-initial')
                dashboard = directory / 'server-dashboard/dashboard.yaml'
                replacement = dashboard.with_suffix('.writing')
                pid_before = device('shell', 'pidof', package).stdout.strip()
                assert pid_before
                shutil.copyfile(ROOT / 'qualification/frontend/dashboards/alternate.yaml', replacement)
                replacement.replace(dashboard)
                ui.find('Atelier connecté', match=contains)
                ui.find('Mesure réaffectée', match=contains)
                ui.find('Vitesse faible', match=contains)
                ui.capture('server-config-hot-reload')
                ui.tap('Mesure réaffectée', match=contains)
                ui.find('fan.living_room_ceiling_fan', match=contains)
                back()
                ui.tap('Zone expérimentale', match=contains, scroll=True)
                ui.find('light.kitchen_main_lights', match=contains)
                back()
                assert device('shell', 'pidof', package).stdout.strip() == pid_before
                for sequence, action in [(1, FixtureAction.DENY), (2, FixtureAction.ALLOW)]:
                    temporary = control / 'command.writing'
                    write(temporary, {'sequence': sequence, 'action': action.value})
                    temporary.replace(control / 'command.json')
                    deadline = time.monotonic() + 8
                    while not (control / ('ack-%02d.json' % sequence)).exists():
                        assert time.monotonic() < deadline
                        time.sleep(.05)
                    if action is FixtureAction.DENY:
                        ui.find('Afficher ma maison', match=contains)
                        labels, _, _, _ = ui.labels()
                        assert not any('zoneexperimentale' in android_input['normalized'](row['label']) for row in labels)
                        ui.capture('dashboard-rights-withdrawn')
                ui.tap('Afficher ma maison', match=contains)
                ui.find('Atelier connecté', match=contains)
                ui.find('Mesure réaffectée', match=contains)
                shutil.copyfile(ROOT / 'qualification/frontend/dashboards/home.yaml', replacement)
                replacement.replace(dashboard)
                ui.find('Ma maison', match=contains)
                ui.find('Confort principal', match=contains)
                receipt['server_hot_reload'] = {'same_pid': pid_before, 'same_apk': digest(apk), 'title_rooms_and_binding_changed': True, 'exact_observation_and_room_bindings_opened': True, 'rights_withdrawal_cleared_private_layout': True, 'fresh_authorized_read_restored_current_definition': True, 'source_restored_with_new_revision': True}
                write(directory / 'hot-reload-result.json', receipt['server_hot_reload'])
                room_checks = []
                for case in [RoomCase.LIVING, RoomCase.KITCHEN, RoomCase.BEDROOM]:
                    before = ui.tap(case.value, scroll=True)
                    ui.find('Retour')
                    ui.capture('room-' + case.name.lower())
                    back()
                    after, _, _, _ = ui.find(case.value)
                    delta = abs(after['y'] - before['y'])
                    assert delta <= 2, 'System Back changed the room opener position'
                    device('shell', 'input', 'keyevent', '66')
                    ui.find('Retour')
                    back()
                    room_checks.append({'room': case.value, 'scroll_delta': delta, 'keyboard_reopened_opener': True})
                # Keep the stable search field focused while checking reflow,
                # rather than asking Android to reveal the last focused tile.
                ui.tap('Rechercher un élément', match=contains)
                back()
                formats = []
                for width, height in [(1024, 600), (1280, 800), (800, 1280)]:
                    path, actual_width, actual_height = ui.format(width, height, f'home-{width}x{height}')
                    formats.append({'requested': [width, height], 'actual': [actual_width, actual_height], 'capture': path.name})
                # A room selector is a separate page; system Back restores home.
                ui.tap('Ma maison')
                ui.find('Choisissez une pièce')
                back()
                ui.find('En un regard')
                # Closing the drawer must retain its underlying page.
                ui.navigation()
                ui.find('Préférences')
                back()
                ui.find('En un regard')
                # A search uses actual touch and Android's native text input.
                ui.tap('Rechercher un élément', match=contains)
                text('kitchen')
                time.sleep(.5)
                detail_alias = query_alias('kitchen')
                ui.capture('search-kitchen')
                back()  # Close the native IME before navigating the drawer.
                ui.tap(detail_alias, match=contains)
                ui.find('Retour aux éléments')
                ui.find(detail_alias, match=contains)
                ui.capture('entity-detail')
                ui.format(1024, 600, 'entity-detail-1024x600')
                ui.find(detail_alias, match=contains)
                back()
                ui.find(detail_alias, match=contains)
                ui.capture('search-1024x600')
                ui.format(1280, 800, 'search-1280x800')
                ui.tap(detail_alias, match=contains)
                ui.find('Retour aux éléments')
                ui.find(detail_alias, match=contains)
                ui.capture('entity-detail-1280x800')
                back()
                ui.format(800, 1280, 'search-restored-800x1280')
                ui.tap('kitchen')
                device('shell', 'input', 'keyevent', '123')
                for _ in 'kitchen':
                    device('shell', 'input', 'keyevent', '67')
                ui.find('Rechercher un élément', match=contains)
                back()
                # The header navigation trigger has an icon-only visual label;
                # its measured public 48dp slot is at the leading header edge.
                ui.navigation()
                ui.tap('Préférences')
                ui.tap('Thème sombre')
                ui.tap('Texte à 200', match=contains, scroll=True)
                ui.find('Préférences enregistrées', match=contains)
                saved = json.loads(device('shell', 'run-as', package, 'cat', 'files/interface.json').stdout)
                assert saved['preferences']['appearance'] == 'Dark' and saved['preferences']['text'] == 'Double'
                ui.capture('preferences-dark-200')
                ui.navigation()
                ui.tap('Accueil')
                ui.format(1024, 600, 'home-dark-200-1024x600')
                before_press, before = ui.observe_press(RoomCase.LIVING.value, scroll=True)
                ui.find('Retour')
                ui.capture('room-dark-200')
                back()
                after, _, _, _ = ui.find(RoomCase.LIVING.value)
                write(directory / 'doubled-text-focus-phases.json', {'before_down': before_press,
                    'while_pressed': before, 'after_back': after,
                    'focus_reveal_delta_px': before['y'] - before_press['y'],
                    'return_delta_px': after['y'] - before['y']})
                assert abs(after['y'] - before['y']) <= 2, 'Doubled text changed the room opener on Back'
                # Exercise an ordinary tap too after the observed focus phase.
                before_tap = ui.tap(RoomCase.LIVING.value)
                ui.find('Retour')
                back()
                after_tap, _, _, _ = ui.find(RoomCase.LIVING.value)
                assert abs(after_tap['y'] - before_tap['y']) <= 2, 'Ordinary doubled-text tap changed the return position'
                ui.capture('home-dark-200-restored')
                ui.format(800, 1280, 'home-dark-200-800x1280')
                ui.navigation()
                ui.tap('Préférences')
                device('shell', 'am', 'force-stop', package)
                start()
                ui.find('Afficher ma maison', match=contains)
                ui.navigation()
                ui.tap('Préférences')
                ui.find('Thème clair')
                ui.find('Texte standard')
                ui.capture('preferences-restored')
                ui.tap('Texte standard')
                ui.tap('Se déconnecter', scroll=True)
                ui.find('Se connecter')
                deadline = time.monotonic() + 8
                while records() and time.monotonic() < deadline:
                    time.sleep(.1)
                assert not records()
                ui.capture('signed-out')
                receipt.update(status='AUTOMATED_CHECKS_PASS', apk_sha256=digest(apk), formats=formats,
                               native_room_navigation=room_checks, native_back_and_keyboard_restore=True,
                               native_query_and_detail=True, preferences_persisted_across_restart=True,
                               selected_detail_alias=detail_alias,
                               doubled_text_native_room_and_back=True,
                               physical_device_qualified=False, captures_require_visual_inspection=True)
            except BaseException as error:
                receipt['failure'] = str(error)
                if installed and 'ui' in locals():
                    try:
                        ui.capture('failure')
                    except Exception as capture_error:
                        receipt['capture_failure'] = str(capture_error)
                raise
            finally:
                write(directory / 'result.json', receipt)
                if installed:
                    device('shell', 'am', 'force-stop', package, check=False)
                if owned:
                    current = json.loads(device('shell', 'run-as', package, 'cat', 'files/endpoint.json').stdout)
                    assert current == session['public_profile'](private)
                if forwarded:
                    device('reverse', '--remove', f'tcp:{port}')
                device('shell', 'wm', 'size', 'reset', check=False)
                device('shell', 'wm', 'density', 'reset', check=False)
                for key, value in original_rotation.items():
                    if value == 'null':
                        device('shell', 'settings', 'delete', 'system', key)
                    else:
                        device('shell', 'settings', 'put', 'system', key, value)
                if installed:
                    device('uninstall', package)
    observed = json.loads((control / 'result.json').read_text())
    assert 2 <= observed['state_read_calls'] <= 8
    report = session['validate_app_report'](control, 1, observed['state_read_calls'])
    assert report['dashboard_list_calls'] >= 1 and report['dashboard_get_calls'] >= 2 and report['dashboard_watch_calls'] >= 1
    assert report['command_driver_entries'] == 0 and report['action_rpc_calls'] == 0
    write(directory / 'server-invariants.json', {'driver_entries': 0, 'action_rpc_calls': 0, 'server_joined': report['server_joined'], 'state_view_reads_including_rights_resync': report['state_read_calls']})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', type=PlatformSelection, choices=list(PlatformSelection), default=PlatformSelection.DESKTOP)
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through scripts/frontend.py run --cargo.')
    proof = Path(session['CONFIG']['frontend_cache']) / 'navigation-ui' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    print('Native navigation proof: ' + str(proof), flush=True)
    server = proof / 'server.test'
    run(['go', 'test', '-c', '-race', '-o', server, './internal/server'])
    for platform in args.platform.platforms():
        match platform:
            case Platform.DESKTOP:
                desktop(proof, server)
            case Platform.ANDROID:
                android(proof, server)
    write(proof / 'result.json', {'status': 'AUTOMATED_CHECKS_PASS', 'server_sha256': digest(server), 'platforms': [platform.value for platform in args.platform.platforms()], 'captures_require_visual_inspection': True})


if __name__ == '__main__':
    main()
