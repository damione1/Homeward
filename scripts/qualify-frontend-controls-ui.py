#!/usr/bin/env python3
"""NHF008 native desktop gestures through isolated Go/TLS and real helper SQL.

Run through frontend.py run --cargo. This test executable has no physical driver,
provider credentials or production origin. Android qualification is final-stage.
"""
import argparse
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import plistlib
import runpy
import shutil
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
run, write, digest = SESSION['run'], SESSION['write'], SESSION['digest']


class Scenario(Enum):
    NOMINAL = 'nominal'
    SQL_CONFLICT = 'sql-conflict'
    CAPABILITY_WITHDRAWAL = 'capability-withdrawal'
    WRITE_WITHDRAWAL = 'write-withdrawal'
    OBSERVATION_REMOVAL = 'observation-removal'
    CLIMATE_RANGE = 'climate-range'
    FAN = 'fan'
    COLOR = 'color'
    TEXT_CIVIL = 'text-civil'
    COUNTER_SELECT = 'counter-select'
    NUMBER = 'number'
    NUMBER_CONFLICT = 'number-conflict'
    THEMES = 'themes'

    def fixture(self):
        return {
            Scenario.NOMINAL: Fixture.CONTROLS, Scenario.SQL_CONFLICT: Fixture.CONTROLS,
            Scenario.CAPABILITY_WITHDRAWAL: Fixture.CONTROLS, Scenario.WRITE_WITHDRAWAL: Fixture.CONTROLS,
            Scenario.OBSERVATION_REMOVAL: Fixture.CONTROLS, Scenario.CLIMATE_RANGE: Fixture.CONTROLS,
            Scenario.FAN: Fixture.CONTROLS, Scenario.COLOR: Fixture.CONTROLS,
            Scenario.TEXT_CIVIL: Fixture.CONTROLS, Scenario.COUNTER_SELECT: Fixture.CONTROLS,
            Scenario.NUMBER: Fixture.NUMBER, Scenario.NUMBER_CONFLICT: Fixture.NUMBER,
            Scenario.THEMES: Fixture.CONTROLS,
        }[self]

    def counts(self):
        # Expected Submit calls, then physical fake-driver entries. Helpers use
        # the actual local SQL driver, never the fake physical driver.
        return {
            Scenario.NOMINAL: (2,1), Scenario.SQL_CONFLICT: (1,0),
            Scenario.CAPABILITY_WITHDRAWAL: (0,0), Scenario.WRITE_WITHDRAWAL: (0,0),
            Scenario.OBSERVATION_REMOVAL: (0,0), Scenario.CLIMATE_RANGE: (3,3),
            Scenario.FAN: (2,2), Scenario.COLOR: (1,1), Scenario.TEXT_CIVIL: (4,0),
            Scenario.COUNTER_SELECT: (3,0), Scenario.NUMBER: (3,0),
            Scenario.NUMBER_CONFLICT: (1,0), Scenario.THEMES: (0,0),
        }[self]


class Fixture(Enum):
    CONTROLS = 'controls'
    NUMBER = 'number'

    def parameters(self):
        match self:
            case Fixture.CONTROLS:
                return 'HOMEWARD_NHF008_FIXTURE', '^TestNHF008ControlServer$', 'controls.yaml', 12
            case Fixture.NUMBER:
                return 'HOMEWARD_NHF008_NUMBER_FIXTURE', '^TestNHF008NumberServer$', 'number.yaml', 1
        raise AssertionError('Unhandled fixture')


class Control(Enum):
    SQL_CONFLICT = 'sql-race-counter'
    WITHDRAW = 'withdraw-thermostat'
    REDISCOVER = 'rediscover'
    DENY_WRITE = 'deny-write'
    ALLOW_WRITE = 'allow-write'
    DROP_OBSERVATIONS = 'drop-observations'
    RESTORE_OBSERVATIONS = 'restore-observations'
    NUMBER_CONFLICT = 'sql-race-number'
    READ_SQL = 'read-sql'


class Command(Enum):
    INFO = 'info'
    TREE = 'tree'
    FILL_LOGIN = 'fill-login'
    RESIZE = 'resize'
    CLICK = 'click'
    TAP = 'tap'
    KEY = 'key'
    SCROLL = 'scroll'
    PRESS = 'press'
    RELEASE = 'release'
    SIGN_OUT = 'sign-out'
    CLOSE = 'close'


class Key(Enum):
    SELECT_ALL = 'select-all'
    ZERO = 'digit-0'
    TWO = 'digit-2'
    ONE = 'digit-1'
    THREE = 'digit-3'
    FOUR = 'digit-4'
    FIVE = 'digit-5'
    SIX = 'digit-6'
    SEVEN = 'digit-7'
    EIGHT = 'digit-8'
    NINE = 'digit-9'
    MINUS = 'minus'
    PERIOD = 'period'
    COLON = 'colon'
    CIVIL_SEPARATOR = 'civil-separator'
    LETTER_K = 'letter-k'
    LETTER_I = 'letter-i'
    LETTER_T = 'letter-t'
    LETTER_C = 'letter-c'
    LETTER_H = 'letter-h'
    LETTER_E = 'letter-e'
    LETTER_N = 'letter-n'
    DOWN = 'down'
    ENTER = 'enter'

    @classmethod
    def character(cls, value):
        return dict(zip('0123456789-.:Tkitchen', [
            cls.ZERO, cls.ONE, cls.TWO, cls.THREE, cls.FOUR, cls.FIVE,
            cls.SIX, cls.SEVEN, cls.EIGHT, cls.NINE, cls.MINUS, cls.PERIOD,
            cls.COLON, cls.CIVIL_SEPARATOR, cls.LETTER_K, cls.LETTER_I,
            cls.LETTER_T, cls.LETTER_C, cls.LETTER_H, cls.LETTER_E, cls.LETTER_N,
        ], strict=True))[value]


class Role(Enum):
    FIELD = 'AXTextField'
    BUTTON = 'AXButton'
    SLIDER = 'AXSlider'
    COMBO = 'AXPopUpButton'


def wait_file(path, child):
    deadline = time.monotonic() + 60
    while not path.is_file():
        if child.poll() is not None or time.monotonic() >= deadline:
            raise RuntimeError('Isolated control fixture unavailable: ' + path.name)
        time.sleep(.05)
    return json.loads(path.read_text())


@contextlib.contextmanager
def fixture(directory, server, kind):
    control = directory / 'fixture'
    variable, test, dashboard, entity_count = kind.parameters()
    env = dict(os.environ,
               HOMEWARD_NHF008_SOURCES=str(ROOT / 'qualification/frontend/controls'),
               HOMEWARD_NHF005_DASHBOARD=str(ROOT / 'qualification/frontend/dashboards' / dashboard))
    env[variable] = str(control)
    child = None
    with (directory / 'server.log').open('wb') as log:
        try:
            child = subprocess.Popen([str(server), '-test.run='+test,
                                      '-test.v', '-test.timeout=8m'], cwd=ROOT, env=env,
                                     stdout=log, stderr=subprocess.STDOUT)
            private = wait_file(control / 'ready.json', child)
            origin = urlsplit(private['origin'])
            if (private['mode'] != 'synthetic-only' or origin.scheme != 'https'
                    or origin.hostname != '127.0.0.1' or not origin.port or private['entity_count'] != entity_count):
                raise RuntimeError('Only the selected isolated loopback fixture is accepted')
            if kind is Fixture.NUMBER and private['fixture_mode'] != 'number-contract-only':
                raise RuntimeError('Number contract mode required')
            sequence = 0
            def send(action):
                nonlocal sequence
                if not isinstance(action, Control):
                    raise TypeError('Synthetic control requires its enum')
                sequence += 1
                temporary = control / 'command.writing'
                write(temporary, dict(sequence=sequence, action=action.value))
                temporary.replace(control / 'command.json')
                response = wait_file(control / ('ack-%02d.json' % sequence), child)
                assert response['sequence']==sequence and response['action']==action.value
                expected = {'sequence', 'action', 'helpers'} if action is Control.READ_SQL else {'sequence', 'action'}
                assert set(response)==expected
                return response.get('helpers')
            yield private, send
            (control / 'stop').touch(mode=0o600)
            if child.wait(timeout=25) != 0:
                raise RuntimeError('Isolated control server failed to join')
        finally:
            SESSION['TRANSPORT']['stop'](child, timeout=20)
            with contextlib.suppress(FileNotFoundError):
                (control / 'ready.json').unlink()


def desktop(directory, server, helper, binary, scenario):
    directory.mkdir(mode=0o700)
    data = Path.home() / 'Library/Application Support/Homeward'
    public, preferences = data / 'endpoint.json', data / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve existing product profile/preferences')
    receipt = dict(status='FAIL', scenario=scenario.value)
    with fixture(directory, server, scenario.fixture()) as (private, control):
        namespace = SESSION['namespace'](private)
        def present():
            result = subprocess.run(['security', 'find-generic-password', '-s',
                                     'ca.homeward.session.v1', '-a', namespace], capture_output=True, timeout=10)
            if result.returncode not in (0, 44):
                raise RuntimeError('Keychain metadata unavailable')
            return result.returncode == 0
        if present():
            raise RuntimeError('Preserve existing synthetic credentials')
        data.mkdir(parents=True, exist_ok=True)
        write(public, SESSION['public_profile'](private))
        child, pressed = None, None
        def native(command, **fields):
            if not isinstance(command, Command):
                raise TypeError('Native command requires its enum')
            result = subprocess.run([str(helper), '-'], input=json.dumps(dict(
                pid=child.pid, command=command.value, syntheticOnly=True, **fields)),
                capture_output=True, text=True, timeout=10)
            result.check_returncode()
            return json.loads(result.stdout) if result.stdout.strip() else None
        def tree():
            return native(Command.TREE)
        def point(row):
            bounds = native(Command.INFO)['bounds']
            return row['x'] + row['width']/2 - bounds['X'], row['y'] + row['height']/2 - bounds['Y']
        def find(role, label):
            if not isinstance(role, Role):
                raise TypeError('Native role requires its enum')
            for attempt in range(80):
                matches = [r for r in tree() if r['role'] == role.value and label in (r['title'], r['description']) and r['width'] > 5 and r['height'] > 5]
                if matches:
                    row = matches[0]
                    x, y = point(row)
                    bounds = native(Command.INFO)['bounds']
                    if 1 < x < bounds['Width']-1 and 48 < y < bounds['Height']-48:
                        return row
                    native(Command.SCROLL, delta=-350 if y >= bounds['Height']-48 else 350)
                else:
                    native(Command.SCROLL, delta=-350 if attempt < 40 else 350)
                time.sleep(.15)
            write(directory / 'missing-element-tree.json', tree())
            raise AssertionError('Native element absent: ' + role.value + '/' + label)
        def tap(label):
            find(Role.BUTTON, label)
            native(Command.TAP, label=label)
        def navigate(label):
            if not any(label in (r['title'],r['description']) for r in tree()):
                tap('Navigation')
                wait(label)
            native(Command.TAP,label=label)
        def keys(*values):
            for value in values:
                native(Command.KEY, key=value.value)
        def edit(label, *values):
            row = find(Role.FIELD, label)
            x, y = point(row)
            native(Command.CLICK, x=x, y=y)
            keys(Key.SELECT_ALL, *values)
        def edit_text(label, value):
            edit(label, *[Key.character(c) for c in value])
        def acknowledged(card):
            wait('Acquittée · confirmation en attente', seek=True)
            tap(card+' · Arrêter le suivi')
        def sql(alias, expected):
            for _ in range(4):
                row = control(Control.READ_SQL)[alias]
                if all(row.get(key)==value for key,value in expected.items()):
                    return row
                time.sleep(.2)
            raise AssertionError('Actual helper SQL differs from the submitted value: '+alias)
        def wait(label, timeout=20, seek=False):
            deadline = time.monotonic()+timeout
            attempt = 0
            while not any(label in value for row in tree() for value in (row['title'], row['description'], row['text'])):
                if child.poll() is not None or time.monotonic() > deadline:
                    write(directory / 'missing-label-tree.json', tree())
                    raise AssertionError('Native label absent: ' + label)
                if seek:
                    native(Command.SCROLL, delta=-250 if attempt % 20 < 10 else 250)
                    attempt += 1
                time.sleep(.1)
        def capture(name):
            info = native(Command.INFO)
            run(['screencapture', '-x', '-o', '-l', str(info['window_id']), directory / (name+'.png')], capture_output=True)
            write(directory / (name+'.tree.json'), tree())
            write(directory / (name+'.window.json'), info)
        try:
            with (directory / 'app.log').open('wb') as log:
                child = subprocess.Popen([str(binary)], cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
            deadline = time.monotonic()+20
            while True:
                try:
                    native(Command.INFO)
                    break
                except subprocess.CalledProcessError:
                    if child.poll() is not None or time.monotonic()>deadline:
                        raise
                    time.sleep(.1)
            native(Command.RESIZE, width=1280, height=800)
            wait('Se connecter')
            native(Command.FILL_LOGIN, loginName=private['login_name'], password=private['password'])
            wait('Afficher ma maison')
            assert present()
            tap('Afficher ma maison')
            find(Role.FIELD, 'Valeur' if scenario.fixture() is Fixture.NUMBER else 'Consigne')
            capture('configured-controls')
            match scenario:
                case Scenario.NOMINAL:
                    edit('Consigne', Key.TWO, Key.ZERO)
                    tap('Consigne · Appliquer')
                    wait('Acquittée · confirmation en attente', seek=True)
                    capture('thermostat-ack-observation-separate')
                    tap('Thermostat simulé · Arrêter le suivi')
                    tap('Booléen simulé · Activer')
                    wait('Effet confirmé', seek=True)
                    capture('helper-sql-confirmed')
                case Scenario.SQL_CONFLICT:
                    edit('Compteur simulé · Compteur', Key.TWO, Key.ZERO)
                    control(Control.SQL_CONFLICT)
                    tap('Compteur simulé · Compteur · Appliquer')
                    wait('Résultat indéterminé', seek=True)
                    capture('helper-stale-sql-revision-refused')
                case Scenario.CAPABILITY_WITHDRAWAL | Scenario.WRITE_WITHDRAWAL:
                    find(Role.SLIDER, 'Consigne')
                    pressed = native(Command.PRESS, label='Consigne', fraction=.65)
                    control(Control.WITHDRAW if scenario is Scenario.CAPABILITY_WITHDRAWAL else Control.DENY_WRITE)
                    deadline = time.monotonic()+20
                    while any(r['role']=='AXSlider' and r['title']=='Consigne' and r['enabled'] for r in tree()):
                        if time.monotonic()>deadline:
                            raise AssertionError('Held thermostat retained its withdrawn capability')
                        time.sleep(.1)
                    native(Command.RELEASE, **pressed)
                    pressed = None
                    control(Control.REDISCOVER if scenario is Scenario.CAPABILITY_WITHDRAWAL else Control.ALLOW_WRITE)
                    find(Role.SLIDER, 'Consigne')
                    time.sleep(.5)
                    capture('withdrawn-gesture-not-replayed')
                case Scenario.OBSERVATION_REMOVAL:
                    assert any('Température · 0 °C' in r['text'] for r in tree())
                    control(Control.DROP_OBSERVATIONS)
                    deadline = time.monotonic()+10
                    while any('Température ·' in r['text'] or 'Consigne observée' in r['text'] for r in tree()):
                        if time.monotonic()>deadline:
                            raise AssertionError('Missing climate observations retained old values')
                        time.sleep(.1)
                    find(Role.FIELD, 'Consigne')
                    capture('missing-observations-no-invented-zero')
                    control(Control.RESTORE_OBSERVATIONS)
                    wait('Température · 0 °C', seek=True)
                    capture('present-zero-restored')
                case Scenario.CLIMATE_RANGE:
                    edit_text('Seuil bas', '20')
                    edit_text('Seuil haut', '25')
                    find(Role.FIELD, 'Seuil bas')
                    capture('range-drafts-before-submit')
                    tap('Plage de consigne · Appliquer')
                    wait('Acquittée · confirmation en attente', seek=True)
                    capture('range-both-endpoints-independent-of-observation')
                    tap('HVAC simulé · Arrêter le suivi')
                    tap('HVAC simulé · Mode demandé · Climatisation')
                    acknowledged('HVAC simulé')
                    tap('HVAC simulé · Ventilation demandée · Marche')
                    wait('Acquittée · confirmation en attente', seek=True)
                    capture('mode-and-fan-target-separate-from-activity')
                    tap('HVAC simulé · Arrêter le suivi')
                case Scenario.FAN:
                    tap('Ventilateur simulé · Allumer')
                    acknowledged('Ventilateur simulé')
                    tap('Ventilateur simulé · Vitesse · Haute')
                    wait('Acquittée · confirmation en attente', seek=True)
                    capture('fan-independent-power-and-speed')
                    tap('Ventilateur simulé · Arrêter le suivi')
                case Scenario.COLOR:
                    edit_text('Température de couleur', '0')
                    tap('Température de couleur · Appliquer')
                    wait('Valeur hors limites', seek=True)
                    capture('invalid-kelvin-stays-editable')
                    edit_text('Température de couleur', '2700')
                    tap('Température de couleur · Appliquer')
                    wait('Acquittée · confirmation en attente', seek=True)
                    capture('kelvin-preview-and-exact-server-grid')
                    tap('Couleur simulée · Arrêter le suivi')
                case Scenario.TEXT_CIVIL:
                    edit_text('Texte simulé · Texte', 'kitchenkitchen')
                    tap('Texte simulé · Texte · Appliquer')
                    wait('Valeur hors limites', seek=True)
                    edit_text('Texte simulé · Texte', 'kitchen')
                    tap('Texte simulé · Texte · Appliquer')
                    wait('Effet confirmé', seek=True)
                    sql('input_text.guest_bedroom_remote_last_event', dict(text='kitchen', revision=2))
                    capture('text-invalid-corrected-and-confirmed')
                    edit_text('Date simulée · Date', '2026-02-30')
                    tap('Date simulée · Date · Appliquer')
                    wait('Valeur hors limites', seek=True)
                    edit_text('Date simulée · Date', '2026-02-20')
                    tap('Date simulée · Date · Appliquer')
                    sql('input_datetime.gazon_date_de_semis', dict(date='2026-02-20', revision=2))
                    edit_text('Heure simulée · Heure', '05:20:00')
                    tap('Heure simulée · Heure · Appliquer')
                    sql('input_datetime.bathroom_heating_time', dict(time='05:20:00', revision=2))
                    edit_text('Instant civil simulé · Date et heure', '2026-10-20T05:20:00')
                    tap('Instant civil simulé · Date et heure · Appliquer')
                    sql('input_datetime.sdb1_occupancy_last_time', dict(datetime='2026-10-20T05:20:00', revision=2))
                    wait('2026-10-20 05:20:00', seek=True)
                    capture('civil-values-confirmed-without-timezone')
                case Scenario.COUNTER_SELECT:
                    edit_text('Compteur simulé · Compteur', '1')
                    tap('Compteur simulé · Compteur · Appliquer')
                    wait('Valeur hors limites', seek=True)
                    edit_text('Compteur simulé · Compteur', '2')
                    tap('Compteur simulé · Compteur · Appliquer')
                    sql('counter.bathroom_heating_used_today', dict(counter=2, revision=2))
                    tap('Compteur simulé · Augmenter')
                    sql('counter.bathroom_heating_used_today', dict(counter=4, revision=3))
                    row=find(Role.COMBO, 'Choix simulé · Option')
                    x,y=point(row)
                    native(Command.CLICK,x=x,y=y)
                    time.sleep(.2)
                    keys(Key.DOWN,Key.ENTER)
                    sql('input_select.time_of_day', dict(selected='Morning', revision=2))
                    capture('counter-and-keyboard-option-confirmed')
                    navigate('Préférences')
                    tap('Thème sombre')
                    tap('Texte à 200 %')
                    navigate('Accueil')
                    find(Role.COMBO, 'Choix simulé · Option')
                    wait('Morning')
                    capture('observed-option-dark-200')
                case Scenario.NUMBER:
                    edit_text('Valeur', '11')
                    tap('Valeur · Appliquer')
                    wait('Valeur hors limites', seek=True)
                    capture('number-out-of-bounds-not-submitted')
                    edit_text('Valeur', '-2.5')
                    keys(Key.ENTER)
                    sql('input_number.nhf008_number', dict(number=-2.5, revision=2))
                    tap('Valeur · Augmenter')
                    tap('Valeur · Appliquer')
                    sql('input_number.nhf008_number', dict(number=-2, revision=3))
                    find(Role.SLIDER, 'Valeur')
                    pressed=native(Command.PRESS,label='Valeur',fraction=.5)
                    native(Command.RELEASE,**pressed)
                    pressed=None
                    sql('input_number.nhf008_number', dict(number=0, revision=4))
                    wait('Effet confirmé', seek=True)
                    capture('number-keyboard-stepper-pointer-and-present-zero')
                case Scenario.NUMBER_CONFLICT:
                    edit_text('Valeur', '2')
                    control(Control.NUMBER_CONFLICT)
                    tap('Valeur · Appliquer')
                    wait('Résultat indéterminé', seek=True)
                    capture('number-stale-sql-revision-not-overwritten')
                case Scenario.THEMES:
                    navigate('Préférences')
                    tap('Thème sombre')
                    tap('Texte à 200 %')
                    navigate('Accueil')
                    for width,height in [(1024,600),(1280,800),(800,1280)]:
                        native(Command.RESIZE,width=width,height=height)
                        find(Role.FIELD,'Consigne')
                        capture(f'dark-200-{width}x{height}')
            native(Command.SIGN_OUT)
            wait('Se connecter')
            assert not present()
            native(Command.CLOSE)
            assert child.wait(timeout=15)==0
            receipt.update(status='AUTOMATED_CHECKS_PASS', binary_sha256=digest(binary), keychain_absent_after_logout=True)
        except BaseException as error:
            receipt['failure'] = str(error)
            if child is not None and child.poll() is None:
                with contextlib.suppress(Exception):
                    capture('failure')
            raise
        finally:
            if pressed is not None and child is not None and child.poll() is None:
                with contextlib.suppress(Exception):
                    native(Command.RELEASE, **pressed)
            SESSION['TRANSPORT']['stop'](child, timeout=15)
            if present():
                run(['security', 'delete-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True)
            if public.exists():
                assert json.loads(public.read_text())==SESSION['public_profile'](private)
                public.unlink()
            if preferences.exists():
                preferences.unlink()
            write(directory / 'interaction-result.json', receipt)
    report = json.loads((directory / 'fixture/result.json').read_text())
    submitted, physical = scenario.counts()
    assert report['server_joined'] and len(report['submit_ids'] or [])==submitted
    assert len(set(report['submit_ids'] or []))==submitted
    reads = (report['get_ids'] or []) + (report['watch_ids'] or [])
    assert all(command_id in (report['submit_ids'] or []) for command_id in reads)
    assert bool(reads)==bool(submitted)
    assert report['fake_driver_calls']==physical
    assert report['command_driver_entries']==submitted
    assert report['native_login_calls']==report['native_revokes']==1
    assert report['dashboard_get_calls']>=1 and report['dashboard_watch_calls']>=1
    before, after = report['helper_sql_initial'], report['helper_sql_final']
    match scenario:
        case Scenario.NOMINAL:
            assert report['last_device_arguments']==dict(setClimate=dict(setpointCelsius=20))
            assert after['input_boolean.away_mode']['boolean'] is True
            assert after['input_boolean.away_mode']['revision']==before['input_boolean.away_mode']['revision']+1
        case Scenario.SQL_CONFLICT:
            assert after['counter.bathroom_heating_used_today']['counter']==2
            assert after['counter.bathroom_heating_used_today']['revision']==before['counter.bathroom_heating_used_today']['revision']+1
        case (Scenario.CAPABILITY_WITHDRAWAL | Scenario.WRITE_WITHDRAWAL
              | Scenario.OBSERVATION_REMOVAL | Scenario.THEMES):
            assert before==after
        case Scenario.CLIMATE_RANGE:
            assert report['device_arguments']==[
                dict(setClimate=dict(heatThresholdCelsius=20,coolThresholdCelsius=25)),
                dict(setClimate=dict(mode='CLIMATE_MODE_COOL')),
                dict(setClimate=dict(fanTarget='CLIMATE_FAN_TARGET_ON')),
            ]
            assert before==after
        case Scenario.FAN:
            assert report['device_arguments']==[dict(setFan=dict(on=True)),dict(setFan=dict(speed='High'))]
            assert before==after
        case Scenario.COLOR:
            assert report['device_arguments']==[dict(setLight=dict(colorTempMireds=369))]
            assert before==after
        case Scenario.TEXT_CIVIL:
            assert after['input_text.guest_bedroom_remote_last_event']['text']=='kitchen'
            assert after['input_datetime.gazon_date_de_semis']['date']=='2026-02-20'
            assert after['input_datetime.bathroom_heating_time']['time']=='05:20:00'
            assert after['input_datetime.sdb1_occupancy_last_time']['datetime']=='2026-10-20T05:20:00'
        case Scenario.COUNTER_SELECT:
            assert after['counter.bathroom_heating_used_today']['counter']==4
            assert after['counter.bathroom_heating_used_today']['revision']==3
            assert after['input_select.time_of_day']['selected']=='Morning'
        case Scenario.NUMBER:
            assert after['input_number.nhf008_number']['number']==0 and after['input_number.nhf008_number']['revision']==4
        case Scenario.NUMBER_CONFLICT:
            assert after['input_number.nhf008_number']['number']==1 and after['input_number.nhf008_number']['revision']==2
    receipt['server'] = report
    write(directory / 'result.json', receipt)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scenario', type=Scenario, choices=list(Scenario), action='append')
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through frontend.py run --cargo.')
    proof = Path(SESSION['CONFIG']['frontend_cache']) / 'nhf008' / ('homeward-controls-native-'+str(time.time_ns()))
    proof.mkdir(mode=0o700)
    print('NHF008 native controls proof: '+str(proof), flush=True)
    server, helper = proof/'server.test', proof/'native-ui'
    run(['go', 'test', '-c', '-race', '-o', server, './internal/server'], env=dict(os.environ, GOFLAGS='-p=2'))
    modules = Path(SESSION['CONFIG']['frontend_cache']) / 'session-spike/swift-modules'
    run(['xcrun', 'swiftc', '-module-cache-path', modules, ROOT/'qualification/frontend/native-ui.swift', '-o', helper])
    run(['cargo', 'build', '--locked', '-p', 'homeward-app', '--bin', 'homeward-desktop'])
    bundle = proof/'Homeward Controls.app/Contents'
    (bundle/'MacOS').mkdir(parents=True)
    binary = bundle/'MacOS/homeward-desktop'
    shutil.copy2(SESSION['TARGET']/'debug/homeward-desktop', binary)
    with (bundle/'Info.plist').open('wb') as output:
        plistlib.dump(dict(CFBundleExecutable=binary.name, CFBundleIdentifier=SESSION['PACKAGE'],
                          CFBundleName='Homeward Controls', CFBundlePackageType='APPL', NSHighResolutionCapable=True), output)
    for scenario in args.scenario or list(Scenario):
        desktop(proof/scenario.value, server, helper, binary, scenario)
    write(proof/'result.json', dict(status='AUTOMATED_CHECKS_PASS', server_sha256=digest(server),
                                  binary_sha256=digest(binary), synthetic_only=True,
                                  captures_require_visual_inspection=True, physical_device_qualified=False))


if __name__ == '__main__':
    main()
