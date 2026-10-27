#!/usr/bin/env python3
"""Native H264 video UI through Slint MCP and the actual isolated Go TLS fixture.

Run through frontend.py run --cargo. Injected Slint events and native captures
are not physical touch, IME, Android or screen-reader qualification.
"""
import argparse
import base64
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import runpy
import socket
import statistics
import subprocess
import time
from urllib.error import URLError
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
MCP = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-mcp.py'))
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
NATIVE = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-recipes-ui.py'))
Tool, structured, Case = MCP['Tool'], MCP['structured'], NATIVE['Case']


class Control(Enum):
    VIDEO_UNSUPPORTED = 'video-unsupported'
    VIDEO_UNAVAILABLE = 'video-unavailable'
    VIDEO_AVAILABLE = 'video-available'
    SNAPSHOT = 'snapshot'
    EXPIRE = 'expire-leases'
    REVOKE = 'revoke-read'
    ALLOW = 'allow-read'
    MISSING = 'missing-image'
    RESTORE_IMAGES = 'restore-images'
    WRONG_CAMERA = 'wrong-camera'
    OUTAGE = 'outage'
    RESTORE_NETWORK = 'restore-network'


class Key(Enum):
    END = '\uf72b'
    HOME = '\uf729'


class NativeCommand(Enum):
    FOCUS = 'focus'
    CLOSE = 'close'


def run_case(proof, binary, server_binary, helper, case, resources=False):
    directory = proof / case.value
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    source = directory / 'sources'
    source.mkdir(mode=0o700)
    cameras = json.loads((ROOT / 'qualification/frontend/cameras/cameras.json').read_text())
    cameras['cameras'][0]['events'] = 3
    SESSION['write'](source / 'cameras.json', cameras)
    product = Path.home() / 'Library/Application Support/Homeward'
    profile, preferences = product / 'endpoint.json', product / 'interface.json'
    if profile.exists() or preferences.exists():
        raise RuntimeError('Preserve existing Homeward settings')
    server = app = private = namespace = None
    credential_owned = profile_owned = preferences_owned = False
    client = window = root = None
    sequence = 0
    appearance, text = case.preferences()
    preference_value = dict(version='1', preferences=dict(appearance=appearance.value,
        surfaces='Opaque', text=text.value, motion='Reduced'))

    def focus():
        deadline = time.monotonic() + 5
        while True:
            result = subprocess.run([str(helper), '-'], input=json.dumps(dict(
                command=NativeCommand.FOCUS.value, pid=app.pid, syntheticOnly=True)),
                text=True, capture_output=True, timeout=10)
            if result.returncode == 0:
                return
            with (directory / 'focus-errors.jsonl').open('a') as output:
                output.write(json.dumps(dict(code=result.returncode, stderr=result.stderr)) + '\n')
            if app.poll() is not None or time.monotonic() >= deadline:
                raise RuntimeError('Owned native camera focus failed: ' + result.stderr)
            time.sleep(.1)

    def tree():
        # MCP injects Slint events but does not activate the macOS application.
        # Match the ordinary native helper: keep the owned window exposed before
        # every interaction/read, so occlusion cannot retire the tested session.
        focus()
        return structured(client.tool(Tool.TREE, elementHandle=root, maxElements=1000))['elements']

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
                raise RuntimeError('Native camera surface missing: ' + label)
            time.sleep(.05)

    def scroll(delta):
        panels = [row for row in tree() if any(value.get('id') == 'HomewardWindow::cameras-pane'
                  for value in row.get('typeNamesAndIds', []))]
        if len(panels) != 1:
            raise RuntimeError('One camera scroll surface required')
        panel = panels[0]
        position, extent = panel['absolutePosition'], panel['size']
        client.tool(Tool.SCROLL, windowHandle=window,
            position=dict(x=position['x'] + extent['width'] - 8,
                          y=position['y'] + extent['height'] / 2),
            deltaX=0, deltaY=delta)
        return position['y'] + 8, position['y'] + extent['height'] - 8

    def reveal(label, role='Button'):
        top, bottom = 10, size['height'] / scale - 15
        panels = [row for row in tree() if any(value.get('id') == 'HomewardWindow::cameras-pane'
                  for value in row.get('typeNamesAndIds', []))]
        if panels:
            top = panels[0]['absolutePosition']['y'] + 8
            bottom = top + panels[0]['size']['height'] - 16
        for _ in range(24):
            found = rows(label, role)
            if found:
                row = found[0]
                y = row.get('absolutePosition', {}).get('y', 0)
                height = row.get('size', {}).get('height', 0)
                if top < y + height / 2 < bottom:
                    return row
                delta = 260 if y < top else -260
            else:
                delta = -260
            scroll(delta)
            time.sleep(.04)
        raise RuntimeError('Native camera control is unreachable: ' + label)

    def tap(label, role='Button'):
        row = reveal(label, role)
        client.tool(Tool.CLICK, elementHandle=row['handle'])

    def navigate(label):
        # Sidebar entries belong to the window, outside the camera scroll pane.
        if not rows(label, 'ListItem'):
            opener = wait('Navigation', 'Button')
            client.tool(Tool.CLICK, elementHandle=opener['handle'])
        row = wait(label, 'ListItem')
        client.tool(Tool.CLICK, elementHandle=row['handle'])

    def choose_camera(key, identity):
        if not isinstance(key, Key):
            raise TypeError('Typed catalog navigation required')
        row = reveal('Caméras autorisées', 'List')
        client.tool(Tool.CLICK, elementHandle=row['handle'])
        client.tool(Tool.KEY, windowHandle=window, text=key.value, eventType='PressAndRelease')
        deadline = time.monotonic() + 20
        while not any(row.get('accessibleRole') == 'Text' and row.get('accessibleLabel') == identity for row in tree()):
            if app.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('Exact selected camera missing: ' + identity)
            time.sleep(.05)
        wait('Image autorisée de la caméra', 'Image')

    def fill(label, value):
        row = reveal(label, 'TextInput')
        client.tool(Tool.CLICK, elementHandle=row['handle'])
        client.tool(Tool.VALUE, elementHandle=row['handle'], value=value)

    def capture(name):
        response = client.tool(Tool.SCREENSHOT, windowHandle=window)
        image = next(item for item in response['content'] if item.get('type') == 'image')
        (directory / (name + '.png')).write_bytes(base64.b64decode(image['data'], validate=True))
        # Never persist a credential even if a platform exposes an editable value.
        raw = json.dumps(tree(), ensure_ascii=False)
        if private is not None:
            raw = raw.replace(private['reader']['password'], '<redacted>')
        (directory / (name + '.tree.json')).write_text(raw)

    def private_images():
        return [row for row in tree() if row.get('computedOpacity', 1) > 0
            and row.get('size', {}).get('width', 0) >= 100
            and row.get('size', {}).get('height', 0) >= 100
            and any(value.get('typeName') == 'Image'
                    for value in row.get('typeNamesAndIds', []))]

    def expect_pixels(present):
        if len(private_images()) != (1 if present else 0):
            raise RuntimeError('Exactly one private image when playing, none after retirement')

    def control(action, entity_id=None):
        nonlocal sequence
        if not isinstance(action, Control):
            raise TypeError('Typed camera control required')
        sequence += 1
        instruction = dict(sequence=sequence, action=action.value)
        if entity_id is not None:
            instruction['entity_id'] = entity_id
        path = fixture / 'control.writing'
        SESSION['write'](path, instruction)
        path.replace(fixture / 'control.json')
        ack = SHARED['wait_file'](fixture / ('ack-%02d.json' % sequence), server, timeout=10)
        if ack != instruction:
            raise RuntimeError('Camera control acknowledgement mismatch')
        if action is Control.SNAPSHOT:
            return json.loads((fixture / ('stats-%02d.json' % sequence)).read_text())

    def sample_process():
        raw = subprocess.check_output(['ps', '-p', str(app.pid), '-o', 'rss=,time='], text=True).split()
        if len(raw) != 2:
            raise RuntimeError('One owned process resource sample required')
        seconds = 0.0
        for field in raw[1].split(':'):
            seconds = seconds * 60 + float(field)
        return dict(wall_seconds=time.monotonic(), rss_bytes=int(raw[0]) * 1024,
                    process_cpu_seconds=seconds)

    def measure_process():
        samples = [sample_process()]
        for _ in range(5):
            time.sleep(1)
            samples.append(sample_process())
        elapsed = samples[-1]['wall_seconds'] - samples[0]['wall_seconds']
        cpu = samples[-1]['process_cpu_seconds'] - samples[0]['process_cpu_seconds']
        return dict(samples=samples, mean_rss_bytes=statistics.mean(row['rss_bytes'] for row in samples),
                    process_cpu_percent=100 * cpu / elapsed, wall_seconds=elapsed)

    def reader_cycle(index):
        started = time.monotonic()
        tap('Ouvrir la vidéo')
        reveal('Lecture native autorisée', 'Image')
        wait('Lecture native autorisée', 'Image')
        expect_pixels(True)
        opened = time.monotonic()
        tap('Revenir à l’instantané')
        reveal('Image autorisée de la caméra', 'Image')
        wait('Image autorisée de la caméra', 'Image')
        closed = time.monotonic()
        return dict(index=index, first_frame_observed_ms=1000 * (opened - started),
                    close_to_snapshot_ms=1000 * (closed - opened), **sample_process())

    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            server = subprocess.Popen([str(server_binary), '-test.run=^TestNHF016CameraServer$',
                '-test.v', '-test.timeout=8m'], cwd=ROOT / 'internal/server',
                env=dict(os.environ, HOMEWARD_NHF016_SYNTHETIC_ONLY='1',
                    HOMEWARD_NHF016_FIXTURE=str(fixture), HOMEWARD_NHF016_SOURCES=str(source),
                    HOMEWARD_NHF017_SYNTHETIC_ONLY='1',
                    HOMEWARD_NHF017_VIDEO=str(ROOT / 'qualification/frontend/video')),
                stdout=server_log, stderr=subprocess.STDOUT)
            private = SHARED['wait_file'](fixture / 'ready.json', server, timeout=120)
            origin = urlsplit(private['origin'])
            if not (private['mode'] == 'synthetic-only' and private['production_cameras'] is False
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port):
                raise RuntimeError('Explicit isolated loopback camera fixture required')
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
                        clientInfo=dict(name='homeward-video-qualification', version='1')))
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
            fill('Identifiant', private['reader']['login_name'])
            fill('Mot de passe', private['reader']['password'])
            tap('Se connecter')
            wait('Session sécurisée active', timeout=30)
            print('Native camera: authenticated foreground session', flush=True)
            navigate('Caméras')
            wait('Caméras autorisées', timeout=30)
            choose_camera(Key.END, 'driveway')
            reveal('MOCK ISOLÉ', None)
            wait('Image autorisée de la caméra', 'Image', timeout=30)
            capture('snapshot-first')
            measurements = dict(cold_snapshot=measure_process()) if resources else None
            tap('Ouvrir la vidéo')
            reveal('Lecture native autorisée', 'Image')
            wait('Lecture native autorisée', 'Image')
            expect_pixels(True)
            capture('video-first')
            time.sleep(.65)
            capture('video-second')
            tap('Mettre en pause')
            reveal('Vidéo en pause', 'Image')
            expect_pixels(False)
            capture('paused')
            tap('Relancer la vidéo')
            reveal('Lecture native autorisée', 'Image')
            capture('resumed')
            control(Control.EXPIRE)
            reveal('Session vidéo expirée', 'Image')
            expect_pixels(False)
            capture('expired')
            tap('Relancer la vidéo')
            reveal('Lecture native autorisée', 'Image')
            capture('new-authorized-session')
            # Hide the picture while leaving the card's buttons on screen.
            # Keeping only the surrounding card visible must not keep decoding.
            surface = wait('Lecture native autorisée', 'Image')
            panels = [row for row in tree() if any(value.get('id') == 'HomewardWindow::cameras-pane'
                      for value in row.get('typeNamesAndIds', []))]
            bottom = surface['absolutePosition']['y'] + surface['size']['height']
            top = panels[0]['absolutePosition']['y']
            scroll(-(bottom - top + 20))
            time.sleep(.3)
            stopped = control(Control.SNAPSHOT)
            SESSION['write'](directory / 'outside-viewport-stats.json', stopped)
            if stopped['video_streams'] != 0:
                raise RuntimeError('Video transport continues while only card buttons are visible')
            expect_pixels(False)
            capture('outside-viewport')
            scroll(10000)
            reveal('Vidéo arrêtée hors écran', 'Image')
            time.sleep(.3)
            expect_pixels(False)
            tap('Relancer la vidéo')
            reveal('Lecture native autorisée', 'Image')
            expect_pixels(True)
            capture('viewport-resumed')
            control(Control.VIDEO_UNSUPPORTED)
            tap('Mettre en pause')
            tap('Relancer la vidéo')
            reveal('Vidéo ou décodeur natif indisponible', 'Image')
            expect_pixels(False)
            capture('unsupported')
            tap('Revenir à l’instantané')
            reveal('Image autorisée de la caméra', 'Image')
            capture('snapshot-fallback')
            control(Control.VIDEO_AVAILABLE)
            tap('Ouvrir la vidéo')
            reveal('Lecture native autorisée', 'Image')
            capture('restored')
            if resources:
                measurements['playing'] = measure_process()
                tap('Revenir à l’instantané')
                reveal('Image autorisée de la caméra', 'Image')
                for index in range(10):
                    reader_cycle(index + 1)
                measurements['warm_snapshot'] = measure_process()
                cycles = []
                for index in range(100):
                    cycles.append(reader_cycle(index + 1))
                    if (index + 1) % 10 == 0:
                        stats = control(Control.SNAPSHOT)
                        if stats['video_streams'] != 0 or stats['retained_leases'] > 64:
                            raise RuntimeError('A closed video reader retained its active server stream')
                        cycles[-1]['server'] = stats
                        print('Native video: %d full reader cycles closed' % (index + 1), flush=True)
                    SESSION['write'](directory / ('cycle-%03d.json' % (index + 1)), cycles[-1])
                SESSION['write'](directory / 'cycles.json', cycles)
                measurements['cycles'] = cycles
                measurements['final_snapshot'] = measure_process()
                measurements['first_frame_observed_median_ms'] = statistics.median(row['first_frame_observed_ms'] for row in cycles)
                measurements['first_frame_observed_max_ms'] = max(row['first_frame_observed_ms'] for row in cycles)
                measurements['scope'] = dict(app_pid=app.pid, build='dev-unoptimized',
                    renderer='winit-software', slint='1.18.1', decoder='VideoToolboxHardware',
                    dimensions=[320,180], fps=10, mcp_overhead_included=True,
                    first_frame_latency='UI request until MCP observes an actual decoded image',
                    measurement_interpretation_required=True)
                SESSION['write'](directory / 'resources.json', measurements)
                tap('Ouvrir la vidéo')
                reveal('Lecture native autorisée', 'Image')
            control(Control.VIDEO_UNAVAILABLE)
            tap('Mettre en pause')
            tap('Relancer la vidéo')
            reveal('Vidéo temporairement indisponible', 'Image')
            expect_pixels(False)
            capture('negotiation-unavailable')
            tap('Revenir à l’instantané')
            reveal('Image autorisée de la caméra', 'Image')
            control(Control.VIDEO_AVAILABLE)
            tap('Ouvrir la vidéo')
            reveal('Lecture native autorisée', 'Image')
            control(Control.OUTAGE)
            reveal('Connexion interrompue', None)
            expect_pixels(False)
            capture('offline')
            control(Control.RESTORE_NETWORK)
            tap('Relancer la vidéo')
            reveal('Lecture native autorisée', 'Image')
            control(Control.REVOKE)
            wait('Accès retiré', timeout=30)
            expect_pixels(False)
            if rows('Source synthétique A') or rows('event-00000'):
                raise RuntimeError('Private camera metadata survived revocation')
            capture('revoked')
            control(Control.ALLOW)
            tap('Relire les caméras')
            reveal('Image autorisée de la caméra', 'Image')
            tap('Ouvrir la vidéo')
            reveal('Lecture native autorisée', 'Image')
            navigate('Accueil')
            time.sleep(.3)
            expect_pixels(False)
            navigate('Caméras')
            reveal('Image autorisée de la caméra', 'Image')
            if rows('Lecture native autorisée', 'Image'):
                raise RuntimeError('Returning to the page silently resumed video')
            tap('Ouvrir la vidéo')
            reveal('Lecture native autorisée', 'Image')
            capture('foreground-new-session')
            subprocess.run([str(helper), '-'], input=json.dumps(dict(command=NativeCommand.CLOSE.value, pid=app.pid, syntheticOnly=True)), text=True, capture_output=True, check=True)
            if app.wait(timeout=15) != 0:
                raise RuntimeError('Native camera window did not close cleanly')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=25) != 0:
                raise RuntimeError('Camera fixture did not join')
            result = dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', case=case.value,
                native_rendering=True, actual_rust_go_tls=True, mcp_input=True,
                production_cameras=False, real_devices=False, children_joined=True,
                actual_h264_decode=True, audio=False, expiry=True, pause=True,
                snapshot_fallback=True, viewport=True, negotiation_failure=True,
                revocation=True, offline=True, foreground=True,
                full_reader_cycles=100 if resources else 0, android=False)
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
    parser.add_argument('--case', type=Case, choices=list(Case), default=Case.LIGHT_STANDARD)
    parser.add_argument('--all-cases', action='store_true')
    parser.add_argument('--resources', action='store_true', help='Measure resources and 100 full reader cycles')
    args = parser.parse_args()
    processes = subprocess.check_output(['ps', '-axo', 'comm'], text=True)
    if any(Path(row.strip()).name == 'homeward-desktop' for row in processes.splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf017' / ('native-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    SESSION['write'](proof / 'sources.json', before)
    binary = MCP['build_native'](proof)
    server, helper = proof / 'camera.test', proof / 'native-ui'
    subprocess.run(['go', 'test', '-c', '-o', str(server), './internal/server'], cwd=ROOT, check=True)
    subprocess.run(['xcrun', 'swiftc', '-module-cache-path', str(Path(config['frontend_cache']) / 'session-spike/swift-modules'),
        str(ROOT / 'qualification/frontend/native-ui.swift'), '-o', str(helper)], check=True)
    cases = list(Case) if args.all_cases else [args.case]
    result = dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_AND_RESOURCE_INSPECTION', cases=[
        run_case(proof, binary, server, helper, case,
                 resources=args.resources and case is cases[0]) for case in cases])
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Native proof inputs changed during the run')
    SESSION['write'](proof / 'result.json', result)
    print('Native Video MCP/TLS PASS ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
