#!/usr/bin/env python3
"""Opt-in native macOS Lists proof over the real isolated ListService.

Screenshots require visual inspection; native AX row counts are not delegate
allocation measurements. The separate Pro tests measure bounded delegates.
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
LISTS = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-lists.py'))
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
Case, Command, Control = NATIVE['Case'], NATIVE['Command'], LISTS['Control']
write, sha, wait_file, stop = SESSION['write'], LISTS['sha'], LISTS['wait_file'], LISTS['stop']


class Key(Enum):
    END = 'end'
    HOME = 'home'
    ENTER = 'enter'
    SELECT_ALL = 'select-all'
    ONE = 'digit-1'
    TWO = 'digit-2'
    THREE = 'digit-3'
    FOUR = 'digit-4'
    FIVE = 'digit-5'
    SIX = 'digit-6'


class Mutation(Enum):
    CREATE = 'Créer la liste'
    RENAME = 'Renommer'
    ADD = 'Ajouter'
    SAVE = 'Enregistrer'
    MOVE_DOWN = 'Descendre'
    DELETE_ITEM = 'Supprimer'
    DELETE_LIST = 'Supprimer la liste'

    def ready_action(self):
        match self:
            case Mutation.DELETE_ITEM:
                return Mutation.ADD
            case Mutation.DELETE_LIST:
                return Mutation.CREATE
            case Mutation.CREATE | Mutation.RENAME | Mutation.ADD | Mutation.SAVE | Mutation.MOVE_DOWN:
                return self
        raise AssertionError('Unadmitted native list mutation')


def run_case(proof, build, case):
    directory = proof / case.value
    directory.mkdir(mode=0o700)
    fixture = directory / 'fixture'
    product = Path.home() / 'Library/Application Support/Homeward'
    public, preferences = product / 'endpoint.json', product / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve existing Homeward settings')
    if any(Path(p.strip()).name == 'homeward-desktop' for p in subprocess.check_output(
            ['ps', '-axo', 'comm'], text=True).splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    server = app = private = namespace = None
    sequence, captures = 0, []
    max_exposed_rows = 0
    gutter_x = None

    def credential_present():
        result = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1',
                                 '-a', namespace], capture_output=True, timeout=10)
        assert result.returncode in (0, 44)
        return result.returncode == 0

    def native(command, **fields):
        assert isinstance(command, Command)
        result = subprocess.run([build['helper'], '-'], input=json.dumps(dict(
            pid=app.pid, command=command.value, syntheticOnly=True, **fields)),
            capture_output=True, text=True, timeout=10)
        result.check_returncode()
        return json.loads(result.stdout) if result.stdout.strip() else None

    def tree():
        return native(Command.TREE)

    def labels(row):
        return (row['title'], row['description'], row['text'])

    def has(label):
        return any(label in value for row in tree() for value in labels(row))

    def until(label, predicate, timeout=30):
        deadline = time.monotonic() + timeout
        while not predicate():
            if app.poll() is not None or time.monotonic() > deadline:
                write(directory / 'failed-wait.tree.json', tree())
                raise RuntimeError('Native Lists wait failed: ' + label)
            time.sleep(.1)

    def wait(label, absent=False):
        until(label, lambda: has(label) != absent)

    def enabled(label):
        return any(row['role'] == 'AXButton' and row['title'] == label and row['enabled'] for row in tree())

    def tap(label):
        reveal(label)
        native(Command.TAP, label=label)

    def reveal(label):
        nonlocal gutter_x
        deadline = time.monotonic() + 30
        attempt = 0
        while True:
            values, bounds = tree(), native(Command.INFO)['bounds']
            targets = [row for row in values if row['title'] == label and row['width'] > 5 and row['height'] > 5]
            if targets:
                target = targets[0]
                if bounds['Y'] + 30 <= target['y'] and target['y'] + target['height'] <= bounds['Y'] + bounds['Height'] - 12:
                    return target
                delta = 300 if target['y'] < bounds['Y'] + 30 else -300
            else:
                delta = 300 if attempt % 20 < 10 else -300
            if time.monotonic() > deadline:
                raise RuntimeError('Native page could not reveal: ' + label)
            catalog = [row for row in values if row['role'] == 'AXList' and row['title'] == 'Listes synchronisées']
            # The page gutter lies outside both nested collection viewports.
            # Wheel events here scroll the page, preserving task selection.
            if catalog:
                gutter_x = catalog[0]['x'] - 8
            x = gutter_x if gutter_x is not None else bounds['X'] + 20
            native(Command.SCROLL, delta=delta, x=x, y=bounds['Y'] + bounds['Height'] / 2)
            attempt += 1

    def key(value):
        assert isinstance(value, Key)
        native(Command.KEY, key=value.value)

    def fill(label, value):
        tap(label)
        key(Key.SELECT_ALL)
        for digit in value:
            key(digit)

    def control(action):
        nonlocal sequence
        assert isinstance(action, Control)
        sequence += 1
        command = dict(sequence=sequence, action=action.value)
        LISTS['SHARED']['acknowledge'](fixture, server, command)
        if action is Control.SNAPSHOT:
            return wait_file(fixture / f'stats-{sequence:02d}.json', server)
        return None

    def capture(name):
        nonlocal max_exposed_rows
        info, rows = native(Command.INFO), tree()
        rename = [row for row in rows if row['role'] == 'AXTextField' and row['title'] == 'Renommer la liste']
        add_label = [row for row in rows if row['role'] == 'AXStaticText' and row['text'] == 'Nouvel élément']
        if rename and add_label:
            assert rename[0]['y'] + rename[0]['height'] <= add_label[0]['y'], 'Header fields overlap at this text scale'
        exposed = sum(row['role'] in ('AXRow', 'AXCell', 'AXStaticText') and any(
            value.startswith('Tâche synthétique ') for value in labels(row)) for row in rows)
        max_exposed_rows = max(max_exposed_rows, exposed)
        assert exposed <= 40, 'Native accessibility tree materialized the long collection'
        path = directory / (name + '.png')
        subprocess.run(['screencapture', '-x', '-o', '-l', str(info['window_id']), str(path)],
                       check=True, capture_output=True)
        write(directory / (name + '.tree.json'), rows)
        write(directory / (name + '.window.json'), info)
        captures.append(dict(name=name, sha256=sha(path), exposed_task_rows=exposed))

    def choose_list(title):
        deadline, attempt = time.monotonic() + 30, 0
        while True:
            values = tree()
            candidates = [row for row in values if row['role'] in ('AXRow', 'AXCell', 'AXStaticText')
                          and row['title'].startswith(title + ',')]
            catalog = [row for row in values if row['role'] == 'AXList' and row['title'] == 'Listes synchronisées']
            assert len(catalog) == 1
            box = catalog[0]
            if len(candidates) == 1:
                row = candidates[0]
                bounds = native(Command.INFO)['bounds']
                top = max(row['y'], box['y'], bounds['Y'] + 30)
                bottom = min(row['y'] + row['height'], box['y'] + box['height'], bounds['Y'] + bounds['Height'] - 12)
                if bottom - top >= min(row['height'], box['height']) - 1:
                    native(Command.TAP, label=row['title'], x=row['x'] + row['width'] / 2, y=(top + bottom) / 2)
                    break
            if time.monotonic() > deadline:
                raise RuntimeError('One visible native catalog identity expected: ' + title)
            native(Command.SCROLL, delta=-250 if attempt % 20 < 10 else 250,
                   x=box['x'] + box['width'] / 2, y=box['y'] + box['height'] / 2)
            attempt += 1
        until('selected list ready', lambda: enabled('Ajouter'))

    def mutation(action, count):
        assert isinstance(action, Mutation)
        button = action.value
        until(button + ' ready', lambda: enabled(button))
        tap(button)
        until('mutation admitted', lambda: control(Control.SNAPSHOT)['mutation_requests'] == count)
        wait('Modification confirmée par le serveur')
        next_button = action.ready_action().value
        reveal(next_button)
        until('new list capture', lambda: enabled(next_button))

    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            server = subprocess.Popen([build['server'], '-test.run=^TestNHF011ListsServer$',
                                       '-test.v', '-test.timeout=9m'], cwd=ROOT,
                                      env=dict(os.environ, HOMEWARD_NHF011_FIXTURE=str(fixture),
                                               HOMEWARD_NHF011_SYNTHETIC_ONLY='1'),
                                      stdout=server_log, stderr=subprocess.STDOUT)
            private = wait_file(fixture / 'ready.json', server)
            origin = urlsplit(private['origin'])
            assert private['mode'] == 'synthetic-only' and origin.scheme == 'https'
            assert origin.hostname == '127.0.0.1' and origin.port and private['seed_item_count'] == 10000
            namespace = SESSION['namespace'](private)
            if credential_present():
                raise RuntimeError('Preserve existing synthetic credential')
            appearance, text = case.preferences()
            product.mkdir(parents=True, exist_ok=True)
            write(public, SESSION['public_profile'](private))
            write(preferences, dict(version='1', preferences=dict(
                appearance=appearance.value, surfaces='Opaque', text=text.value, motion='Reduced')))
            app = subprocess.Popen([build['binary']], cwd=ROOT, env=dict(os.environ, SLINT_BACKEND='winit-software'),
                                   stdout=app_log, stderr=subprocess.STDOUT)
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
            native(Command.FILL, loginName=private['admin']['login_name'], password=private['admin']['password'])
            wait('Session sécurisée active')
            if enabled('Navigation'):
                tap('Navigation')
            wait('Listes')
            tap('Listes')
            wait('Liste synthétique longue')
            capture('catalog')
            fill('Nouvelle liste', [Key.ONE] * 3)
            mutation(Mutation.CREATE, 1)
            choose_list('111')
            capture('selected-list')
            fill('Renommer la liste', [Key.TWO] * 3)
            mutation(Mutation.RENAME, 2)
            wait('222')
            fill('Nouvel élément', [Key.THREE] * 3)
            mutation(Mutation.ADD, 3)
            wait('333')
            fill('Nouvel élément', [Key.FOUR] * 3)
            mutation(Mutation.ADD, 4)
            wait('2 éléments')
            tap('Modifier')
            fill('Élément', [Key.FIVE] * 3)
            mutation(Mutation.SAVE, 5)
            wait('555')
            mutation(Mutation.MOVE_DOWN, 6)
            capture('moved')
            # Refocus the same selected editor after the row moved, then edit
            # again. The unique changed summary must belong to the moved row.
            fill('Élément', [Key.SIX] * 3)
            mutation(Mutation.SAVE, 7)
            wait('666')
            wait('555', absent=True)
            capture('edited-after-move')
            mutation(Mutation.DELETE_ITEM, 8)
            wait('666', absent=True)
            assert not any(row['role'] == 'AXTextField' and row['focused'] for row in tree())
            mutation(Mutation.DELETE_LIST, 9)
            wait('222', absent=True)
            choose_list('Liste synthétique longue')
            wait('10000 éléments')
            tap('Modifier')
            key(Key.END)
            wait('Tâche synthétique 10000')
            reveal('Tâche synthétique 10000')
            last = [row for row in tree() if row['title'] == 'Tâche synthétique 10000' and row['role'] == 'AXStaticText']
            editor_label = [row for row in tree() if row['text'] == 'Élément' and row['role'] == 'AXStaticText']
            assert len(last) == len(editor_label) == 1
            assert last[0]['y'] + last[0]['height'] <= editor_label[0]['y'], 'Selected last task is clipped by the editor'
            capture('last-item')
            key(Key.HOME)
            wait('Tâche synthétique 00001')
            reveal('Tâche synthétique 00001')
            capture('first-item')
            if case is Case.LIGHT_STANDARD:
                before = control(Control.SNAPSHOT)['mutation_requests']
                control(Control.OUTAGE)
                wait('Hors ligne · cache du')
                capture('offline')
                assert not enabled('Ajouter') and not enabled('Enregistrer')
                tap('Ajouter')
                assert control(Control.SNAPSHOT)['mutation_requests'] == before
                control(Control.RESTORE)
                until('online after network restoration', lambda: enabled('Ajouter'))
                wait('Hors ligne · cache du', absent=True)
                assert control(Control.SNAPSHOT)['mutation_requests'] == before
                capture('reconnected')
            native(Command.SIGN_OUT)
            wait('Se connecter')
            assert not credential_present()
            native(Command.CLOSE)
            assert app.wait(timeout=15) == 0
            final = control(Control.SNAPSHOT)
            assert final == dict(mutation_requests=9, active_streams=0, action_rpc_requests=0,
                                 device_drivers=0, seeded_items=10000)
            (fixture / 'stop').touch(mode=0o600)
            assert server.wait(timeout=30) == 0
            assert 1 <= max_exposed_rows <= 40
            result = dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', case=case.value,
                          captures=captures, max_exposed_task_rows=max_exposed_rows, server=final,
                          children_joined=True, native_backend='macOS winit-software')
            write(directory / 'result.json', result)
            return dict(proof=str(directory), case=case.value, result_sha256=sha(directory / 'result.json'))
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
                        subprocess.run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1',
                                        '-a', namespace], capture_output=True, check=True)
                    if public.exists():
                        assert private is not None and json.loads(public.read_text()) == SESSION['public_profile'](private)
                        public.unlink()
                    if preferences.exists():
                        preferences.unlink()
                    with contextlib.suppress(FileNotFoundError):
                        (fixture / 'ready.json').unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', type=Case, choices=list(Case), action='append')
    args = parser.parse_args()
    if platform.system() != 'Darwin' or not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run on macOS through frontend.py run --cargo; Android is final-stage')
    config = SESSION['CONFIG']
    proof = Path(config['frontend_cache']) / 'lists-ui' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    print('NHF011 native proof: ' + str(proof), flush=True)
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    before = LISTS['SHARED']['source_inventory'](config)
    write(proof / 'sources.json', before)
    build = NATIVE['build'](proof, config, 'Homeward Lists')
    results = [run_case(proof, build, case) for case in args.case or list(Case)]
    if before != LISTS['SHARED']['source_inventory'](config):
        raise RuntimeError('Sources changed during native qualification')
    write(proof / 'result.json', dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', results=results))
    print('NHF011 native automated checks PASS; inspect captures: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
