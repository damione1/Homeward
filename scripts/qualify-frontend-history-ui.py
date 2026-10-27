#!/usr/bin/env python3
"""Owned native macOS history UI on the ordinary isolated TLS/Recorder server.

Run through frontend.py run --cargo. Captures require visual inspection; this
does not qualify Android, a screen reader or production monetary calculations.
"""
import argparse
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import platform
import runpy
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
NATIVE = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-recipes-ui.py'))
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
Case, write = NATIVE['Case'], SESSION['write']
stop, wait_file = SHARED['stop'], SHARED['wait_file']


class Command(Enum):
    INFO = 'info'
    TREE = 'tree'
    FILL = 'fill-login'
    RESIZE = 'resize'
    TAP = 'tap'
    READ = 'read'
    KEY = 'key'
    SCROLL = 'scroll'
    CLOSE = 'close'
    SIGN_OUT = 'sign-out'
    CANVAS = 'canvas'


class Key(Enum):
    HOME = 'home'
    END = 'end'
    ENTER = 'enter'
    SHIFT_RIGHT = 'shift-right'
    ESCAPE = 'escape'


def run_case(proof, build, case, costs_mock=False):
    appearance, text = case.preferences()
    directory = proof / case.value
    directory.mkdir(mode=0o700)
    source = directory / 'source.json'
    configuration = json.loads((ROOT / 'qualification/frontend/history/source.json').read_text())
    configuration['costs_mock'] = costs_mock
    source.write_text(json.dumps(configuration))
    fixture = directory / 'fixture'
    product = Path.home() / 'Library/Application Support/Homeward'
    public, preferences = product / 'endpoint.json', product / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve existing Homeward settings')
    if any(Path(p.strip()).name == 'homeward-desktop' for p in subprocess.check_output(['ps', '-axo', 'comm'], text=True).splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    server = app = private = namespace = None

    def credential_present():
        result = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, timeout=10)
        if result.returncode not in (0, 44):
            raise RuntimeError('Unverified synthetic credential state')
        return result.returncode == 0

    def native(command, **fields):
        if not isinstance(command, Command):
            raise TypeError('Native command requires its enum')
        result = subprocess.run([build['helper'], '-'], input=json.dumps(dict(pid=app.pid, command=command.value, syntheticOnly=True, **fields)), capture_output=True, text=True, timeout=10)
        result.check_returncode()
        return json.loads(result.stdout) if result.stdout.strip() else None

    def tree():
        return native(Command.TREE)

    def has(label):
        return any(label in value for row in tree() for value in (row['title'], row['description'], row['text']))

    def wait(label, timeout=60):
        deadline = time.monotonic() + timeout
        while not has(label):
            if app.poll() is not None or time.monotonic() > deadline:
                write(directory / 'failed-wait.tree.json', tree())
                raise RuntimeError('Native history label missing: ' + label)
            time.sleep(.1)

    def reveal(label, button=False):
        deadline, attempt = time.monotonic() + 30, 0
        while time.monotonic() < deadline:
            bounds = native(Command.INFO)['bounds']
            rows = [row for row in tree() if any(label in value for value in (row['title'], row['description'], row['text']))
                    and (not button or row['role'] == 'AXButton') and row['width'] > 5 and row['height'] > 5]
            # The canvas AX label also includes the inspected value. Prefer
            # the actual text row when framing that value for a screenshot.
            text_rows = [row for row in rows if row['role'] == 'AXStaticText']
            if text_rows:
                rows = text_rows
            if rows:
                center = rows[0]['y'] + rows[0]['height'] / 2 - bounds['Y']
                if 35 < center < bounds['Height'] - 35:
                    return rows[0]
                native(Command.SCROLL, delta=-300 if center > 35 else 300)
            else:
                native(Command.SCROLL, delta=-300 if attempt % 30 < 15 else 300)
            attempt += 1
            time.sleep(.1)
        write(directory / 'failed-reveal.tree.json', tree())
        raise RuntimeError('Native history control is outside its viewport: ' + label)

    def tap(label):
        reveal(label, button=True)
        native(Command.TAP, label=label)

    def key(value):
        if not isinstance(value, Key):
            raise TypeError('Native key requires its enum')
        native(Command.KEY, key=value.value)

    def capture(name):
        info = native(Command.INFO)
        subprocess.run(['screencapture', '-x', '-o', '-l', str(info['window_id']), str(directory / (name + '.png'))], check=True, capture_output=True)
        write(directory / (name + '.tree.json'), tree())
        write(directory / (name + '.window.json'), info)

    def inspect_canvas():
        prefix = 'Historique · consommation et coûts serveur · '
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            candidates = [row for row in tree() if row['role'] == 'AXGroup'
                          and any(value.startswith(prefix) for value in (row['title'], row['description']))]
            if candidates:
                label = next(value for value in (candidates[0]['title'], candidates[0]['description']) if value.startswith(prefix))
                reveal(label)
                native(Command.CANVAS, label=label, canvasGesture='inspect', fraction=.95)
                return
            native(Command.SCROLL, delta=-250)
        raise RuntimeError('Native history canvas missing')

    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            server = subprocess.Popen([build['server'], '-test.run=^TestNHF013HistoryServer$', '-test.v', '-test.timeout=11m'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF013_FIXTURE=str(fixture), HOMEWARD_NHF013_SYNTHETIC_ONLY='1', HOMEWARD_NHF013_SOURCE=str(source)), stdout=server_log, stderr=subprocess.STDOUT)
            private = wait_file(fixture / 'ready.json', server, timeout=240)
            origin = urlsplit(private['origin'])
            if not (private['mode'] == 'synthetic-only' and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port and private['point_count'] == 20000 and private['costs_mock'] is costs_mock):
                raise RuntimeError('Explicit loopback history fixture required')
            namespace = SESSION['namespace'](private)
            if credential_present():
                raise RuntimeError('Preserve existing synthetic credential')
            product.mkdir(parents=True, exist_ok=True)
            write(public, SESSION['public_profile'](private))
            write(preferences, dict(version='1', preferences=dict(appearance=appearance.value, surfaces='Opaque', text=text.value, motion='Reduced')))
            app = subprocess.Popen([build['binary']], cwd=ROOT, env=dict(os.environ, SLINT_BACKEND='winit-software'), stdout=app_log, stderr=subprocess.STDOUT)
            deadline = time.monotonic() + 20
            while True:
                try:
                    native(Command.INFO)
                    break
                except subprocess.CalledProcessError:
                    if app.poll() is not None or time.monotonic() > deadline:
                        raise
                    time.sleep(.1)
            native(Command.RESIZE, width=1280, height=800)
            wait('Se connecter')
            native(Command.FILL, loginName=private['reader']['login_name'], password=private['reader']['password'])
            wait('Afficher ma maison')
            native(Command.READ)
            alias = json.loads(source.read_text())['alias']
            wait(alias)
            reveal(alias)
            native(Command.TAP,label=alias)
            wait('Retour aux éléments')
            capture('entity-detail')
            tap('Afficher / relire')
            points = 2 if costs_mock else 20000
            reveal(str(points) + ' points source')
            inspect_canvas()
            capture('history-loaded')
            key(Key.HOME)
            reveal('0.00 CAD · disponible' if costs_mock else '0 kWh · disponible')
            capture('original-zero')
            key(Key.END)
            reveal('123456789012345678901234567890.123456789 CAD · disponible' if costs_mock else '19.999 kWh · disponible')
            if costs_mock:
                reveal('MOCK coûts synthétiques')
            capture('original-last')
            key(Key.SHIFT_RIGHT)
            key(Key.ESCAPE)
            if not costs_mock:
                tap('Contribution')
                tap('Afficher / relire')
                reveal('Contribution serveur · [')
                inspect_canvas()
                capture('server-contribution')
            native(Command.SIGN_OUT)
            wait('Se connecter')
            if credential_present():
                raise RuntimeError('Synthetic credential survived logout')
            native(Command.CLOSE)
            if app.wait(timeout=15) != 0:
                raise RuntimeError('Owned history app did not stop cleanly')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('Owned history server did not stop cleanly')
            final = json.loads((fixture / 'result.json').read_text())
            if final != dict(server_joined=True, command_driver_entries=0, real_device_writes=0):
                raise RuntimeError('History fixture did not close without command execution')
            result = dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', case=case.value, points=points, original_zero=True, original_last=True, server_contribution=not costs_mock, costs_mock=costs_mock, server=final, native_rendering=True, production_costs=False, children_joined=True)
            write(directory / 'result.json', result)
            return dict(proof=str(directory), **result)
        except BaseException:
            if app is not None and app.poll() is None:
                with contextlib.suppress(Exception):
                    capture('failure')
            raise
        finally:
            try:
                stop(app)
            finally:
                try:
                    stop(server)
                finally:
                    if namespace is not None and credential_present():
                        subprocess.run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, check=True)
                    if public.exists():
                        if private is None or json.loads(public.read_text()) != SESSION['public_profile'](private):
                            raise RuntimeError('Public endpoint changed during native qualification')
                        public.unlink()
                    if preferences.exists():
                        preferences.unlink()
                    with contextlib.suppress(FileNotFoundError):
                        (fixture / 'ready.json').unlink()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--case', type=Case, choices=list(Case), action='append')
    parser.add_argument('--costs-mock', action='store_true')
    args = parser.parse_args()
    if platform.system() != 'Darwin' or not os.environ.get('CARGO_TARGET_DIR'):
        raise SystemExit('Run on macOS through frontend.py run --cargo.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / ('history-costs-ui' if args.costs_mock else 'history-ui') / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    sources = SHARED['source_inventory'](config)
    write(proof / 'sources.json', sources)
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    build = NATIVE['build'](proof, config, 'Homeward History')
    results = [run_case(proof, build, case, args.costs_mock) for case in args.case or list(Case)]
    if SHARED['source_inventory'](config) != sources:
        raise RuntimeError('Sources changed during native history qualification')
    write(proof / 'result.json', dict(cases=results, visual_inspection_required=True))
    print(json.dumps(dict(proof=str(proof), cases=results), indent=2))


if __name__ == '__main__':
    main()
