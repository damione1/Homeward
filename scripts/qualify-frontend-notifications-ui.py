#!/usr/bin/env python3
"""Native notification inbox and private destinations through Slint MCP and an isolated Go TLS fixture.

Run through frontend.py run --cargo. Injected Slint events and native captures
are not physical touch, IME, Android or screen-reader qualification.
"""
import argparse
import base64
import contextlib
from enum import Enum
import json
import hashlib
import shutil
import os
import re
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


class Delivery(Enum):
    INBOX = 'inbox'
    NATIVE = 'native'

    def __str__(self):
        return self.value


class Key(Enum):
    END = '\uf72b'
    HOME = '\uf729'


class NativeCommand(Enum):
    FOCUS = 'focus'
    CLOSE = 'close'


def run_case(proof, binary, server_binary, helper, permission_helper, case, delivery):
    directory = proof / case.value
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    product = Path.home() / 'Library/Application Support/Homeward'
    profile, preferences = product / 'endpoint.json', product / 'interface.json'
    if profile.exists() or preferences.exists():
        raise RuntimeError('Preserve existing Homeward settings')
    server = app = private = namespace = permission_process = None
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

    def scroll(delta, position_y=None):
        pane = panel()
        if pane is None:
            # The pre-login surface fills the window.
            x, y, w, h = 0, 0, size['width'] / scale, size['height'] / scale
        else:
            position, extent = pane['absolutePosition'], pane['size']
            x, y, w, h = position.get('x', 0), position.get('y', 0), extent['width'], extent['height']
        client.tool(Tool.SCROLL, windowHandle=window,
            position=dict(x=x + w - 8, y=y + h / 2 if position_y is None else position_y), deltaX=0, deltaY=delta)
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

    def record(label, edge=Key.HOME):
        # Expose the table header before scrolling its virtualized list. The
        # table may be taller than the outer viewport at 200% text size.
        reveal('Ce compagnon', 'Table')
        for _ in range(8):
            table = next(row for row in tree() if row.get('accessibleRole') == 'Table' and row.get('accessibleLabel') == 'Ce compagnon')
            pane = panel()
            desired = pane['absolutePosition'].get('y', 0) + 8
            difference = desired - table['absolutePosition'].get('y', 0)
            if edge is Key.END:
                # At double text the table is taller than its outer viewport.
                # Expose its bottom for End, instead of aligning its header.
                desired = pane['absolutePosition'].get('y', 0) + pane['size']['height'] - 8
                difference = desired - table['absolutePosition'].get('y', 0) - table['size']['height']
            if abs(difference) < 12:
                break
            # Place the wheel outside the nested table. The viewport centre
            # can fall inside its ListView and scroll capabilities instead.
            outer_top = pane['absolutePosition'].get('y', 0)
            outer_bottom = outer_top + pane['size']['height']
            pointer_y = outer_top + 2 if table['absolutePosition'].get('y', 0) > outer_top else outer_bottom - 2
            scroll(difference, pointer_y)
            time.sleep(.04)
        for attempt in range(100):
            elements = tree()
            table = next(row for row in elements if row.get('accessibleRole') == 'Table' and row.get('accessibleLabel') == 'Ce compagnon')
            table_y = table['absolutePosition'].get('y', 0)
            inner = next(row for row in elements if any(value.get('id') == 'KitDataTable::list'
                         for value in row.get('typeNamesAndIds', []))
                         and table_y <= row['absolutePosition'].get('y', 0) < table_y + table['size']['height'])
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
                deltaX=0, deltaY=(10000 if edge is Key.HOME else -10000) if attempt == 0 else (-1 if edge is Key.END else 1) * -min(72, max(24, (bottom-top)/2)))
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

    def notice_elements():
        groups=[row for row in tree() if row.get('accessibleRole')=='Groupbox' and row.get('accessibleLabel')=='Notifications']
        return structured(client.tool(Tool.TREE,elementHandle=groups[0]['handle'],maxElements=1000))['elements'] if groups else []

    def notice_reveal(label,role='Button',message=None,enabled=False):
        reveal('Notifications','Text')
        deadline=time.monotonic()+25
        while time.monotonic()<deadline:
            elements=notice_elements()
            if not elements:
                # A new inbox can arrive between polls after the old card was
                # scrolled away. Recover its anchor instead of scrolling past it.
                reveal('Notifications','Text')
                elements=notice_elements()
            content=elements
            if message:
                items=[item for item in elements if item.get('accessibleRole')=='ListItem' and item.get('accessibleLabel')==message]
                content=structured(client.tool(Tool.TREE,elementHandle=items[0]['handle'],maxElements=100))['elements'] if items else []
            targets=[item for item in content if item.get('accessibleRole')==role and label in item.get('accessibleLabel','')]
            pane=panel(); top=pane['absolutePosition']['y']+8; bottom=top+pane['size']['height']-16
            lists=[item for item in elements if item.get('accessibleRole')=='List' and item.get('accessibleLabel')=='Notifications']
            if lists and (message or role=='ListItem'):
                # Expose the outer list before scrolling its inner viewport.
                # A partially clipped row has no mounted action descendants.
                difference=top+12-lists[0]['absolutePosition'].get('y',0)
                if abs(difference)>16:
                    scroll(max(-260,min(260,difference)))
                    time.sleep(.04)
                    continue
            target=targets[0] if targets else None
            if target:
                y=target['absolutePosition'].get('y',0)+target['size']['height']/2
                inside=top<y<bottom
                if (message or role=='ListItem') and lists:
                    listing=lists[0];inside=inside and listing['absolutePosition'].get('y',0)<y<listing['absolutePosition'].get('y',0)+listing['size']['height']
                if inside and (not enabled or target.get('accessibleEnabled',False)):
                    return target
                if not inside:
                    if top<y<bottom and lists:
                        listing=lists[0];box=listing['absolutePosition'];extent=listing['size']
                        client.tool(Tool.SCROLL,windowHandle=window,position=dict(x=box['x']+extent['width']/2,y=max(top,min(bottom,box.get('y',0)+extent['height']/2))),deltaX=0,deltaY=260 if y<box.get('y',0) else -260)
                    else:scroll(260 if y<top else -260)
            elif lists and (message or role=='ListItem'):
                listing=lists[0];box=listing['absolutePosition'];extent=listing['size']
                if max(top,box.get('y',0))<min(bottom,box.get('y',0)+extent['height']):
                    client.tool(Tool.SCROLL,windowHandle=window,position=dict(x=box['x']+extent['width']/2,y=(max(top,box.get('y',0))+min(bottom,box.get('y',0)+extent['height']))/2),deltaX=0,deltaY=260 if message=='Notification synthétique' else -260)
                else:scroll(-260)
            elif message or role=='ListItem':
                # Offscreen virtualized rows need the outer pane exposed first.
                scroll(-260)
            else:
                # Header controls also start outside the clipped card at 200%.
                scroll(-160)
            time.sleep(.05)
        raise RuntimeError('Notification control not reachable: '+label)

    def notice_tap(label,message=None):
        client.tool(Tool.CLICK,elementHandle=notice_reveal(label,message=message,enabled=True)['handle'])

    def sign_in(credentials):
        scroll(10000)
        fill('Identifiant', credentials['login_name'])
        fill('Mot de passe', credentials['password'])
        tap('Se connecter')

    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF022NotificationServer$',
                '-test.v', '-test.timeout=11m'], cwd=ROOT / 'internal/server',
                env=dict(os.environ, HOMEWARD_NHF022_SYNTHETIC_ONLY='1',
                    HOMEWARD_NHF022_FIXTURE=str(fixture)),
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

            sign_in(private['user'])
            wait('Session sécurisée active', timeout=30)
            settings()
            ready_action('Inscrire ce compagnon')
            tap('Inscrire ce compagnon'); tap('Confirmer')
            field_value('Modèle et OS')
            ready_action('Inscrire cette installation')
            capture('notification-unregistered')
            tap('Inscrire cette installation')
            reveal('La présentation native restera désactivée', 'Text')
            capture('notification-registration-confirmation')
            tap('Confirmer')
            reveal('Présentation native arrêtée', 'Text')
            capture('notification-registered-default-off')
            permission_requested=False
            if delivery is Delivery.NATIVE:
                if any('Autorisation macOS non demandée' in row.get('accessibleLabel','') for row in notice_elements()):
                    notice_tap('Demander l’autorisation macOS')
                    permission_process=subprocess.Popen([str(permission_helper)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
                    permission_process.stdin.write(json.dumps(dict(pid=app.pid,bundle=str(Path(binary).parents[2]),syntheticOnly=True)))
                    permission_process.stdin.close();permission_process.stdin=None
                    notice_tap('Confirmer');permission_requested=True
                    output,error=permission_process.communicate(timeout=12)
                    if permission_process.returncode!=0:raise RuntimeError('Owned notification permission helper failed: '+error)
                    permission_report=json.loads(output)
                    SESSION['write'](directory / 'os-permission.json',permission_report)
                    if permission_report['status']!='pressed':raise RuntimeError('Owned OS permission action not found; see os-permission.json')
                    notice_reveal('Autorisation macOS accordée','Text')
                # A read synchronizes the OS snapshot without enabling native delivery.
                notice_tap('Actualiser');time.sleep(.5)
                notice_tap('Activer les notifications natives');notice_tap('Confirmer')
                notice_reveal('Présentation native activée','Text')
                capture('notification-native-enabled')
            (fixture / 'seed-nominal').touch(mode=0o600)
            SHARED['wait_file'](fixture / 'seed-nominal-done.json', server)
            notice_reveal('Notification synthétique','ListItem')
            if delivery is Delivery.NATIVE:
                notice_reveal('Remise attestée au centre OS','Text',message='Notification synthétique')
                notice_reveal('Présentation au premier plan demandée','Text',message='Notification synthétique')
            capture('notification-inbox')
            notice_reveal('Remise fournisseur refusée','ListItem')
            capture('notification-provider-refused')
            notice_tap('Ouvrir',message='Notification synthétique')
            wait('Accueil','Text',timeout=20);settings()
            notice_tap('Marquer comme lue',message='Notification synthétique')
            notice_reveal('Lue',message='Notification synthétique')
            capture('notification-read')
            notice_tap('Retirer',message='Notification synthétique')
            deadline=time.monotonic()+20
            while any(row.get('accessibleRole')=='ListItem' and row.get('accessibleLabel')=='Notification synthétique' for row in notice_elements()):
                if time.monotonic()>deadline:raise RuntimeError('Dismissed message remained in the native inbox')
                time.sleep(.05)
            capture('notification-dismissed')
            if delivery is Delivery.NATIVE:
                notice_tap('Arrêter la présentation');notice_reveal('Présentation native arrêtée','Text')
                (fixture / 'seed-entity').touch(mode=0o600)
                SHARED['wait_file'](fixture / 'seed-entity-done.json',server)
                notice_reveal('Présentation non tentée','Text',message='Ressource synthétique')
                capture('notification-local-stop')
            notice_tap('Révoquer la destination')
            reveal('La présentation locale est déjà arrêtée', 'Text')
            capture('notification-revocation-confirmation')
            notice_tap('Confirmer')
            reveal('Destination révoquée', 'Text')
            capture('notification-revoked')
            tap('Se déconnecter');reveal('Se connecter')
            subprocess.run([str(helper), '-'], input=json.dumps(dict(command=NativeCommand.CLOSE.value, pid=app.pid, syntheticOnly=True)), text=True, capture_output=True, check=True)
            if app.wait(timeout=15) != 0:
                raise RuntimeError('Native notification window did not close cleanly')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=25) != 0:
                raise RuntimeError('Notification fixture did not join')
            isolation=json.loads((fixture / 'result.json').read_text())
            expected=dict(registrations=1,updates=isolation['counts']['updates'],receipts=8 if delivery is Delivery.NATIVE else 4,revocations=1,seeded=4 if delivery is Delivery.NATIVE else 3,opens=1,lost_replies=0)
            expected_updates=(2,3) if delivery is Delivery.NATIVE else (0,)
            if isolation['counts']['updates'] not in expected_updates:raise RuntimeError('Unexpected native permission/consent updates')
            if not (isolation['configuration_unchanged'] and isolation['counts']==expected
                    and isolation['companion_counts']==dict(registrations=1,consent_changes=0,publications=0,revocations=0,lost_replies=0)
                    and isolation['no_physical_providers'] and isolation['actual_run_joined']):
                raise RuntimeError('Native inbox exact mutations or isolation failed: '+str(isolation))
            result=dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION',case=case.value,isolation=isolation,
                native_rendering=True,actual_rust_go_tls=True,mcp_input=True,production_configuration=False,
                actual_os_notification_settings=True,permission_requested=permission_requested,native_notification_submitted=delivery is Delivery.NATIVE,
                actual_os_presentation=delivery is Delivery.NATIVE,foreground_delegate_observed=delivery is Delivery.NATIVE,delivery=delivery.value,real_devices=False,children_joined=True)
            SESSION['write'](directory / 'result.json',result)
            return result
        except BaseException:
            if app is not None and app.poll() is None and root is not None:
                with contextlib.suppress(Exception):
                    capture('failure')
            raise
        finally:
            SHARED['stop'](permission_process)
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
    parser.add_argument('--delivery',type=Delivery,choices=list(Delivery),default=Delivery.INBOX)
    parser.add_argument('--reuse-build', type=Path, help='Reuse compiled artifacts only when all product and kit source hashes match')
    parser.add_argument('--matrix', action='store_true', help='Run cases from --case through the end of the matrix')
    parser.add_argument('--case', type=Case, choices=list(Case), default=Case.LIGHT_STANDARD)
    args = parser.parse_args()
    processes = subprocess.check_output(['ps', '-axo', 'comm'], text=True)
    if any(Path(row.strip()).name == 'homeward-desktop' for row in processes.splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf022' / ('native-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    SESSION['write'](proof / 'sources.json', before)
    server, helper = proof / 'administration.test', proof / 'native-ui'
    permission_helper=proof / 'notification-permission'
    if args.reuse_build:
        previous = args.reuse_build.resolve(strict=True)
        expected = json.loads((previous / 'sources.json').read_text())
        # Only the two UI drivers may change: Rust, Slint, Go, schemas,
        # dependencies, other qualifiers and both kits remain byte-identical.
        def product_inventory(value):
            copied = json.loads(json.dumps(value))
            for path in ('scripts/qualify-frontend-notifications-ui.py','qualification/frontend/notification-permission.swift'):
                copied['homeward'].pop(path,None)
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
        SESSION['write'](proof / 'reuse.json', dict(original=str(previous), sources_equal_except_ui_drivers=True, sha256=hashes))
        permission_source='qualification/frontend/notification-permission.swift'
        if expected['homeward'].get(permission_source)==before['homeward'].get(permission_source):
            os.link(previous / 'notification-permission',permission_helper)
        print('Reusing exact product/Go/native-focus artifacts; only UI drivers may change',flush=True)
    else:
        built = Path(MCP['build_native'](proof))
        binary = proof / 'homeward-desktop'
        os.link(built, binary)
        with binary.open('rb') as input_file:
            SESSION['write'](proof / 'artifact.json', dict(original=str(built), sha256=hashlib.file_digest(input_file, 'sha256').hexdigest()))
        subprocess.run(['go', 'test', '-race', '-c', '-o', str(server), './internal/server'], cwd=ROOT, check=True)
        subprocess.run(['xcrun', 'swiftc', '-module-cache-path', str(Path(config['frontend_cache']) / 'session-spike/swift-modules'),
            str(ROOT / 'qualification/frontend/native-ui.swift'), '-o', str(helper)], check=True)
    if not permission_helper.exists():
        subprocess.run(['xcrun','swiftc','-module-cache-path',str(Path(config['frontend_cache']) / 'session-spike/swift-modules'),
            str(ROOT / 'qualification/frontend/notification-permission.swift'),'-o',str(permission_helper)],check=True)
    import plistlib
    bundle=proof / 'Homeward Notification Qualification.app'
    executable=bundle / 'Contents/MacOS/homeward-desktop'
    executable.parent.mkdir(parents=True)
    shutil.copy2(binary,executable)
    display_name='Homeward Notification Qualification '+str(time.time_ns())
    with (bundle / 'Contents/Info.plist').open('wb') as stream:
        plistlib.dump(dict(CFBundleExecutable='homeward-desktop',CFBundleIdentifier='ca.homeward.qualification.notifications.'+str(time.time_ns()),
            CFBundleName=display_name,CFBundleDisplayName=display_name,
            CFBundlePackageType='APPL',CFBundleVersion='1',NSHighResolutionCapable=True),stream)
    # Bind the development signature to this private bundle's identifier and
    # Info.plist. Never mutate the frozen Cargo executable through a hard link.
    subprocess.run(['codesign','--force','--sign','-',str(bundle)],check=True)
    subprocess.run(['codesign','--verify','--strict',str(bundle)],check=True)
    signature=subprocess.run(['codesign','-dvv',str(bundle)],capture_output=True,text=True,check=True)
    SESSION['write'](proof / 'bundle-signature.json',dict(kind='ad-hoc-local-only',unsigned_sha256=SHARED['sha'](binary),
        signed_sha256=SHARED['sha'](executable),details=signature.stderr,notarized=False))
    registration='/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister'
    subprocess.run([registration,'-f',str(bundle)],check=True)
    result = [run_case(proof, executable, server, helper, permission_helper, case,args.delivery) for case in (list(Case)[list(Case).index(args.case):] if args.matrix else [args.case])]
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Native proof inputs changed during the run')
    SESSION['write'](proof / 'result.json', result)
    print('Native notification inbox MCP/TLS PASS ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
