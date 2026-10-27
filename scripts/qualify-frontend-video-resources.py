#!/usr/bin/env python3
"""Measure the actual native video reader without compiling/enabling Slint MCP.

Run through frontend.py run --cargo. Native AX/pointer input addresses only an
owned macOS process and isolated Go TLS fixture. No real camera/Android claim.
"""
from enum import Enum
import contextlib
import json
import os
from pathlib import Path
import runpy
import statistics
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
NATIVE = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-recipes-ui.py'))

class Control(Enum):
    SNAPSHOT = 'snapshot'


class Command(Enum):
    INFO = 'info'
    TREE = 'tree'
    TAP = 'tap'
    SCROLL = 'scroll'
    LOGIN = 'fill-login'
    CLOSE = 'close'
    FOCUS = 'focus'


def qualify(proof, build):
    fixture = proof / 'fixture'
    product = Path.home() / 'Library/Application Support/Homeward'
    profile, preferences = product / 'endpoint.json', product / 'interface.json'
    if profile.exists() or preferences.exists():
        raise RuntimeError('Preserve existing Homeward settings')
    preference_value = dict(version='1', preferences=dict(appearance=NATIVE['Appearance'].LIGHT.value, surfaces='Opaque',
                                                         text=NATIVE['TextSize'].STANDARD.value, motion='Reduced'))
    server = app = private = namespace = None
    credential_owned = profile_owned = preferences_owned = False
    sequence = 0

    def native(command, **fields):
        if not isinstance(command, Command):
            raise TypeError('Typed native command required')
        result = subprocess.run([build['helper'], '-'], input=json.dumps(dict(
            command=command.value, pid=app.pid, syntheticOnly=True, **fields)),
            text=True, capture_output=True, timeout=10)
        if result.returncode:
            raise RuntimeError('Owned native input failed: ' + result.stderr)
        return json.loads(result.stdout) if result.stdout.strip() else None

    def tree():
        return native(Command.TREE)

    def matching(label):
        return [row for row in tree() if any(label in value for value in
            (row['title'], row['description'], row['text']))]

    def wait(label):
        deadline = time.monotonic() + 20
        while True:
            if matching(label):
                return
            if app.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError('Native video state missing: ' + label)
            time.sleep(.05)

    def reveal(label):
        for _ in range(24):
            found = [row for row in matching(label) if row['role'] == 'AXButton']
            bounds = native(Command.INFO)['bounds']
            if found:
                row = found[0]
                middle = row['y'] + row['height'] / 2
                if bounds['Y'] + 35 < middle < bounds['Y'] + bounds['Height'] - 35:
                    return row
                delta = 260 if middle <= bounds['Y'] + 35 else -260
            else:
                delta = -260
            # Address the outer pane's gutter, outside the nested camera list.
            native(Command.SCROLL, delta=delta,
                   x=bounds['X'] + bounds['Width'] - 24,
                   y=bounds['Y'] + bounds['Height'] * .6)
        raise RuntimeError('Native video control unreachable: ' + label)

    def tap(label):
        row = reveal(label)
        native(Command.TAP, label=row['title'] or row['description'])

    def capture(name):
        info = native(Command.INFO)
        subprocess.run(['screencapture', '-x', '-o', '-l', str(info['window_id']),
                        str(proof / (name + '.png'))], capture_output=True, check=True)
        SESSION['write'](proof / (name + '.tree.json'), tree())

    def sample():
        raw = subprocess.check_output(['ps', '-p', str(app.pid), '-o', 'rss=,time='], text=True).split()
        if len(raw) != 2:
            raise RuntimeError('One owned process resource sample required')
        seconds = 0.0
        for field in raw[1].split(':'):
            seconds = seconds * 60 + float(field)
        return dict(wall_seconds=time.monotonic(), rss_bytes=int(raw[0]) * 1024,
                    process_cpu_seconds=seconds)

    def measure():
        samples = [sample()]
        for _ in range(5):
            time.sleep(1)
            samples.append(sample())
        elapsed = samples[-1]['wall_seconds'] - samples[0]['wall_seconds']
        cpu = samples[-1]['process_cpu_seconds'] - samples[0]['process_cpu_seconds']
        return dict(samples=samples, mean_rss_bytes=statistics.mean(row['rss_bytes'] for row in samples),
                    process_cpu_percent=100 * cpu / elapsed, wall_seconds=elapsed)

    def closed_server():
        nonlocal sequence
        sequence += 1
        instruction = dict(sequence=sequence, action=Control.SNAPSHOT.value)
        SESSION['write'](fixture / 'control.writing', instruction)
        (fixture / 'control.writing').replace(fixture / 'control.json')
        ack = SHARED['wait_file'](fixture / ('ack-%02d.json' % sequence), server, timeout=10)
        if ack != instruction:
            raise RuntimeError('Resource fixture acknowledgement mismatch')
        stats = json.loads((fixture / ('stats-%02d.json' % sequence)).read_text())
        if stats['video_streams'] != 0 or stats['retained_leases'] > 64:
            raise RuntimeError('Closed video reader retained a live server stream')
        return stats

    def cycle(index):
        started = time.monotonic()
        tap('Ouvrir la vidéo')
        wait('Lecture native autorisée')
        opened = time.monotonic()
        tap('Revenir à l’instantané')
        wait('Image autorisée de la caméra')
        closed = time.monotonic()
        return dict(index=index, first_frame_observed_ms=1000 * (opened - started),
                    close_to_snapshot_ms=1000 * (closed - opened), **sample())

    with (proof / 'server.log').open('wb') as server_log, (proof / 'app.log').open('wb') as app_log:
        try:
            server = subprocess.Popen([build['server'], '-test.run=^TestNHF016CameraServer$',
                '-test.v', '-test.timeout=10m'], cwd=ROOT / 'internal/server',
                env=dict(os.environ, HOMEWARD_NHF016_SYNTHETIC_ONLY='1',
                    HOMEWARD_NHF016_FIXTURE=str(fixture),
                    HOMEWARD_NHF016_SOURCES=str(ROOT / 'qualification/frontend/cameras'),
                    HOMEWARD_NHF017_SYNTHETIC_ONLY='1',
                    HOMEWARD_NHF017_VIDEO=str(ROOT / 'qualification/frontend/video')),
                stdout=server_log, stderr=subprocess.STDOUT)
            private = SHARED['wait_file'](fixture / 'ready.json', server, timeout=120)
            origin = urlsplit(private['origin'])
            if not (private['mode'] == 'synthetic-only' and private['production_cameras'] is False
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port
                    and not origin.username and not origin.password):
                raise RuntimeError('Explicit isolated loopback video fixture required')
            namespace = SESSION['namespace'](private)
            present = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1',
                                      '-a', namespace], capture_output=True).returncode
            if present != 44:
                raise RuntimeError('Preserve existing or unverified credential namespace')
            credential_owned = True
            product.mkdir(parents=True, exist_ok=True)
            for path, value in [(profile, SESSION['public_profile'](private)), (preferences, preference_value)]:
                with path.open('x') as output:
                    path.chmod(0o600)
                    json.dump(value, output)
                    if path == profile:
                        profile_owned = True
                    else:
                        preferences_owned = True
            environment = dict(os.environ, SLINT_BACKEND='winit-software')
            environment.pop('SLINT_MCP_PORT', None)
            app = subprocess.Popen([build['binary']], cwd=ROOT, stdout=app_log,
                                   stderr=subprocess.STDOUT, env=environment)
            deadline = time.monotonic() + 20
            while True:
                try:
                    native(Command.FOCUS)
                    break
                except RuntimeError:
                    if app.poll() is not None or time.monotonic() > deadline:
                        raise
                    time.sleep(.1)
            wait('Se connecter')
            native(Command.LOGIN, loginName=private['reader']['login_name'], password=private['reader']['password'])
            wait('Session sécurisée active')
            if not matching('Caméras'):
                native(Command.TAP, label='Navigation')
            native(Command.TAP, label='Caméras')
            wait('Image autorisée de la caméra')
            capture('snapshot-baseline')
            listeners = subprocess.run(['lsof', '-nP', '-a', '-p', str(app.pid), '-iTCP', '-sTCP:LISTEN'],
                                       text=True, capture_output=True)
            if listeners.returncode != 1 or listeners.stdout.strip():
                raise RuntimeError('Resource build must have no MCP listener')
            metrics = dict(cold_snapshot=measure())
            tap('Ouvrir la vidéo')
            wait('Lecture native autorisée')
            capture('playing-first')
            time.sleep(.6)
            capture('playing-second')
            metrics['playing'] = measure()
            tap('Revenir à l’instantané')
            wait('Image autorisée de la caméra')
            for index in range(10):
                cycle(index + 1)
            metrics['warm_snapshot'] = measure()
            rows = []
            for index in range(100):
                row = cycle(index + 1)
                if (index + 1) % 10 == 0:
                    row['server'] = closed_server()
                    print('Native video without MCP: %d full readers closed' % (index + 1), flush=True)
                rows.append(row)
                SESSION['write'](proof / ('cycle-%03d.json' % (index + 1)), row)
            metrics['cycles'] = rows
            metrics['final_snapshot'] = measure()
            metrics['scope'] = dict(mcp=False, native_ax_input=True, renderer='winit-software',
                build='dev application; Slint rendering dependencies opt-level=2', decoder='VideoToolboxHardware', fps=10, dimensions=[320,180],
                first_frame_latency='Native pointer request until AX observes a decoded frame state',
                measurement_interpretation_required=True)
            metrics['first_frame_observed_median_ms'] = statistics.median(row['first_frame_observed_ms'] for row in rows)
            SESSION['write'](proof / 'resources.json', metrics)
            capture('after-100-cycles')
            native(Command.CLOSE)
            if app.wait(timeout=15) != 0:
                raise RuntimeError('Native resource window did not close cleanly')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('Resource fixture did not join')
            return dict(status='AUTOMATED_PASS_REQUIRES_RESOURCE_AND_VISUAL_INSPECTION',
                        cycles=100, native_rendering=True, actual_rust_go_tls=True,
                        mcp=False, children_joined=True, android=False, production_video=False)
        except BaseException:
            if app is not None and app.poll() is None:
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
    if any(Path(row.strip()).name == 'homeward-desktop' for row in
           subprocess.check_output(['ps', '-axo', 'comm'], text=True).splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf017' / ('native-resources-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    SESSION['write'](proof / 'sources.json', before)
    build = NATIVE['build'](proof, config, 'Homeward Video Resources')
    result = qualify(proof, build)
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Native resource proof inputs changed during the run')
    SESSION['write'](proof / 'result.json', result)
    print('Native video resources without MCP complete ' + str(proof), flush=True)

if __name__ == '__main__':
    main()
