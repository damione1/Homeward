#!/usr/bin/env python3
"""Opt-in native macOS environment proof using the actual isolated TLS facade.

Desktop first. Screenshots require visual inspection. Synthetic observations
only, no physical device driver, no production URL, no simulated OS evidence.
"""
import argparse
import contextlib
from datetime import datetime, timezone
from enum import Enum
import json
import os
from pathlib import Path
import platform
import runpy
import shutil
import subprocess
import time
from urllib.parse import urlsplit
from zoneinfo import ZoneInfo

ROOT = Path(__file__).resolve().parents[1]
NATIVE = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-recipes-ui.py'))
PROTOCOL = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
Case, Command = NATIVE['Case'], NATIVE['Command']
Control = PROTOCOL['Control']
write, sha, wait_file, stop = SESSION['write'], PROTOCOL['sha'], PROTOCOL['wait_file'], PROTOCOL['stop']


class Role(str, Enum):
    MEMBER = 'member'
    GUEST = 'guest'

    def __str__(self):
        return self.value


def run_case(proof, build, role, case):
    appearance, text = case.preferences()
    directory = proof / (role.value + '-' + case.value)
    directory.mkdir(mode=0o700)
    source = directory / 'sources'
    shutil.copytree(ROOT / 'qualification/frontend/environment', source)
    fixture = directory / 'fixture'
    product = Path.home() / 'Library/Application Support/Homeward'
    public, preferences = product / 'endpoint.json', product / 'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('preserve existing Homeward settings')
    if any(Path(p.strip()).name == 'homeward-desktop' for p in subprocess.check_output(['ps', '-axo', 'comm'], text=True).splitlines()[1:]):
        raise RuntimeError('preserve existing Homeward process')
    server = app = private = namespace = None
    sequence, checks = 0, []

    def credential_present():
        result = subprocess.run(['security', 'find-generic-password', '-s', 'ca.homeward.session.v1', '-a', namespace], capture_output=True, timeout=10)
        assert result.returncode in (0, 44)
        return result.returncode == 0

    def native(command, **fields):
        assert isinstance(command, Command)
        result = subprocess.run([build['helper'], '-'], input=json.dumps(dict(pid=app.pid, command=command.value, syntheticOnly=True, **fields)), capture_output=True, text=True, timeout=10)
        result.check_returncode()
        return json.loads(result.stdout) if result.stdout.strip() else None

    def tree():
        return native(Command.TREE)

    def has(label):
        return any(label in value for row in tree() for value in (row['title'], row['description'], row['text']))

    def wait(label, absent=False, timeout=30):
        deadline = time.monotonic() + timeout
        while has(label) == absent:
            if app.poll() is not None or time.monotonic() > deadline:
                write(directory / 'failed-wait.tree.json', tree())
                raise RuntimeError('native label did not ' + ('disappear: ' if absent else 'appear: ') + label)
            time.sleep(.1)

    def reveal(label, button=False):
        # Server acknowledgement precedes the asynchronous cache/UI publication.
        # Traverse the visible viewport until the label arrives, with a deadline
        # independent of the speed of the AX helper.
        deadline, attempt = time.monotonic() + 30, 0
        while time.monotonic() < deadline:
            bounds = native(Command.INFO)['bounds']
            rows = [row for row in tree() if any(label in value for value in (row['title'], row['description'], row['text']))
                    and (not button or row['role'] == 'AXButton') and row['height'] > 5 and row['width'] > 5]
            if rows:
                center = rows[0]['y'] + rows[0]['height'] / 2 - bounds['Y']
                if 35 < center < bounds['Height'] - 35:
                    return rows[0]
                native(Command.SCROLL, delta=-300 if center > 35 else 300)
            else:
                native(Command.SCROLL, delta=-300 if attempt % 30 < 15 else 300)
            attempt += 1
            time.sleep(.15)
        write(directory / 'failed-reveal.tree.json', tree())
        raise RuntimeError('native visible label missing: ' + label)

    def tap(label):
        reveal(label, button=True)
        native(Command.TAP, label=label)

    def capture(name):
        info = native(Command.INFO)
        subprocess.run(['screencapture', '-x', '-o', '-l', str(info['window_id']), str(directory / (name + '.png'))], check=True, capture_output=True)
        write(directory / (name + '.tree.json'), tree())
        write(directory / (name + '.window.json'), info)

    def control(action):
        nonlocal sequence
        assert isinstance(action, Control)
        sequence += 1
        PROTOCOL['acknowledge'](fixture, server, dict(sequence=sequence, action=action.value))

    def profile_instants(zone):
        # Independent oracle for the rendered wall-clock values. A scalar
        # timezone label alone would miss a stale nested forecast/event model.
        solar, forecasts = [], []
        for row in tree():
            if row['role'] != 'AXStaticText':
                continue
            label = row['text']
            if ' → ' in label:
                stamps = label.split(' → ')
                forecasts.extend(stamps)
            else:
                stamps = [label]
            for stamp in stamps:
                try:
                    at = datetime.strptime(stamp, '%Y-%m-%d %H:%M %z')
                except ValueError:
                    continue
                assert at.utcoffset() == at.astimezone(zone).utcoffset(), 'rendered instant uses another profile timezone: ' + stamp
                if ' → ' not in label:
                    solar.append(at.astimezone(timezone.utc))
        assert len(solar) == 1 and len(forecasts) == 2, 'expected the synthetic solar event and forecast period'
        return solar[0]

    def fresh_read():
        if has('Afficher ma maison'):
            tap('Afficher ma maison')
        wait('Météo simulée')
        wait('0 °C')

    with (directory / 'server.log').open('wb') as server_log, (directory / 'app.log').open('wb') as app_log:
        try:
            env = dict(os.environ, HOMEWARD_NHF010_FIXTURE=str(fixture), HOMEWARD_NHF010_SOURCES=str(source), HOMEWARD_NHF007_CATALOGUE=str(source / 'native-catalogue.json'))
            env.pop('HOMEWARD_NHF005_DASHBOARD', None)
            env.pop('HOMEWARD_NHF010_SMOKE', None)
            server = subprocess.Popen([build['server'], '-test.run=^TestNHF010EnvironmentServer$', '-test.v', '-test.timeout=8m'], cwd=ROOT, env=env, stdout=server_log, stderr=subprocess.STDOUT)
            private = wait_file(fixture / 'ready.json', server)
            origin = urlsplit(private['origin'])
            assert private['mode'] == 'synthetic-only' and origin.scheme == 'https' and origin.hostname == '127.0.0.1' and origin.port
            namespace = SESSION['namespace'](private)
            if credential_present():
                raise RuntimeError('preserve existing synthetic credential')
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
            credentials = private[role.value]
            native(Command.FILL, loginName=credentials['login_name'], password=credentials['password'])
            wait('Afficher ma maison')
            tap('Afficher ma maison')
            wait('Météo simulée')
            wait('0 °C')
            reveal('Météo simulée')
            capture('home-weather')
            reveal('Soleil simulé')
            capture('home-solar')
            reveal('Présence simulée')
            capture('home-presence')
            if role is Role.GUEST:
                wait('Zone privée simulée', absent=True)
                checks.append('ordinary guest sees authorized weather/solar/presence without private zone')
            else:
                reveal('Zone privée simulée')
                capture('home-private-zone')
                checks.append('ordinary member sees the separately authorized private zone')
            if role is Role.MEMBER and case is Case.LIGHT_STANDARD:
                control(Control.STALE)
                reveal('Périmées')
                wait('Périmées')
                capture('weather-expired')
                control(Control.UNKNOWN)
                reveal('Présence inconnue')
                wait('Présence inconnue')
                capture('presence-unknown')
                control(Control.ABSENT)
                reveal('Soleil simulé')
                reveal('Absent dans l’horizon de recherche')
                wait('Absent dans l’horizon de recherche')
                capture('solar-absent')
                control(Control.MISSING)
                reveal('Météo non fournie')
                wait('Météo non fournie')
                capture('weather-missing')
                control(Control.NOMINAL)
                reveal('0 °C')
                wait('0 °C')
                reveal('Soleil simulé')
                solar_utc = profile_instants(ZoneInfo('America/Toronto'))
                control(Control.PARIS)
                reveal('Europe/Paris')
                wait('Europe/Paris')
                reveal('Soleil simulé')
                capture('profile-paris')
                assert profile_instants(ZoneInfo('Europe/Paris')) == solar_utc, 'profile change moved the absolute solar event'
                control(Control.TORONTO)
                reveal('America/Toronto')
                wait('America/Toronto')
                assert profile_instants(ZoneInfo('America/Toronto')) == solar_utc, 'profile restoration moved the absolute solar event'
                tap('Détails privés simulés · Détails')
                wait('Retour aux éléments')
                wait('Zone privée simulée')
                capture('private-detail')
                control(Control.REVOKE)
                wait('Zone privée simulée', absent=True)
                capture('private-detail-revoked')
                if has('Retour aux éléments'):
                    tap('Retour aux éléments')
                control(Control.ALLOW)
                fresh_read()
                wait('Environnement simulé')
                reveal('Zone privée simulée')
                wait('Zone privée simulée')
                control(Control.OUTAGE)
                wait('Hors ligne')
                reveal('Météo simulée')
                capture('offline')
                control(Control.NETWORK)
                control(Control.NOMINAL)
                wait('États à jour')
                wait('Environnement simulé')
                reveal('Météo simulée')
                capture('reconnected')
                checks.append('actual updates preserve expired/unknown/absent/missing, profile changes, private-detail purge and offline recovery')
            native(Command.SIGN_OUT)
            wait('Se connecter')
            assert not credential_present()
            native(Command.CLOSE)
            assert app.wait(timeout=15) == 0
            (fixture / 'stop').touch(mode=0o600)
            assert server.wait(timeout=25) == 0
            final = json.loads((fixture / 'result.json').read_text())
            assert final['server_joined'] and final['native_login_calls'] == final['native_revokes'] == 1
            assert final['command_driver_entries'] == final['fake_driver_calls'] == final['real_device_writes'] == 0 and not final['submit_ids']
            assert final['dashboard_get_calls'] >= 1 and final['dashboard_watch_calls'] >= 1
            assert not (directory / 'app.log').read_bytes()
            result = dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', role=role.value, case=case.value, checks=checks, server=final, controls=sequence, children_joined=True)
            write(directory / 'result.json', result)
            return dict(proof=str(directory), role=role.value, case=case.value, result_sha256=sha(directory / 'result.json'))
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
                        assert private is not None and json.loads(public.read_text()) == SESSION['public_profile'](private)
                        public.unlink()
                    if preferences.exists():
                        preferences.unlink()
                    with contextlib.suppress(FileNotFoundError):
                        (fixture / 'ready.json').unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--role', type=Role, choices=list(Role), action='append')
    parser.add_argument('--case', type=Case, choices=list(Case), action='append')
    args = parser.parse_args()
    if platform.system() != 'Darwin' or not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('run on macOS through frontend.py run --cargo; Android is final-stage')
    config = SESSION['CONFIG']
    proof = Path(config['frontend_cache']) / 'environment-ui' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    print('NHF010 native proof: ' + str(proof), flush=True)
    (proof / 'harness.py').write_bytes(Path(__file__).read_bytes())
    before = PROTOCOL['source_inventory'](config)
    write(proof / 'sources.json', before)
    results = []
    try:
        artifacts = NATIVE['build'](proof, config, 'Homeward Environment')
        for role in args.role or list(Role):
            for case in args.case or (list(Case) if role is Role.MEMBER else [Case.LIGHT_STANDARD]):
                results.append(run_case(proof, artifacts, role, case))
        assert PROTOCOL['source_inventory'](config) == before
        write(proof / 'result.json', dict(status='AUTOMATED_PASS_REQUIRES_VISUAL_INSPECTION', cases=results, build=artifacts, current_source_bytes_unchanged=True, platform=platform.platform(), backend='winit-software', no_real_device_actions=True, android_qualified=False))
    except BaseException:
        write(proof / 'result.json', dict(status='FAILED', completed_cases=results, current_source_bytes_unchanged=PROTOCOL['source_inventory'](config) == before))
        raise
    print('NHF010 native UI PASS (inspect captures): ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
