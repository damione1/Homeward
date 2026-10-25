#!/usr/bin/env python3
"""Desktop dashboard List/Get/Watch over isolated SQL/auth/TLS + fake Host.

Opt-in only; no production origin, private HA file or real device is accepted.
Run through frontend.py run --cargo. Native captures require visual inspection.
"""
from enum import Enum
from pathlib import Path
import contextlib
import json
import os
import plistlib
import runpy
import shutil
import stat
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SESSION = runpy.run_path(str(ROOT/'scripts/qualify-frontend-session.py'))
TRANSPORT = SESSION['TRANSPORT']
run, write, digest = SESSION['run'], SESSION['write'], SESSION['digest']

class Command(Enum):
    INFO='info'; FOCUS='focus'; TAP='tap'; RESIZE='resize'; TREE='tree'; KEY='key'
    FILL_LOGIN='fill-login'; SCROLL='scroll'; SIGN_OUT='sign-out'; CLOSE='close'
class Key(Enum):
    END='end'; ENTER='enter'; ESCAPE='escape'
class Control(Enum):
    DENY_DASHBOARDS='deny-dashboards'; ALLOW_DASHBOARDS='allow-dashboards'
class Projection(Enum):
    NORMAL='normal'; FUTURE_VERSION='future-version'; FUTURE_PROFILE='future-profile'; WRONG_CONFIG='wrong-config'
class Gate(Enum):
    GET='get'; WATCH='watch'

def atomic(path, content):
    temporary=path.with_suffix(path.suffix+'.writing')
    temporary.write_text(content)
    temporary.replace(path)

def deduplicate_closed_binary(path, root):
    digest_value=digest(path); info=path.stat()
    for candidate in root.rglob(path.name):
        if candidate==path or candidate.is_symlink() or not candidate.is_file(): continue
        older=candidate.stat()
        if older.st_uid!=os.getuid() or older.st_dev!=info.st_dev or older.st_size!=info.st_size or stat.S_IMODE(older.st_mode)!=stat.S_IMODE(info.st_mode): continue
        if digest(candidate)!=digest_value: continue
        closed=subprocess.run(['lsof','-t',str(candidate)],capture_output=True,timeout=15)
        if closed.returncode!=1 or closed.stdout: continue
        temporary=path.with_name('.closed-proof-'+str(time.time_ns()))
        os.link(candidate,temporary); temporary.replace(path)
        assert digest(path)==digest_value
        write(path.with_name(path.name+'.deduplicated.json'),dict(canonical=str(candidate),sha256=digest_value,closed_owned_proof_only=True))
        return

def desktop(proof, server_binary, helper, binary):
    directory=proof/'desktop'; directory.mkdir(mode=0o700)
    sources=directory/'server-dashboard'; sources.mkdir(mode=0o700)
    alpha=(ROOT/'qualification/frontend/dashboards/actions.yaml').read_text().replace('Maison synthétique','Maison réseau')
    beta=(ROOT/'qualification/frontend/dashboards/network-alternate.yaml').read_text()
    atomic(sources/'alpha.yaml',alpha); atomic(sources/'beta.yaml',beta)
    entries=[dict(id='synthetic-dashboard',url_path='synthetic',file='alpha.yaml'),dict(id='synthetic-alternate',url_path='alternate',file='beta.yaml')]
    def catalogue(values): atomic(sources/'catalogue.json',json.dumps(dict(version=1,dashboards=values)))
    catalogue(entries)
    fixture=directory/'fixture'
    env=dict(os.environ,HOMEWARD_NHF006_FIXTURE=str(fixture),HOMEWARD_NHF006_SCENARIO='ack-only',
             HOMEWARD_NHF006_DEVICES=str(ROOT/'qualification/frontend/action-devices.json'),HOMEWARD_NHF007_CATALOGUE=str(sources/'catalogue.json'))
    env.pop('HOMEWARD_NHF005_DASHBOARD',None)
    server=child=None; private=None; namespace=None; public=None; preferences=None
    receipt=dict(status='FAIL',synthetic_only=True,physical_device_qualified=False)
    profile_owned=False
    checks=[]
    def until(predicate, label, timeout=25):
        deadline=time.monotonic()+timeout
        while not predicate():
            if server is not None and server.poll() is not None: raise RuntimeError('Isolated server stopped: '+label)
            if child is not None and child.poll() is not None: raise RuntimeError('Owned UI stopped: '+label)
            if time.monotonic()>=deadline: raise AssertionError('Timed out: '+label)
            time.sleep(.1)
    def native(command, **fields):
        if not isinstance(command,Command): raise TypeError('Native command requires enum')
        result=subprocess.run([str(helper),'-'],input=json.dumps(dict(pid=child.pid,command=command.value,syntheticOnly=True,**fields)),capture_output=True,text=True,timeout=10)
        if result.returncode: raise RuntimeError('Owned native command failed: '+command.value+' '+result.stderr.strip())
        return json.loads(result.stdout) if result.stdout.strip() else None
    def labels(): return [value for row in native(Command.TREE) for value in (row['title'],row['description'],row['text']) if value]
    def has(text): return any(text in value for value in labels())
    def wait(text): until(lambda:has(text),text)
    def absent(text): until(lambda:not has(text),'removed '+text)
    def capture(name):
        info=native(Command.INFO)
        run(['screencapture','-x','-o','-l',str(info['window_id']),directory/(name+'.png')],capture_output=True)
        write(directory/(name+'.tree.json'),native(Command.TREE))
        write(directory/(name+'.window.json'),info)
    def top():
        for _ in range(3): native(Command.SCROLL,delta=700)
    def read():
        top()
        if has('Afficher ma maison'): native(Command.TAP,label='Afficher ma maison')
    def navigate(label):
        if not any(row['title']==label and row['role'] in ('AXButton','AXStaticText') for row in native(Command.TREE)):
            native(Command.TAP,label='Navigation')
        native(Command.TAP,label=label)
    def card_text(title,expected):
        rows=native(Command.TREE)
        group=next((row for row in rows if row['role']=='AXGroup' and row['title']==title),None)
        return group is not None and any(row['role']=='AXStaticText' and row['text']==expected and group['x']<=row['x']<=group['x']+group['width'] and group['y']<=row['y']<=group['y']+group['height'] for row in rows)
    def choose_last():
        top(); native(Command.TAP,label='Choisir un dashboard')
        native(Command.KEY,key=Key.END.value); native(Command.KEY,key=Key.ENTER.value)
    def gate(kind):
        for prefix in ('pause-','release-'):
            with contextlib.suppress(FileNotFoundError): (sources/(prefix+kind.value)).unlink()
        with contextlib.suppress(FileNotFoundError): (sources/(kind.value+'-paused')).unlink()
        (sources/('pause-'+kind.value)).touch(mode=0o600)
    def paused(kind): until(lambda:(sources/(kind.value+'-paused')).exists(),kind.value+' barrier')
    def release(kind): (sources/('release-'+kind.value)).touch(mode=0o600)
    def present():
        result=subprocess.run(['security','find-generic-password','-s','ca.homeward.session.v1','-a',namespace],capture_output=True,timeout=10)
        if result.returncode not in (0,44): raise RuntimeError('Keychain metadata unavailable')
        return result.returncode==0
    try:
        with (directory/'server.log').open('wb') as log:
            server=subprocess.Popen([str(server_binary),'-test.run=^TestNHF006ActionServer$','-test.v','-test.timeout=10m'],cwd=ROOT,env=env,stdout=log,stderr=subprocess.STDOUT)
        until(lambda:(fixture/'ready.json').exists(),'fixture readiness',60)
        private=json.loads((fixture/'ready.json').read_text()); origin=urlsplit(private['origin'])
        assert private['mode']=='synthetic-only' and origin.scheme=='https' and origin.hostname=='127.0.0.1' and origin.port
        namespace=SESSION['namespace'](private)
        data=Path.home()/'Library/Application Support/Homeward'; public=data/'endpoint.json'; preferences=data/'interface.json'
        if public.exists() or preferences.exists() or present(): raise RuntimeError('Preserve an existing profile/preferences/credential')
        data.mkdir(parents=True,exist_ok=True); write(public,SESSION['public_profile'](private)); profile_owned=True
        with (directory/'app.log').open('wb') as log: child=subprocess.Popen([str(binary)],stdout=log,stderr=subprocess.STDOUT)
        def ready():
            try: native(Command.FOCUS); return True
            except RuntimeError: return False
        until(ready,'native UI readiness')
        native(Command.RESIZE,width=1280,height=800)
        wait('Se connecter'); native(Command.FILL_LOGIN,loginName=private['login_name'],password=private['password'])
        wait('Afficher ma maison'); read(); wait('Lumière synthétique'); wait('Choisir un dashboard')
        capture('first-dashboard'); checks.append('authorized-first-source')
        choose_last(); wait('Confort réseau'); wait('Mesure réseau'); absent('Relais synthétique')
        until(lambda:card_text('Mesure réseau','0 Cel'),'exact measurement binding'); capture('second-dashboard'); checks.append('selection-second-source')
        atomic(sources/'beta.yaml',beta.replace('Atelier réseau','Atelier actualisé').replace('Mesure réseau','Mesure actualisée').replace('sensor.backyard_all_count','binary_sensor.arc_soundbar_microphone',1))
        wait('Atelier actualisé'); wait('Mesure actualisée'); until(lambda:card_text('Mesure actualisée','Inactif'),'exact changed binary binding'); capture('hot-reload-title-layout-binding'); checks.append('hot-reload')
        # Force a fresh List/Get with a changed Get result. The List token must
        # be re-read before the new layout is published.
        gate(Gate.GET)
        atomic(sources/'beta.yaml',beta.replace('Atelier réseau','Avant Get'))
        paused(Gate.GET)
        atomic(sources/'beta.yaml',beta.replace('Atelier réseau','Après Get').replace('Confort réseau','Après course Get'))
        release(Gate.GET); wait('Après course Get'); capture('list-get-race'); checks.append('list-get-race')
        gate(Gate.WATCH)
        atomic(sources/'beta.yaml',beta.replace('Atelier réseau','Avant Watch'))
        paused(Gate.WATCH)
        atomic(sources/'beta.yaml',beta.replace('Atelier réseau','Après Watch').replace('Confort réseau','Après course Watch'))
        release(Gate.WATCH); wait('Après course Watch'); capture('read-watch-race'); checks.append('read-watch-race')
        catalogue([entries[0]]); wait('Maison réseau'); absent('Après course Watch'); capture('active-deleted'); checks.append('active-deletion')
        catalogue([]); wait('Le serveur ne fournit pas de dashboard autorisé'); absent('Lumière synthétique')
        catalogue(entries); wait('Lumière synthétique'); capture('empty-reappeared'); checks.append('empty-reappearance')
        sequence=0
        def control(action):
            nonlocal sequence
            sequence+=1; write(fixture/'command.writing',dict(sequence=sequence,action=action.value)); (fixture/'command.writing').replace(fixture/'command.json')
            until(lambda:(fixture/('ack-%02d.json'%sequence)).exists(),action.value)
        control(Control.DENY_DASHBOARDS); absent('Lumière synthétique'); read(); wait('Le serveur ne fournit pas de dashboard autorisé')
        capture('rights-withdrawn'); checks.append('rights-withdrawal')
        control(Control.ALLOW_DASHBOARDS); read(); wait('Lumière synthétique'); checks.append('rights-restored')
        choose_last(); wait('Après course Watch')
        catalogue([entries[0],dict(entries[1],projection=Projection.WRONG_CONFIG.value)])
        wait('Synchronisation du dashboard'); absent('Mesure réseau'); capture('config-cohort-rejected'); checks.append('mismatched-config-hidden')
        catalogue(entries); wait('Mesure réseau')
        catalogue([entries[0],dict(entries[1],projection=Projection.FUTURE_PROFILE.value)])
        wait('homeward-dashboard-99'); wait('beta.yaml'); absent('Mesure réseau'); capture('future-profile-inert'); checks.append('future-profile')
        catalogue([entries[0],dict(entries[1],projection=Projection.FUTURE_VERSION.value)])
        wait('Version du layout non prise en charge'); capture('future-version-inert'); checks.append('future-version')
        catalogue(entries); atomic(sources/'beta.yaml','strategy:\n  type: custom:synthetic-strategy\n')
        wait('custom:synthetic-strategy'); capture('runtime-strategy-inert'); checks.append('runtime-strategy')
        unknown=beta.replace('    - type: grid\n      columns: 2','    - type: custom:synthetic-card\n      name: Extension inconnue\n    - type: grid\n      columns: 2',1)
        atomic(sources/'beta.yaml',unknown); wait('custom:synthetic-card'); capture('unknown-card-inert'); checks.append('unknown-card')
        atomic(sources/'beta.yaml',beta); wait('Confort réseau')
        # Enter a room directly from the home shortcut, then verify that Back
        # restores the actual observed scroll position and opener focus.
        for _ in range(3): native(Command.SCROLL,delta=-700)
        def room_row():
            return next((row for row in native(Command.TREE) if row['role']=='AXButton' and row['title'].startswith('Pièce réseau, ')),None)
        until(lambda:room_row() is not None,'home room shortcut')
        opener=room_row(); capture('home-before-room')
        native(Command.TAP,label=opener['title']); wait('Relais pièce réseau'); capture('room-from-server-view')
        native(Command.TAP,label='Retour'); until(lambda:room_row() is not None,'home room shortcut restored')
        until(lambda:room_row() is not None and abs(room_row()['y']-opener['y'])<=2,'home scroll restored')
        until(lambda:room_row()['focused'],'room opener focus restored')
        capture('home-scroll-focus-restored'); checks.append('room-back-scroll-focus')
        top()
        for width,height in [(1024,600),(1280,800),(800,1280)]:
            native(Command.RESIZE,width=width,height=height); capture('light-%dx%d'%(width,height))
        native(Command.RESIZE,width=1280,height=800)
        navigate('Préférences'); native(Command.TAP,label='Thème sombre'); native(Command.TAP,label='Texte à 200 %')
        navigate('Accueil'); top()
        for width,height in [(1024,600),(1280,800),(800,1280)]:
            native(Command.RESIZE,width=width,height=height); capture('dark-200-%dx%d'%(width,height))
        native(Command.RESIZE,width=1280,height=800)
        checks.append('light-dark-double-text-formats')
        native(Command.SIGN_OUT); wait('Se connecter'); assert not present(); capture('signed-out')
        native(Command.CLOSE); assert child.wait(timeout=15)==0
        (fixture/'stop').touch(mode=0o600); assert server.wait(timeout=25)==0
        report=json.loads((fixture/'result.json').read_text())
        assert report['server_joined'] and report['fake_driver_calls']==0 and report['command_driver_entries']==0 and not report['submit_ids']
        assert report['native_login_calls']==1 and report['native_revokes']==1 and report['dashboard_get_calls']>8 and report['dashboard_watch_calls']>5
        receipt.update(status='AUTOMATED_CHECKS_PASS',checks=checks,server=report,binary_sha256=digest(binary),keychain_absent_after_logout=True,captures_require_visual_inspection=True)
    except BaseException as error:
        receipt.update(failure=str(error),checks=checks)
        if child is not None and child.poll() is None:
            with contextlib.suppress(Exception): capture('failure')
        raise
    finally:
        TRANSPORT['stop'](child,timeout=15)
        TRANSPORT['stop'](server,timeout=20)
        if profile_owned and namespace is not None and public is not None and public.exists() and private is not None:
            if present(): run(['security','delete-generic-password','-s','ca.homeward.session.v1','-a',namespace],capture_output=True)
            assert json.loads(public.read_text())==SESSION['public_profile'](private); public.unlink()
            if preferences.exists(): preferences.unlink()
        if fixture.exists():
            with contextlib.suppress(FileNotFoundError): (fixture/'ready.json').unlink()
        write(directory/'result.json',receipt)

def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'): raise SystemExit('Run through frontend.py run --cargo')
    proof=Path(SESSION['CONFIG']['frontend_cache'])/'dashboard-ui'/str(time.time_ns()); proof.mkdir(parents=True,mode=0o700)
    print('Native dashboard proof: '+str(proof),flush=True)
    server=proof/'server.test'; run(['go','test','-c','-race','-o',server,'./internal/server']); deduplicate_closed_binary(server,proof.parent)
    helper=proof/'native-ui'; modules=Path(SESSION['CONFIG']['frontend_cache'])/'session-spike/swift-modules'
    run(['xcrun','swiftc','-module-cache-path',modules,ROOT/'qualification/frontend/native-ui.swift','-o',helper])
    run(['cargo','build','--locked','-p','homeward-app','--bin','homeward-desktop'])
    bundle=proof/'Homeward Dashboards.app/Contents'; (bundle/'MacOS').mkdir(parents=True)
    binary=bundle/'MacOS/homeward-desktop'; shutil.copy2(SESSION['TARGET']/'debug/homeward-desktop',binary); deduplicate_closed_binary(binary,proof.parent)
    with (bundle/'Info.plist').open('wb') as output: plistlib.dump(dict(CFBundleExecutable=binary.name,CFBundleIdentifier=SESSION['PACKAGE'],CFBundleName='Homeward Dashboards',CFBundlePackageType='APPL',NSHighResolutionCapable=True),output)
    desktop(proof,server,helper,binary)
    write(proof/'result.json',dict(status='AUTOMATED_CHECKS_PASS',desktop=str(proof/'desktop/result.json'),server_sha256=digest(server),physical_device_qualified=False))
if __name__=='__main__': main()
