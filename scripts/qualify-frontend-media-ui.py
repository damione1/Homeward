#!/usr/bin/env python3
"""Owned native macOS media UI on the explicitly synthetic Go TLS mock.

Run through frontend.py run --cargo. Captures require visual inspection; this
does not qualify Android, a screen reader or production media devices.
"""
import argparse
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import platform
import runpy
import signal
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


class Control(Enum):
    INSPECT = 'inspect-actions'
    CONFIRM = 'confirm'
    DENY_ARTWORK = 'deny-artwork'
    EXPIRE_ARTWORK = 'expire-artwork'
    ABSENT_ARTWORK = 'absent-artwork'
    REVOKE_READ = 'revoke-read'
    ALLOW_READ = 'allow-read'


class Key(Enum):
    HOME = 'home'
    END = 'end'
    ENTER = 'enter'
    SHIFT_RIGHT = 'shift-right'
    ESCAPE = 'escape'


def run_case(proof, build, case):
    appearance, text = case.preferences()
    directory = proof / case.value
    directory.mkdir(mode=0o700)
    source = ROOT / 'qualification/frontend/media'
    fixture = directory / 'fixture'
    product = Path.home() / 'Library/Application Support/Homeward'
    public, preferences = product / 'endpoint.json', product / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve existing Homeward settings')
    if any(Path(p.strip()).name == 'homeward-desktop' for p in subprocess.check_output(['ps', '-axo', 'comm'], text=True).splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    server = app = private = namespace = None
    sequence = 0
    confirmed = set()

    def credential_present():
        result = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, timeout=10)
        if result.returncode not in (0, 44):
            raise RuntimeError('Unverified synthetic credential state')
        return result.returncode == 0

    def native(command, **fields):
        if not isinstance(command, Command):
            raise TypeError('Native command requires its enum')
        deadline = time.monotonic() + 5
        while True:
            result = subprocess.run([build['helper'], '-'], input=json.dumps(dict(pid=app.pid, command=command.value, syntheticOnly=True, **fields)), capture_output=True, text=True, timeout=10)
            if result.returncode == 0:
                break
            with (directory / 'native-errors.jsonl').open('a') as output:
                output.write(json.dumps(dict(command=command.value, stderr=result.stderr)) + '\n')
            if command not in {Command.RESIZE, Command.INFO, Command.TREE} or app.poll() is not None or time.monotonic() > deadline:
                result.check_returncode()
            time.sleep(.2)
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
                raise RuntimeError('Native media label missing: ' + label)
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
        raise RuntimeError('Native media control is outside its viewport: ' + label)

    def tap(label):
        row = reveal(label, button=True)
        native(Command.TAP, label=row['title'] or row['description'])

    def key(value):
        if not isinstance(value, Key):
            raise TypeError('Native key requires its enum')
        native(Command.KEY, key=value.value)

    def capture(name):
        info = native(Command.INFO)
        subprocess.run(['screencapture', '-x', '-o', '-l', str(info['window_id']), str(directory / (name + '.png'))], check=True, capture_output=True)
        write(directory / (name + '.tree.json'), tree())
        write(directory / (name + '.window.json'), info)

    def control(action, command_id=None):
        nonlocal sequence
        if not isinstance(action, Control):
            raise TypeError('Typed media fixture control required')
        sequence += 1
        instruction = dict(sequence=sequence, action=action.value)
        if command_id is not None:
            instruction['command_id'] = command_id
        SHARED['acknowledge'](fixture, server, instruction)
        return sequence

    def confirm_action():
        reveal('Acquittée · confirmation en attente')
        current = control(Control.INSPECT)
        stats = json.loads((fixture / ('stats-%02d.json' % current)).read_text())
        records = [row['command_id'] for row in stats['records'] if row['command_id'] not in confirmed]
        if len(records) != 1:
            raise RuntimeError('Native action must have exactly one new synthetic record')
        control(Control.CONFIRM, records[0])
        confirmed.add(records[0])
        reveal('Effet confirmé')

    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            server = subprocess.Popen([build['server'], '-test.run=^TestNHF014MediaServer$', '-test.v', '-test.timeout=11m'],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF014_FIXTURE=str(fixture), HOMEWARD_NHF014_SYNTHETIC_ONLY='1', HOMEWARD_NHF014_SOURCES=str(source)), stdout=server_log, stderr=subprocess.STDOUT)
            private = wait_file(fixture / 'ready.json', server, timeout=240)
            origin = urlsplit(private['origin'])
            if not (private['mode'] == 'synthetic-only' and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port and private['production_media'] is False):
                raise RuntimeError('Explicit loopback media fixture required')
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
            players = json.loads((source / 'players.json').read_text())['players']
            minimal, group = players
            wait(minimal['alias'])
            reveal(minimal['alias'])
            native(Command.TAP, label=minimal['alias'])
            wait(minimal['title'])
            reveal('Lire', button=True)
            capture('minimal-player')
            tap('Lire')
            confirm_action()
            capture('minimal-confirmed')
            tap('Retour aux éléments')
            wait(group['alias'])
            reveal(group['alias'])
            native(Command.TAP, label=group['alias'])
            wait(group['title'])
            reveal(group['title'])
            capture('group-player')
            reveal('Volume')
            capture('group-controls')
            native(Command.SCROLL, delta=1000)
            native(Command.SCROLL, delta=1000)
            control(Control.DENY_ARTWORK)
            wait('Pochette non autorisée')
            reveal('Pochette non autorisée')
            capture('artwork-denied')
            control(Control.EXPIRE_ARTWORK)
            wait('Pochette expirée')
            control(Control.ABSENT_ARTWORK)
            wait('Pochette non fournie')
            control(Control.REVOKE_READ)
            wait('La vue a été invalidée')
            if has(group['title']):
                raise RuntimeError('Private media metadata survived READ revocation')
            capture('read-revoked')
            control(Control.ALLOW_READ)
            native(Command.READ)
            wait(group['alias'])
            reveal(group['alias'])
            native(Command.TAP, label=group['alias'])
            wait(group['title'])
            count = control(Control.INSPECT)
            stats = json.loads((fixture / ('stats-%02d.json' % count)).read_text())
            if stats['synthetic_submit_calls'] != 1 or stats['synthetic_mutations'] != 1:
                raise RuntimeError('Native read/reconnect replayed an action')
            native(Command.SIGN_OUT)
            wait('Se connecter')
            if credential_present():
                raise RuntimeError('Synthetic credential survived logout')
            native(Command.CLOSE)
            if app.wait(timeout=15) != 0:
                raise RuntimeError('Owned media app did not stop cleanly')
            (fixture / 'stop').touch(mode=0o600)
            if server.wait(timeout=30) != 0:
                raise RuntimeError('Owned media server did not stop cleanly')
            final = json.loads((fixture / 'result.json').read_text())
            if final != dict(server_joined=True, command_driver_entries=0, real_device_writes=0,
                             synthetic_submit_calls=1, synthetic_mutations=1, synthetic_action_records=1):
                raise RuntimeError('Media fixture did not close with exactly one fake action')
            result = dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', case=case.value,
                          minimal_action_confirmed=True, group_controls=True, artwork_fallbacks=True,
                          read_revocation_purges=True, reconnect_no_replay=True, server=final,
                          native_rendering=True, production_media=False, children_joined=True)
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
    args = parser.parse_args()
    if platform.system() != 'Darwin' or not os.environ.get('CARGO_TARGET_DIR'):
        raise SystemExit('Run on macOS through frontend.py run --cargo.')
    # The storage guard terminates our process group. Unwind run_case so only
    # this fixture's credential and temporary settings are removed as usual.
    def interrupted(_signum, _frame):
        raise KeyboardInterrupt('Owned native media qualification interrupted')
    signal.signal(signal.SIGTERM, interrupted)
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'media-ui' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    sources = SHARED['source_inventory'](config)
    write(proof / 'sources.json', sources)
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    build = NATIVE['build'](proof, config, 'Homeward Media')
    results = [run_case(proof, build, case) for case in args.case or list(Case)]
    if SHARED['source_inventory'](config) != sources:
        raise RuntimeError('Sources changed during native media qualification')
    write(proof / 'result.json', dict(cases=results, visual_inspection_required=True))
    print(json.dumps(dict(proof=str(proof), cases=results), indent=2))


if __name__ == '__main__':
    main()
