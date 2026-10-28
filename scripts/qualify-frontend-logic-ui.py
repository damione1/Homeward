#!/usr/bin/env python3
"""Native logic inspector through Slint MCP and an isolated Go TLS fixture.

Run through frontend.py run --cargo. Injected Slint events and native captures
are not physical touch, IME, Android or screen-reader qualification.
"""
import argparse
import base64
import contextlib
from enum import Enum
import json
import hashlib
import os
from pathlib import Path
import runpy
import socket
import subprocess
import time
from urllib.error import URLError
from io import BytesIO
from PIL import Image, ImageDraw
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
MCP = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-mcp.py'))
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
NATIVE = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-recipes-ui.py'))
Tool, structured, Case = MCP['Tool'], MCP['structured'], NATIVE['Case']


class Key(Enum):
    END = '\uf72b'
    HOME = '\uf729'


class NativeCommand(Enum):
    FOCUS = 'focus'
    CLOSE = 'close'


def run_case(proof, binary, server_binary, helper, case):
    directory = proof / case.value
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    product = Path.home() / 'Library/Application Support/Homeward'
    profile, preferences = product / 'endpoint.json', product / 'interface.json'
    if profile.exists() or preferences.exists():
        raise RuntimeError('Preserve existing Homeward settings')
    server = app = private = namespace = None
    credential_owned = profile_owned = preferences_owned = False
    client = window = root = None
    appearance, text = case.preferences()
    preference_value = dict(version='1', preferences=dict(appearance=appearance.value,
        surfaces='Opaque', text=text.value, motion='Reduced'))

    def focus():
        subprocess.run([str(helper), '-'], input=json.dumps(dict(
            command=NativeCommand.FOCUS.value, pid=app.pid, syntheticOnly=True)),
            text=True, capture_output=True, check=True, timeout=10)

    def tree():
        # MCP injects Slint events but does not activate the macOS application.
        # Match the ordinary native helper: keep the owned window exposed before
        # every interaction/read, so occlusion cannot retire the tested session.
        focus()
        return structured(client.tool(Tool.TREE, elementHandle=root, maxElements=3000))['elements']

    def rows(label, role=None):
        return [row for row in tree() if label in row.get('accessibleLabel', '')
                and (role is None or row.get('accessibleRole') == role)
                and row.get('computedOpacity', 1) > 0 and row.get('size', {}).get('height', 0) > 0]

    def wait(label, role=None, timeout=20):
        deadline = time.monotonic() + timeout
        while True:
            found = rows(label, role)
            if found:
                return found[0]
            if app.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('Native administration surface missing: ' + label)
            time.sleep(.05)

    def panel():
        panels = [row for row in tree() if any(value.get('id') == 'HomewardWindow::settings-pane'
                  for value in row.get('typeNamesAndIds', [])) and row.get('size', {}).get('height', 0) > 0]
        return panels[0] if panels else None

    def scroll(delta):
        pane = panel()
        if pane is None:
            # The pre-login surface fills the window.
            x, y, w, h = 0, 0, size['width'] / scale, size['height'] / scale
        else:
            position, extent = pane['absolutePosition'], pane['size']
            x, y, w, h = position.get('x', 0), position.get('y', 0), extent['width'], extent['height']
        client.tool(Tool.SCROLL, windowHandle=window,
            position=dict(x=x + w - 8, y=y + h / 2), deltaX=0, deltaY=delta)
        return y + 8, y + h - 8

    def reveal(label, role='Button'):
        top, bottom = 10, size['height'] / scale - 15
        pane = panel()
        if pane:
            top = pane['absolutePosition'].get('y', 0) + 8
            bottom = top + pane['size']['height'] - 16
        for attempt in range(40):
            found = rows(label, role)
            if found:
                exact = [row for row in found if label in row.get('accessibleLabel', '')]
                row = (exact or found)[0]
                y = row.get('absolutePosition', {}).get('y', 0)
                height = row.get('size', {}).get('height', 0)
                if top < y + height / 2 < bottom:
                    return row
                delta = 260 if y < top else -260
            else:
                delta = 10000 if attempt == 0 else -260
            scroll(delta)
            time.sleep(.04)
        raise RuntimeError('Native administration control is unreachable: ' + label)

    def tap(label, role='Button'):
        row = reveal(label, role)
        client.tool(Tool.CLICK, elementHandle=row['handle'])

    def fill(label, value):
        row = reveal(label, 'TextInput')
        client.tool(Tool.CLICK, elementHandle=row['handle'])
        client.tool(Tool.VALUE, elementHandle=row['handle'], value=value)

    def record(label):
        # Expose the table header before scrolling its virtualized list. The
        # table may be taller than the outer viewport at 200% text size.
        reveal('', 'Table')
        for _ in range(4):
            table = next(row for row in tree() if row.get('accessibleRole') == 'Table')
            pane = panel()
            desired = pane['absolutePosition'].get('y', 0) + 8
            difference = desired - table['absolutePosition'].get('y', 0)
            if abs(difference) < 12:
                break
            scroll(difference)
            time.sleep(.04)
        for attempt in range(45):
            elements = tree()
            inner = next(row for row in elements if any(value.get('id') == 'KitDataTable::list'
                         for value in row.get('typeNamesAndIds', [])))
            pane = panel()
            position, extent = inner['absolutePosition'], inner['size']
            top = max(position.get('y', 0), pane['absolutePosition'].get('y', 0)) + 8
            bottom = min(position.get('y', 0) + extent['height'],
                         pane['absolutePosition'].get('y', 0) + pane['size']['height']) - 8
            found = [row for row in elements if row.get('accessibleRole') == 'ListItem' and label in row.get('accessibleLabel', '')]
            if found:
                row = found[0]
                middle = row['absolutePosition'].get('y', 0) + row['size']['height'] / 2
                if top < middle < bottom:
                    return row
            client.tool(Tool.SCROLL, windowHandle=window,
                position=dict(x=position.get('x', 0) + extent['width'] / 2, y=(top + bottom) / 2),
                deltaX=0, deltaY=10000 if attempt == 0 else -180)
            time.sleep(.04)
        raise RuntimeError('Virtualized native record is unreachable: ' + label)

    def capture(name):
        elements = tree()
        response = client.tool(Tool.SCREENSHOT, windowHandle=window)
        image = next(item for item in response['content'] if item.get('type') == 'image')
        pixels = Image.open(BytesIO(base64.b64decode(image['data'], validate=True))).convert('RGB')
        # Recovery codes are tested in memory and never persisted as pixels.
        for element in elements:
            if element.get('accessibleRole') == 'TextInput' and element.get('accessibleLabel') == 'Codes à conserver':
                bounds, extent = element['absolutePosition'], element['size']
                factor = pixels.width / (size['width'] / scale)
                x, y = bounds.get('x', 0) * factor, bounds.get('y', 0) * factor
                ImageDraw.Draw(pixels).rectangle((x - 1, y - 1, x + extent['width'] * factor + 1, y + extent['height'] * factor + 1), fill='#777777')
        pixels.save(directory / (name + '.png'))
        # Retain geometry and labels only; never an editable or secret value.
        safe_keys = ('absolutePosition', 'size', 'accessibleRole', 'accessibleLabel',
                     'accessibleEnabled', 'computedOpacity', 'typeNamesAndIds')
        (directory / (name + '.tree.json')).write_text(json.dumps(
            [{key: row[key] for key in safe_keys if key in row} for row in elements], ensure_ascii=False))

    def settings():
        if not rows('Préférences', 'ListItem'):
            tap('Navigation')
        tap('Préférences', 'ListItem')
        reveal('Mon compte', 'Button')

    def sign_in(credentials):
        scroll(10000)
        fill('Identifiant', credentials['login_name'])
        fill('Mot de passe', credentials['password'])
        tap('Se connecter')

    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF020LogicServer$',
                '-test.v', '-test.timeout=11m'], cwd=ROOT / 'internal/server',
                env=dict(os.environ, HOMEWARD_NHF020_SYNTHETIC_ONLY='1',
                    HOMEWARD_NHF020_FIXTURE=str(fixture)),
                stdout=server_log, stderr=subprocess.STDOUT)
            private = SHARED['wait_file'](fixture / 'ready.json', server, timeout=120)
            origin = urlsplit(private['origin'])
            if not (private['mode'] == 'synthetic-only' and private['production_configuration'] is False
                    and private['no_physical_providers'] is True and origin.scheme == 'https'
                    and origin.hostname == '127.0.0.1' and origin.port and not origin.username
                    and not origin.password and not origin.query and not origin.fragment):
                raise RuntimeError('Explicit isolated loopback administration fixture required')
            namespace = SESSION['namespace'](private)
            present = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True).returncode
            if present != 44:
                raise RuntimeError('Preserve existing or unverified credential namespace')
            credential_owned = True
            product.mkdir(parents=True, exist_ok=True)
            with profile.open('x') as output:
                profile.chmod(0o600)
                json.dump(SESSION['public_profile'](private), output)
                profile_owned = True
            with preferences.open('x') as output:
                preferences.chmod(0o600)
                json.dump(preference_value, output)
                preferences_owned = True
            with socket.socket() as reservation:
                reservation.bind(('127.0.0.1', 0))
                port = reservation.getsockname()[1]
            app = subprocess.Popen([binary], cwd=ROOT, stdout=app_log, stderr=subprocess.STDOUT,
                env=dict(os.environ, SLINT_BACKEND='winit-software', SLINT_MCP_PORT=str(port)))
            client = MCP['Mcp'](port)
            deadline = time.monotonic() + 20
            while True:
                try:
                    client.rpc('initialize', dict(protocolVersion='2025-06-18', capabilities={},
                        clientInfo=dict(name='homeward-administration-qualification', version='1')))
                    break
                except URLError:
                    if app.poll() is not None or time.monotonic() >= deadline:
                        raise
                    time.sleep(.1)
            listeners = subprocess.check_output(['lsof', '-nP', '-a', '-p', str(app.pid), '-iTCP', '-sTCP:LISTEN'], text=True)
            listener_rows = listeners.strip().splitlines()[1:]
            if len(listener_rows) != 1 or f'127.0.0.1:{port} (LISTEN)' not in listener_rows[0]:
                raise RuntimeError('MCP listener must belong to the owned loopback process')
            (directory / 'listeners.txt').write_text(listeners)
            windows = structured(client.tool(Tool.WINDOWS))['windowHandles']
            if len(windows) != 1:
                raise RuntimeError('One owned native window required')
            window = windows[0]
            properties = structured(client.tool(Tool.PROPERTIES, windowHandle=window))
            root, size, scale = properties['rootElementHandle'], properties['size'], properties['scaleFactor']
            focus()
            def field_value(label, expected=None):
                reveal(label, 'TextInput')
                deadline = time.monotonic() + 25
                while True:
                    found = rows(label, 'TextInput')
                    if found:
                        value = found[0].get('accessibleValue', '')
                        if expected is None or expected in value:
                            return value
                    if app.poll() is not None or time.monotonic() > deadline:
                        raise RuntimeError('Native configuration field missing expected value: ' + label)
                    time.sleep(.05)

            def ready_action(label):
                deadline = time.monotonic() + 25
                while True:
                    row = reveal(label)
                    if row.get('accessibleEnabled', False):
                        return
                    if time.monotonic() > deadline:
                        raise RuntimeError('Native configuration action remained disabled: ' + label)
                    time.sleep(.05)

            sign_in(private['admin'])
            wait('Session sécurisée active', timeout=30)
            settings()
            tap('Logique et traces')
            record('17 définitions retenues · 72 reportées')
            capture('catalogue')
            client.tool(Tool.CLICK, elementHandle=record('Définition synthétique 01')['handle'])
            record('Source protégée')
            capture('definition')
            tap('Exécuter · synthétique')
            field_value('Tentative explicite')
            capture('start-confirmation')
            tap('Confirmer une seule tentative')
            deadline = time.monotonic() + 20
            while not any('Run synthétique enregistré' in row.get('accessibleLabel', '') for row in tree()):
                if time.monotonic() > deadline:
                    raise RuntimeError('Native explicit synthetic start did not finish')
                time.sleep(.05)
            capture('started')
            tap('Fermer')
            ready_action('Exécuter · synthétique')
            # The first dynamic run identity is read from a server-rendered row,
            # never hardcoded into the application or the reusable component.
            client.tool(Tool.CLICK, elementHandle=record('Source protégée')['handle'])
            client.tool(Tool.KEY, windowHandle=window, text=Key.END.value, eventType='PressAndRelease')
            synthetic_rows = [row for row in tree() if row.get('accessibleRole') == 'ListItem' and len(row.get('accessibleLabel', '')) == 32]
            if len(synthetic_rows) != 1:
                raise RuntimeError('One exact server-generated run row required')
            client.tool(Tool.CLICK, elementHandle=record(synthetic_rows[0]['accessibleLabel'])['handle'])
            client.tool(Tool.CLICK, elementHandle=record('Inspecter la trace')['handle'])
            record('480 événements · capture')
            capture('trace-start')
            record('#11 · Événements évincés')
            capture('trace-known-gap')
            record('#52 · Événements expurgés')
            capture('trace-redacted-gap')
            # Activate a fact row, then End: host navigation must reveal the
            # last row without constructing one delegate per captured event.
            first = record('#1 ·')
            client.tool(Tool.CLICK, elementHandle=first['handle'])
            client.tool(Tool.KEY, windowHandle=window, text=Key.END.value, eventType='PressAndRelease')
            record('#495 ·')
            event_delegates = [row for row in tree() if row.get('accessibleRole') == 'ListItem' and row.get('accessibleLabel', '').startswith('#')]
            if not event_delegates or len(event_delegates) > 40:
                raise RuntimeError('Trace delegates are not bounded at the final row')
            capture('trace-end-keyboard')
            # Reload the definition/run through the catalogue before mutation;
            # the frozen trace is never an authorization to cancel a run.
            tap('Logique et traces')
            client.tool(Tool.CLICK, elementHandle=record('Définition synthétique 01')['handle'])
            client.tool(Tool.CLICK, elementHandle=record(synthetic_rows[0]['accessibleLabel'])['handle'])
            ready_action('Annuler les étapes futures…')
            tap('Annuler les étapes futures…')
            capture('cancel-confirmation')
            tap('Confirmer une seule tentative')
            deadline = time.monotonic() + 20
            while not any('ne sont pas inversés' in row.get('accessibleLabel', '') for row in tree()):
                if time.monotonic() > deadline:
                    raise RuntimeError('Native cancellation did not preserve dispatched-effect semantics')
                time.sleep(.05)
            capture('canceled-future-only')
            tap('Fermer')
            tap('Logique et traces')
            client.tool(Tool.CLICK, elementHandle=record('Définition synthétique 01')['handle'])
            tap('Simuler une source fournie…')
            ready_action('Préparer un candidat et son aperçu')
            tap('Préparer un candidat et son aperçu')
            if field_value('Octets exacts fournis'):
                raise RuntimeError('Logic metadata inferred editable source bytes')
            tap('Script')
            fill('Identité logique', 'script.synthetic-native')
            fill('Fichier d’entrée', 'logic.yaml')
            fill('Chemin logique du fichier sélectionné', 'logic.yaml')
            fill('Octets exacts fournis', 'sequence:\n- action: light.turn_on\n  target: {entity_id: light.fixture}\n  data: {brightness_pct: 25}\n')
            fill('États et capacités synthétiques (JSON)', '[{"alias":"light.fixture","state":"off","supports":["light_turn_on","light_brightness_absolute"]}]')
            ready_action('Simuler avec les doubles fournis')
            tap('Simuler avec les doubles fournis')
            field_value('Intentions synthétiques', 'Luminosité 25 %')
            capture('simulation-separate-source')
            tap('Fermer')
            tap('Se déconnecter')
            reveal('Se connecter')
            print('Native logic: server catalogue, confirmed run/cancel, paged virtual trace and separate simulation passed ' + case.value, flush=True)
            subprocess.run([str(helper), '-'], input=json.dumps(dict(command=NativeCommand.CLOSE.value, pid=app.pid, syntheticOnly=True)), text=True, capture_output=True, check=True)
            if app.wait(timeout=15) != 0:
                raise RuntimeError('Native administration window did not close cleanly')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=25) != 0:
                raise RuntimeError('Configuration fixture did not join')
            isolation = json.loads((fixture / 'result.json').read_text())
            if not (isolation['configuration_unchanged'] and isolation['counts']['starts'] == 1 and isolation['counts']['cancellations'] == 1 and isolation['no_physical_providers'] and isolation['actual_run_joined']):
                raise RuntimeError('Native logic failed exact mutation counts or isolation')
            result = dict(isolation=isolation, status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', case=case.value,
                native_rendering=True, actual_rust_go_tls=True, mcp_input=True,
                production_configuration=False, real_devices=False, children_joined=True)
            result['trace_event_delegates_at_end'] = len(event_delegates)
            result['paged_events'] = 480
            result['keyboard_end'] = True
            SESSION['write'](directory / 'result.json', result)
            return result
        except BaseException:
            if app is not None and app.poll() is None and root is not None:
                with contextlib.suppress(Exception):
                    capture('failure')
            raise
        finally:
            SHARED['stop'](app)
            if server is not None and server.poll() is None:
                if fixture.is_dir():
                    (fixture / 'stop').touch(mode=0o600)
                with contextlib.suppress(subprocess.TimeoutExpired):
                    server.wait(timeout=30)
            SHARED['stop'](server)
            if namespace is not None and credential_owned:
                found = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True)
                if found.returncode == 0:
                    subprocess.run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, check=True)
                elif found.returncode != 44:
                    raise RuntimeError('Synthetic credential state could not be verified')
            if profile_owned and profile.exists():
                if private is None or json.loads(profile.read_text()) != SESSION['public_profile'](private):
                    raise RuntimeError('Preserve a profile changed outside this qualification')
                profile.unlink()
            if preferences_owned and preferences.exists():
                if json.loads(preferences.read_text()) != preference_value:
                    raise RuntimeError('Preserve preferences changed outside this qualification')
                preferences.unlink()
            with contextlib.suppress(FileNotFoundError):
                (fixture / 'ready.json').unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--reuse-build', type=Path, help='Reuse compiled artifacts only when all product and kit source hashes match')
    parser.add_argument('--matrix', action='store_true', help='Run cases from --case through the end of the matrix')
    parser.add_argument('--case', type=Case, choices=list(Case), default=Case.LIGHT_STANDARD)
    args = parser.parse_args()
    processes = subprocess.check_output(['ps', '-axo', 'comm'], text=True)
    if any(Path(row.strip()).name == 'homeward-desktop' for row in processes.splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf020' / ('native-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    SESSION['write'](proof / 'sources.json', before)
    server, helper = proof / 'administration.test', proof / 'native-ui'
    if args.reuse_build:
        previous = args.reuse_build.resolve(strict=True)
        expected = json.loads((previous / 'sources.json').read_text())
        # Only this Python UI driver may change: Rust, Slint, Go, schemas,
        # dependencies, other qualifiers and both kits remain byte-identical.
        def product_inventory(value):
            copied = json.loads(json.dumps(value))
            copied['homeward'].pop('scripts/qualify-frontend-logic-ui.py', None)
            return copied
        if product_inventory(expected) != product_inventory(before):
            raise RuntimeError('Reused native artifacts do not match current product/kit sources')
        events = [json.loads(line) for line in (previous / 'build.jsonl').read_text().splitlines() if line.startswith('{')]
        products = [event['executable'] for event in events if event.get('reason') == 'compiler-artifact'
                    and event.get('target', {}).get('name') == 'homeward-desktop' and event.get('executable')]
        if len(products) != 1:
            raise RuntimeError('One original Cargo-provided desktop executable required')
        original = previous / 'homeward-desktop'
        artifact = json.loads((previous / 'artifact.json').read_text())
        with original.open('rb') as input_file:
            if artifact != dict(original=products[0], sha256=hashlib.file_digest(input_file, 'sha256').hexdigest()):
                raise RuntimeError('Frozen native artifact differs from its build receipt')
        if original.stat().st_mtime_ns > (previous / 'build.jsonl').stat().st_mtime_ns:
            raise RuntimeError('Desktop artifact was rebuilt after the referenced Cargo invocation')
        binary = proof / 'homeward-desktop'
        artifacts = [(original, binary), (previous / 'administration.test', server), (previous / 'native-ui', helper)]
        hashes = {}
        for source, target in artifacts:
            os.link(source, target)
            with target.open('rb') as input_file:
                hashes[target.name] = hashlib.file_digest(input_file, 'sha256').hexdigest()
        (proof / 'build.jsonl').write_bytes((previous / 'build.jsonl').read_bytes())
        (proof / 'artifact.json').write_bytes((previous / 'artifact.json').read_bytes())
        SESSION['write'](proof / 'reuse.json', dict(original=str(previous), sources_equal_except_ui_driver=True, sha256=hashes))
        print('Reusing exact compiled product/Go/Swift artifacts; UI driver is the only changed source', flush=True)
    else:
        built = Path(MCP['build_native'](proof))
        binary = proof / 'homeward-desktop'
        os.link(built, binary)
        with binary.open('rb') as input_file:
            SESSION['write'](proof / 'artifact.json', dict(original=str(built), sha256=hashlib.file_digest(input_file, 'sha256').hexdigest()))
        subprocess.run(['go', 'test', '-c', '-o', str(server), './internal/server'], cwd=ROOT, check=True)
        subprocess.run(['xcrun', 'swiftc', '-module-cache-path', str(Path(config['frontend_cache']) / 'session-spike/swift-modules'),
            str(ROOT / 'qualification/frontend/native-ui.swift'), '-o', str(helper)], check=True)
    result = [run_case(proof, binary, server, helper, case) for case in (list(Case)[list(Case).index(args.case):] if args.matrix else [args.case])]
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Native proof inputs changed during the run')
    SESSION['write'](proof / 'result.json', result)
    print('Native logic MCP/TLS PASS ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
