#!/usr/bin/env python3
"""Normal frontend, production dashboard transport, two real SQL publications.

Run through frontend.py's reservation. Build the NORMAL executable and Go test
server once beforehand. This runner never enables dashboard-preview, sets Slint
properties, or invokes callbacks. --serve-only exposes the same isolated TLS
fixture for separately coordinated Android qualification; it never uses ADB.
"""
import argparse
import contextlib
import hashlib
import json
import os
from pathlib import Path
import runpy
import shutil
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write(path, value):
    temporary = path.with_name(path.name + '.writing')
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')
    temporary.chmod(0o600)
    temporary.replace(path)


def wait_for(predicate, description, children=(), timeout=35):
    deadline = time.monotonic() + timeout
    while True:
        value = predicate()
        if value:
            return value
        if any(child.poll() is not None for child in children):
            raise RuntimeError('Owned process stopped while waiting for ' + description)
        if time.monotonic() > deadline:
            raise AssertionError('Timed out: ' + description)
        time.sleep(.1)


def stop(child):
    if child is None or child.poll() is not None:
        return
    child.terminate()
    try:
        child.wait(15)
    except subprocess.TimeoutExpired:
        child.kill()
        child.wait(5)


def run(args):
    proof = args.proof.resolve()
    proof.mkdir(mode=0o700, parents=True, exist_ok=False)
    sources = proof / 'sources'
    shutil.copytree(ROOT / 'qualification/frontend/dashboard-widgets', sources)
    fixture = proof / 'fixture'
    server = app = None
    namespace = None
    namespace_owned = False
    owned_profiles = {}
    sequence = 0
    binary_hash = digest(args.binary) if args.binary else None
    receipt = dict(status='FAIL', production_dashboard_service=True,
                   production_configuration_apply=True, synthetic_only=True,
                   physical_device_qualified=False, same_binary_sha256=binary_hash,
                   appearance=args.appearance, text_size=args.text,
                   viewport=dict(width=args.width,height=args.height),
                   server_sources={p.name:digest(p) for p in sources.iterdir() if p.is_file()})
    session = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))

    def control(action):
        nonlocal sequence
        sequence += 1
        write(fixture / 'command.json', dict(sequence=sequence, action=action))
        return wait_for(lambda: read(fixture / 'control-result.json', {}).get('sequence') == sequence,
                        'server control ' + action, [server])

    def submitted_commands(expected):
        def snapshot():
            control('snapshot')
            requests = read(fixture / 'control-result.json').get('device_requests') or []
            if len(requests) > expected:
                raise AssertionError('Unexpected duplicate native command')
            return requests if len(requests) == expected else None
        return wait_for(snapshot, 'exactly ' + str(expected) + ' native command(s)', [server, app])

    def read(path, default=None):
        return json.loads(path.read_text()) if path.exists() else default

    def native(command, **fields):
        result = subprocess.run([str(args.helper), '-'],
            input=json.dumps(dict(pid=app.pid, command=command, syntheticOnly=True, **fields)),
            text=True, capture_output=True, check=True, timeout=12)
        return json.loads(result.stdout) if result.stdout.strip() else None

    def tree():
        return native('tree')

    def window_ready():
        try:
            info = native('info')
            native('focus')
            return info
        except (subprocess.CalledProcessError, json.JSONDecodeError):
            return None

    def size_window():
        try:
            native('resize', width=args.width, height=args.height)
            return True
        except subprocess.CalledProcessError:
            # Cocoa can report a failed no-op while the window already has
            # the requested content size. Confirm geometry before continuing.
            windows = [row for row in tree() if row['role'] == 'AXWindow']
            if windows and abs(windows[0]['width'] - args.width) <= 2 and abs(windows[0]['height'] - args.height - 32) <= 4:
                return True
            return False

    def labels():
        return [value for row in tree() for value in (row['title'], row['description'], row['text']) if value]

    def wait(label):
        wait_for(lambda: any(label in value for value in labels()), label, [server, app])

    def absent(label):
        wait_for(lambda: all(label not in value for value in labels()), 'absent ' + label, [server, app])

    def top():
        for _ in range(5):
            native('scroll', delta=900)

    def capture(name):
        info = native('info')
        subprocess.run(['screencapture', '-x', '-o', '-l', str(info['window_id']), str(proof / (name + '.png'))], check=True, capture_output=True)
        write(proof / (name + '.tree.json'), tree())
        write(proof / (name + '.window.json'), info)

    def position(label):
        rows = [row for row in tree() if row['width'] > 0 and row['height'] > 0 and
                any(row[field] == label for field in ('title', 'description', 'text'))]
        if not rows:
            raise AssertionError('Missing native layout geometry: ' + label)
        row = min(rows, key=lambda item: (item['y'], item['x'], item['width'] * item['height']))
        return dict(x=row['x'], y=row['y'], width=row['width'], height=row['height'])

    def precedes(left, right):
        return (round(left['y'] / 4), left['x']) < (round(right['y'] / 4), right['x'])

    def thermostat_shows(value):
        rows = tree()
        groups = [row for row in rows if row['role'] == 'AXGroup' and row['title'] == 'Confort configuré']
        return any(value in row['text'].replace(',', '.') and group['x'] <= row['x'] < group['x'] + group['width'] and
                   group['y'] <= row['y'] < group['y'] + group['height'] for group in groups for row in rows if row['role'] == 'AXStaticText')

    def increase():
        for _ in range(18):
            candidates = [row for row in tree() if row['role'] == 'AXButton' and
                          any(args.increase_label in row[field] for field in ('title', 'description'))]
            if candidates:
                candidate = candidates[0]
                bounds = native('info')['bounds']
                center = candidate['y'] + candidate['height'] / 2 - bounds['Y']
                if 35 < center < bounds['Height'] - 35:
                    label = candidate['title'] or candidate['description']
                    native('tap', label=label)
                    return
                native('scroll', delta=-250 if center > 35 else 250)
                continue
            native('scroll', delta=-250)
        raise AssertionError('Configured thermostat stepper absent')

    def tap_button_containing(label):
        row = next(row for row in tree() if row['role'] == 'AXButton' and any(label in row[field] for field in ('title', 'description')))
        native('tap', label=row['title'] or row['description'])

    with (proof / 'server.log').open('wb') as server_log, (proof / 'frontend.log').open('wb') as app_log:
        try:
            env = dict(os.environ, HOMEWARD_DASHBOARD_WIDGETS_FIXTURE=str(fixture),
                       HOMEWARD_DASHBOARD_WIDGETS_SOURCES=str(sources),
                       HOMEWARD_DASHBOARD_WIDGETS_SYNTHETIC_ONLY='1',
                       HOMEWARD_DASHBOARD_WIDGETS_LIFETIME_MINUTES=str(args.fixture_lifetime_minutes))
            server = subprocess.Popen([str(args.server), '-test.run=^TestDashboardWidgetsConfigurationServer$', '-test.v', f'-test.timeout={args.fixture_lifetime_minutes+1}m'],
                                      cwd=ROOT, env=env, stdout=server_log, stderr=subprocess.STDOUT)
            private = wait_for(lambda: read(fixture / 'ready.json'), 'isolated production TLS fixture', [server], timeout=90)
            origin = urlsplit(private['origin'])
            if private['mode'] != 'synthetic-only' or origin.scheme != 'https' or origin.hostname != '127.0.0.1' or not origin.port:
                raise RuntimeError('Only fresh local synthetic TLS fixture admitted')
            write(proof / 'public-endpoint.json', session['public_profile'](private))
            if args.smoke_only:
                first = read(fixture / 'wire-a.json')['dashboard']
                control('apply-b')
                second = read(fixture / 'wire-b.json')['dashboard']
                def widgets(document):
                    return [node for node in document['nativeLayout']['nodes'] if 'widget' in node]
                a, b = widgets(first), widgets(second)
                assert [node['title'] for node in a] == ['Confort configuré', 'Pièce du serveur A', 'Bloc conditionnel', 'Agenda uniquement A', 'Navigation du serveur', 'Liste uniquement A']
                assert [node['title'] for node in b] == ['Épicerie uniquement B', 'Bloc conditionnel', 'Confort configuré', 'Pièce du serveur B', 'Navigation du serveur']
                assert a[0]['widget']['thermostat']['entityId'] == private['entity_bindings']['climate.neviweb130_climate_sous_sol']
                assert b[2]['widget']['thermostat']['entityId'] == private['entity_bindings']['climate.chambre_eugenie']
                assert a[5]['widget']['tasks']['listId'] == private['list_bindings']['A']
                assert b[0]['widget']['tasks']['listId'] == private['list_bindings']['B']
                assert a[5]['widget']['tasks']['limit'] == 1 and b[0]['widget']['tasks']['limit'] == 2
                assert first['metadata']['version'] != second['metadata']['version']
                assert 'definition' not in first and 'definition' not in second
                assert all(node.get('kind') != 'LAYOUT_KIND_UNSUPPORTED' for node in first['nativeLayout']['nodes'] + second['nativeLayout']['nodes'])
                for action in ('condition-off', 'condition-on', 'condition-stale', 'condition-on'):
                    control(action)
                control('apply-showcase')
                showcase = read(fixture / 'wire-showcase.json')['dashboard']
                assert all(node.get('kind') != 'LAYOUT_KIND_UNSUPPORTED' for node in showcase['nativeLayout']['nodes'])
                receipt.update(status='PASS', ui_qualified=False, wire_structure_and_bindings_changed=True,
                               no_raw_yaml_in_dashboard=True)
                return
            if args.serve_only:
                write(proof / 'server-ready.json', dict(fixture=str(fixture), public_endpoint=str(proof / 'public-endpoint.json'),
                      controls=['apply-a', 'apply-b', 'apply-showcase', 'condition-on', 'condition-off', 'condition-stale', 'snapshot']))
                wait_for(lambda: (fixture / 'stop').exists(), 'externally coordinated qualification completion', [server], timeout=max(10,args.fixture_lifetime_minutes*60-50))
                receipt.update(status='SERVER_ONLY', ui_qualified=False)
                return
            namespace = session['namespace'](private)
            existing = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True)
            if existing.returncode != 44:
                raise RuntimeError('Preserve existing credential namespace')
            namespace_owned = True
            # Keep the normal macOS HOME so Security.framework can resolve its
            # native login keychain. Own only fresh public profile files, as in
            # the existing native session qualification.
            product = Path.home() / 'Library/Application Support/Homeward'
            if any((product / name).exists() for name in ('endpoint.json', 'interface.json')):
                raise RuntimeError('Preserve existing product endpoint/preferences')
            product.mkdir(parents=True, mode=0o700, exist_ok=True)
            write(product / 'endpoint.json', session['public_profile'](private))
            write(product / 'interface.json', dict(version='1', preferences=dict(appearance=args.appearance, surfaces='Opaque', text=args.text, motion='Reduced')))
            owned_profiles = {product / name: json.loads((product / name).read_text()) for name in ('endpoint.json', 'interface.json')}
            app = subprocess.Popen([str(args.binary)], cwd=ROOT, env=dict(os.environ, SLINT_BACKEND='winit-software'), stdout=app_log, stderr=subprocess.STDOUT)
            receipt['frontend_pid'] = app.pid
            wait_for(window_ready, 'native window', [server, app])
            wait_for(size_window, 'native requested viewport', [server, app])
            native('info')
            # Slint activates its native accessibility children lazily after a
            # real pointer event. Helper click coordinates are window-relative.
            native('click', x=8, y=40)
            wait('Se connecter')
            native('fill-login', loginName=private['login_name'], password=private['password'])
            wait_for(lambda: any('Maison configurée A' in value or 'Afficher ma maison' in value for value in labels()), 'authenticated dashboard', [server, app])
            if any('Afficher ma maison' in value for value in labels()):
                native('tap', label='Afficher ma maison')
            wait('Confort configuré')
            wait_for(lambda: thermostat_shows('21.5'), 'exact first observed climate value', [server, app])
            wait('Pièce du serveur A')
            wait('Agenda uniquement A')
            wait('Pommes de la liste A')
            absent('Hors limite A')
            absent('Lait de la liste B')
            absent('Épicerie uniquement B')
            absent('Montréal')
            geometry_a = {label: position(label) for label in ('Confort configuré', 'Pièce du serveur A')}
            assert precedes(geometry_a['Confort configuré'], geometry_a['Pièce du serveur A'])
            capture('configuration-a')
            tap_button_containing('Pièce du serveur A')
            wait('Destination configurée')
            capture('configured-room-destination')
            native('tap', label='Retour')
            wait('Confort configuré')
            wait_for(lambda: any(row.get('focused') and any('Pièce du serveur A' in row[field] for field in ('title', 'description')) for row in tree()), 'configured room focus restored after Back', [server, app])
            increase()
            first_requests = submitted_commands(1)
            first_target = first_requests[0]['target']['entityId']
            assert first_target == private['entity_bindings']['climate.neviweb130_climate_sous_sol']
            assert first_requests[0]['arguments']['setClimate']['setpointCelsius'] == 21.5
            control('apply-b')
            top()
            wait('Maison configurée B')
            wait('Épicerie uniquement B')
            wait('Pièce du serveur B')
            wait_for(lambda: thermostat_shows('16.5'), 'exact second observed climate value', [server, app])
            absent('Agenda uniquement A')
            absent('Pièce du serveur A')
            absent('Montréal')
            wait('Lait de la liste B')
            wait('Pain de la liste B')
            absent('Hors limite B')
            absent('Pommes de la liste A')
            geometry_b = {label: position(label) for label in ('Épicerie uniquement B', 'Confort configuré')}
            assert precedes(geometry_b['Épicerie uniquement B'], geometry_b['Confort configuré'])
            write(proof / 'native-order.json', dict(a=geometry_a, b=geometry_b))
            capture('configuration-b')
            increase()
            requests = submitted_commands(2)
            assert requests[1]['target']['entityId'] == private['entity_bindings']['climate.chambre_eugenie'] != first_target
            assert requests[1]['arguments']['setClimate']['setpointCelsius'] == 18.5
            assert thermostat_shows('16.5'), 'command must not replace observed current temperature'
            native('tap', label='Lait de la liste B')
            def list_checkbox_committed():
                control('snapshot')
                list_state = read(fixture / 'lists-current.json')
                def shared_item(slot):
                    return next(row for row in list_state[slot]['items'] if row['uid']['value'] == 'shared-uid')
                assert not shared_item('A')['value'].get('completed', False)
                return shared_item('B')['value'].get('completed', False)
            wait_for(list_checkbox_committed, 'checkbox commit on exact second list', [server, app])
            control('condition-off')
            absent('Bloc conditionnel')
            capture('condition-false')
            control('condition-on')
            wait('binary_sensor.arc_soundbar_microphone')
            absent('Condition de visibilité non disponible')
            control('condition-stale')
            wait('Bloc conditionnel')
            wait('Condition de visibilité non disponible')
            absent('binary_sensor.arc_soundbar_microphone')
            capture('condition-stale')
            control('condition-on')
            wait('binary_sensor.arc_soundbar_microphone')
            absent('Condition de visibilité non disponible')
            control('apply-showcase')
            top()
            wait('Vue d’ensemble')
            wait('Salon')
            absent('Confort configuré')
            capture('showcase-tablet')
            assert app.poll() is None and digest(args.binary) == binary_hash
            receipt.update(status='PASS', same_process=True, native_commands=2, exact_entity_binding_changed=True,
                           omitted_blocks_absent=True, native_order_changed=True, exact_list_source_and_limit=True,
                           same_uid_checkbox_scoped_to_list=True, visibility_fresh_only=True, ui_qualified=True,
                           configured_room_navigation_and_focus_restored=True,
                           server_sources={p.name: digest(p) for p in sources.iterdir() if p.is_file()})
        finally:
            if app is not None:
                with contextlib.suppress(Exception):
                    capture('final')
                stop(app)
            if namespace_owned:
                subprocess.run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True)
                remaining = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True)
                if remaining.returncode != 44:
                    receipt['status'] = 'FAIL'
                    receipt['error'] = 'Owned synthetic keychain cleanup unconfirmed'
            for path, expected in owned_profiles.items():
                try:
                    if path.exists() and json.loads(path.read_text()) == expected:
                        path.unlink()
                    elif path.exists():
                        receipt['status'] = 'FAIL'
                        receipt['error'] = 'Product profile changed outside qualification; preserved'
                except (OSError, ValueError):
                    receipt['status'] = 'FAIL'
                    receipt['error'] = 'Product profile cleanup could not be verified; preserved'
            if fixture.exists():
                (fixture / 'stop').touch(mode=0o600)
                if server is not None:
                    with contextlib.suppress(subprocess.TimeoutExpired):
                        server.wait(25)
            stop(server)
            receipt['server_exit'] = server.returncode if server else None
            if receipt['status'] == 'PASS' and (receipt['server_exit'] != 0 or not read(fixture / 'result.json', {}).get('server_joined')):
                receipt['status'] = 'FAIL'
                receipt['error'] = 'Owned server did not complete cleanly'
            write(proof / 'result.json', receipt)
            if receipt.get('error'):
                raise AssertionError(receipt['error'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--proof', type=Path, required=True)
    parser.add_argument('--server', type=Path, required=True)
    parser.add_argument('--binary', type=Path)
    parser.add_argument('--helper', type=Path)
    parser.add_argument('--increase-label', default='Augmenter')
    parser.add_argument('--appearance', choices=('Light', 'Dark'), default='Light')
    parser.add_argument('--text', choices=('Standard', 'Double'), default='Standard')
    parser.add_argument('--width', type=int, default=1280)
    parser.add_argument('--height', type=int, default=800)
    parser.add_argument('--fixture-lifetime-minutes', type=int, choices=range(1,61), default=20, metavar='1..60',
                        help='isolated synthetic fixture lifetime; use 60 for a physical native run')
    parser.add_argument('--serve-only', action='store_true')
    parser.add_argument('--smoke-only', action='store_true')
    args = parser.parse_args()
    if args.serve_only and args.smoke_only:
        parser.error('choose either server-only or smoke-only')
    if not (args.serve_only or args.smoke_only) and (args.binary is None or args.helper is None):
        parser.error('normal desktop qualification requires --binary and --helper')
    for path in (args.server, args.binary, args.helper):
        if path is not None and not path.is_file():
            parser.error('missing built artifact: ' + str(path))
    run(args)


if __name__ == '__main__':
    main()
