#!/usr/bin/env python3
"""Explicit, bounded physical USB smoke using an already-owned release install.

Only the package created by the supplied installation receipt receives input.
The normal emulator driver keeps its original device guard. No real provider.
"""
import argparse
from enum import Enum
import json
import os
from pathlib import Path
import re
import runpy
import shlex
import signal
import subprocess
import sys
import time

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'scripts'))
SESSION=runpy.run_path(str(ROOT/'scripts/qualify-frontend-session.py'))
INPUT=SESSION['ANDROID_INPUT']
PACKAGE=SESSION['PACKAGE']
ACTIVITY=INPUT['ACTIVITY_NAME']


class Key(Enum):
    BACK='4'
    HOME='3'
    TAB='61'
    ENTER='66'


class PhysicalScope(Enum):
    FULL='full'
    RESTORE='restore'
    def __str__(self): return self.value


class PhysicalInput(INPUT['AndroidInput']):
    """Reuse OCR geometry, with a separate explicit physical-device boundary."""
    def __init__(self,directory,ocr,adb,serial,device,expected_model):
        if not re.fullmatch(r'[A-Za-z0-9._-]{1,128}',serial) or serial.startswith('emulator-'):
            raise RuntimeError('An explicit USB serial is required')
        connected=subprocess.check_output([str(adb),'devices','-l'],text=True,timeout=15)
        if not any(line.split()[:2]==[serial,'device'] and 'usb:' in line for line in connected.splitlines()):
            raise RuntimeError('The specified physical USB device is not authorized')
        if device('shell','getprop','ro.product.model').stdout.strip()!=expected_model or device('shell','getprop','ro.kernel.qemu').stdout.strip() not in ('','0'):
            raise RuntimeError('Physical-device identity changed')
        self.directory,self.ocr,self.adb,self.serial,self.device=directory,ocr,adb,serial,device
        self.sequence=0

    def is_foreground(self):
        windows=self.device('shell','dumpsys','window').stdout
        focused=[line for line in windows.splitlines() if 'mFocusedApp=' in line]
        if not focused: focused=[line for line in windows.splitlines() if 'mCurrentFocus=' in line]
        return any(PACKAGE+'/' in line for line in focused)

    def foreground(self):
        if not self.is_foreground():
            raise RuntimeError('Preserve the device: Homeward is no longer foreground')

    def capture(self,name):
        self.foreground()
        result=super().capture(name)
        self.last_size=result[1:]
        return result

    def find(self,label,match=INPUT['LabelMatch'].EXACT,timeout=20,scroll=False):
        if not scroll: return super().find(label,match,timeout,False)
        deadline=time.monotonic()+timeout
        while True:
            try: return super().find(label,match,.01,False)
            except AssertionError:
                if time.monotonic()>=deadline: raise
                self.foreground()
                width,height=self.last_size
                self.device('shell','input','swipe',str(width*3//4),str(height*4//5),str(width*3//4),str(height*3//10),'400')

    def format(self,*args):
        raise RuntimeError('Physical display size and density must be preserved')


def qualify(receipt_path,server_path,reset_owned_test_data=False,scope=PhysicalScope.FULL):
    if not isinstance(scope,PhysicalScope): raise TypeError('Typed physical scope required')
    if not os.environ.get('CARGO_TARGET_DIR'): raise RuntimeError('Run through frontend.py')
    receipt=json.loads(receipt_path.read_text())
    if not receipt['package_created_by_this_round']: raise RuntimeError('An owned installation receipt is required')
    config=json.loads((ROOT/'frontend.local.json').read_text())
    proof=Path(config['frontend_cache'])/'nhf023'/f'physical-{time.time_ns()}'
    proof.mkdir(mode=0o700)
    (proof/'driver.py').write_bytes(Path(__file__).read_bytes())
    adb=Path(config['android_sdk'])/'platform-tools/adb'
    serial=receipt['serial']
    def device(*args,check=True):
        return subprocess.run([str(adb),'-s',serial,*map(str,args)],capture_output=True,text=True,check=check,timeout=20)
    paths=device('shell','pm','path',PACKAGE).stdout.splitlines()
    if len(paths)!=1 or not paths[0].startswith('package:/data/app/'):
        raise RuntimeError('One owned installed APK required')
    installed=device('shell','sha256sum',shlex.quote(paths[0].removeprefix('package:'))).stdout.split()[0]
    if installed!=receipt['apk_sha256']: raise RuntimeError('Preserve an externally changed installation')
    ocr=proof/'navigation-ocr'
    subprocess.run(['xcrun','swiftc','-module-cache-path',str(Path(config['frontend_cache'])/'session-spike/swift-modules'),str(ROOT/'qualification/frontend/navigation-ocr.swift'),'-o',str(ocr)],check=True)
    ui=PhysicalInput(proof,ocr,adb,serial,device,receipt['model'])
    if abs(time.time()-int(device('shell','date','+%s').stdout.strip()))>25:
        raise RuntimeError('Synchronize the device clock before creating a test session')
    if reset_owned_test_data:
        # Explicit reset applies only to the scratch APK we created, verified
        # above against its original installation receipt and installed hash.
        device('shell','am','force-stop',PACKAGE)
        cleared=device('shell','pm','clear',PACKAGE)
        if cleared.stdout.strip()!='Success': raise RuntimeError('Owned test-state reset failed')
        device('shell','am','start','-W','-n',PACKAGE+'/'+ACTIVITY)
    server=proof/'server.test'
    import shutil
    shutil.copy2(server_path,server)
    def save(name,value): (proof/name).write_text(json.dumps(value,indent=2)+'\n')
    def key(value):
        if not isinstance(value,Key): raise TypeError('Typed key required')
        ui.foreground(); device('shell','input','keyevent',value.value)
    def text(value):
        ui.foreground()
        result=device('shell','input','text',shlex.quote(value.replace(' ','%s')),check=False)
        if result.returncode: raise RuntimeError('Synthetic text input failed')
    def launch():
        device('shell','am','start','-W','-n',PACKAGE+'/'+ACTIVITY)
        deadline=time.monotonic()+10
        while not ui.is_foreground():
            if time.monotonic()>=deadline: raise RuntimeError('Owned app window did not become foreground')
            time.sleep(.05)
    contains=INPUT['LabelMatch'].CONTAINS
    rotation={name:device('shell','settings','get','system',name).stdout.strip() for name in ('accelerometer_rotation','user_rotation')}
    display={name:device('shell','wm',name).stdout.strip() for name in ('size','density')}
    rotation_changed=False
    intended_rotation={}
    def restore_rotation():
        errors=[]
        for name,value in rotation.items():
            try:
                current=device('shell','settings','get','system',name).stdout.strip()
                if current not in (value,intended_rotation.get(name)):
                    errors.append('External rotation change preserved: '+name)
                    continue
                if current!=value:
                    device('shell','settings','delete' if value=='null' else 'put','system',name,*([] if value=='null' else [value]))
            except Exception as error: errors.append(type(error).__name__+': '+name)
        save('rotation-restoration-attempt.json',dict(errors=errors))
        return errors
    forwarded=False
    port=None
    old_signal=signal.signal(signal.SIGTERM,lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    try:
        with SESSION['fixture'](proof/'server',server,kind=SESSION['FixtureKind'].CACHE,profile=SESSION['UIProfile'].HOME) as (private,control):
            try:
                port=SESSION['urlsplit'](private['origin']).port
                device('reverse','--no-rebind',f'tcp:{port}',f'tcp:{port}'); forwarded=True
                ui.tap('Adresse du serveur',offset_y=55); text(private['origin'])
                ui.tap('Certificat de confiance (PEM)',offset_y=65)
                for index,line in enumerate(private['ca_pem'].strip().splitlines()):
                    if index: key(Key.ENTER)
                    text(line)
                if 'mInputShown=true' in device('shell','dumpsys','input_method').stdout:
                    key(Key.BACK)
                ui.tap('Utiliser ce serveur',scroll=True)
                ui.find('Identifiant')
                ui.tap('Identifiant',offset_y=55); text(private['login_name'])
                key(Key.TAB); text(private['password']); key(Key.ENTER)
                ui.find('Afficher ma maison',match=contains)
                ui.tap('Afficher ma maison',match=contains)
                ui.find('851 entités',match=contains,timeout=30)
                ui.find('Ma maison',match=contains)
                home,w,h=ui.capture('home')
                match scope:
                    case PhysicalScope.FULL:
                        ui.navigation(); ui.tap('Inventaire'); ui.find('Élément')
                        ui.tap(private['expected_first_alias'],match=contains)
                        ui.find('Retour aux éléments'); ui.capture('detail')
                        key(Key.BACK); ui.find('Élément'); ui.capture('native-back')
                        ui.navigation(); ui.tap('Accueil'); ui.find('Ma maison',match=contains)
                        pid=device('shell','pidof',PACKAGE).stdout.strip()
                        # Observe a genuine pause using current WindowManager
                        # focus; never capture/change the system surface.
                        device('shell','am','start','-W','-a','android.settings.SETTINGS')
                        deadline=time.monotonic()+8
                        while ui.is_foreground():
                            if time.monotonic()>=deadline: raise RuntimeError('Actual background transition not observed')
                            time.sleep(.1)
                        save('background-observed.json',dict(pid=pid,physical=True,owned_app_not_resumed=True))
                        launch()
                        ui.find('851 entités',match=contains,timeout=30)
                        if device('shell','pidof',PACKAGE).stdout.strip()!=pid: raise RuntimeError('Background resume replaced the app process')
                        ui.capture('resumed')
                        target='0' if w>h else '1'
                        intended_rotation={'accelerometer_rotation':'0','user_rotation':target}
                        save('rotation-lease.json',dict(serial=serial,original=rotation,intended=intended_rotation))
                        rotation_changed=True
                        device('shell','settings','put','system','accelerometer_rotation','0')
                        device('shell','settings','put','system','user_rotation',target)
                        time.sleep(.7)
                        _,rotated_w,rotated_h=ui.capture('rotated')
                        if (rotated_w>rotated_h)==(w>h): raise RuntimeError('Actual orientation did not change')
                        if device('shell','pidof',PACKAGE).stdout.strip()!=pid: raise RuntimeError('Rotation replaced the app process')
                        ui.find('851 entités',match=contains,timeout=30)
                        if restore_rotation(): raise RuntimeError('Rotation restoration needs inspection')
                        rotation_changed=False
                        ui.find('Ma maison',match=contains)
                    case PhysicalScope.RESTORE:
                        pass
                device('shell','am','force-stop',PACKAGE)
                started=time.monotonic(); launch()
                ui.find('851 entités',match=contains,timeout=30)
                ui.find('Ma maison',match=contains)
                ui.capture('fresh-startup')
                startup=time.monotonic()-started
                ui.navigation(); ui.tap('Préférences')
                ui.tap('Se déconnecter',scroll=True); ui.find('Se connecter'); ui.capture('signed-out')
                device('shell','am','force-stop',PACKAGE); launch(); ui.find('Identifiant')
                ui.capture('signed-out-restart')
                save('client-result.json',dict(status='PASS_PENDING_VISUAL_INSPECTION',physical=True,model=receipt['model'],apk_sha256=installed,
                    scope=scope.value,native_back=True if scope is PhysicalScope.FULL else None,
                    rotation_same_process=True if scope is PhysicalScope.FULL else None,
                    background_resume_same_process=True if scope is PhysicalScope.FULL else None,automatic_fresh_startup=True,
                    startup_upper_bound_seconds=startup,startup_scope='Process launch through OCR and completed capture; conservative bound, not first-frame time.',
                    logout_and_signed_out_restart=True,screen_reader_qualified=False,all_nhf023_acceptance_closed=False))
            except BaseException:
                try: ui.capture('failure')
                except Exception: pass
                raise
        server_result=json.loads((control/'result.json').read_text())
        if server_result['action_rpc_calls']!=0 or server_result['command_driver_entries']!=0 or server_result['connections_remaining']!=0 or server_result['revoke_ok']!=1:
            raise RuntimeError('Physical smoke did not preserve mock/cleanup boundaries')
        save('server-result.json',server_result)
        print('PHYSICAL_SMOKE_PASS '+str(proof),flush=True)
    finally:
        errors=[]
        if rotation_changed:
            errors.extend(restore_rotation())
        if forwarded:
            try: device('reverse','--remove',f'tcp:{port}')
            except Exception as error: errors.append(type(error).__name__+': USB reverse cleanup')
        try:
            after={name:device('shell','settings','get','system',name).stdout.strip() for name in rotation}
            after_display={name:device('shell','wm',name).stdout.strip() for name in display}
        except Exception as error:
            after=after_display=None
            errors.append(type(error).__name__+': final settings read')
        save('device-restoration.json',dict(rotation_before=rotation,rotation_after=after,display_before=display,display_after=after_display,
            restored=not errors and rotation==after and display==after_display,errors=errors,owned_apk_left_installed=True))
        signal.signal(signal.SIGTERM,old_signal)
        if errors or rotation!=after or display!=after_display: raise RuntimeError('Device settings restoration not confirmed (see receipt)')


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--installed-receipt',required=True,type=Path)
    parser.add_argument('--server-binary',required=True,type=Path)
    parser.add_argument('--reset-owned-test-data',action='store_true',help='Explicitly clear the scratch app created by the installation receipt, after verifying its installed APK hash')
    parser.add_argument('--scope',type=PhysicalScope,choices=list(PhysicalScope),default=PhysicalScope.FULL)
    args=parser.parse_args()
    qualify(args.installed_receipt,args.server_binary,args.reset_owned_test_data,args.scope)
