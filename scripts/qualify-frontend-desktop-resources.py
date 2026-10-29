"""Native desktop release resources and 100 actual minimize/restore cycles.

Run through frontend.py run --cargo. Isolated Go-race TLS/core and OS input only.
No video or physical-tablet verdict. Optional macOS compositor latency scope.
"""
import argparse
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import platform
import plistlib
import runpy
import shutil
import statistics
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]
SESSION = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
CONFIG = SESSION['CONFIG']

class Command(Enum):
    INFO = 'info'
    TREE = 'tree'
    FILL_LOGIN = 'fill-login'
    READ = 'read'
    TAP = 'tap'
    SCROLL = 'scroll'
    MINIMIZE = 'minimize'
    RESTORE = 'restore'
    LIFECYCLE = 'lifecycle'
    CLOSE = 'close'
    CHECK_ROW = 'check-row'
    NAVIGATE = 'navigate'

class NavigationSection(Enum):
    HOME = 'Accueil'
    INVENTORY = 'Inventaire'
    SETTINGS = 'Préférences'

class QualificationScope(Enum):
    FULL = 'full'
    SHUTDOWN = 'shutdown'
    LATENCY = 'latency'
    LATENCY_LOAD = 'latency-load'
    IME = 'ime'
    VOICEOVER = 'voiceover'
    RECOVERY = 'recovery'
    STARTUP = 'startup'
    STARTUP_WITHDRAWN = 'startup-withdrawn'
    def __str__(self): return self.value
    def cycle_count(self):
        match self:
            case QualificationScope.FULL: return 100
            case QualificationScope.SHUTDOWN: return 0
            case QualificationScope.LATENCY: return 0
            case QualificationScope.LATENCY_LOAD: return 0
            case QualificationScope.IME: return 0
            case QualificationScope.VOICEOVER: return 0
            case QualificationScope.RECOVERY: return 0
            case QualificationScope.STARTUP: return 0
            case QualificationScope.STARTUP_WITHDRAWN: return 0
        raise AssertionError('Unhandled qualification scope')

class RecoveryAction(Enum):
    OUTAGE = 'outage'
    RESTORE = 'restore'
    RESTART = 'restart'
    DENY = 'deny'
    ALLOW = 'allow'

class LatencyStatus(Enum):
    MEASURED = 'MEASURED'
    OVERFLOW = 'CAPTURE_OVERFLOW_NO_VERDICT'

class NativeInputCommand(Enum):
    COMPOSE = 'compose'
    RESTORE = 'restore'

class NativeReaderCommand(Enum):
    QUALIFY = 'qualify'
    RESTORE = 'restore'

class Appearance(Enum):
    LIGHT = 'light'
    DARK = 'dark'
    def __str__(self): return self.value

class TextScale(Enum):
    STANDARD = 'standard'
    DOUBLE = 'double'
    def __str__(self): return self.value

class BuildProfile(Enum):
    DEBUG = 'debug'
    RELEASE = 'release'
    def __str__(self): return self.value
    def arguments(self):
        match self:
            case BuildProfile.DEBUG: return []
            case BuildProfile.RELEASE: return ['--release']
        raise AssertionError('Unhandled build profile')


def qualify(profile, retained_server, scope, appearance, text_scale, retained_app=None):
    if platform.system() != 'Darwin' or not os.environ.get('CARGO_TARGET_DIR'):
        raise RuntimeError('Native macOS proof must run through the frontend build reservation')
    proof = Path(CONFIG['frontend_cache']) / 'nhf023' / ('desktop-resources-' + str(time.time_ns()))
    proof.mkdir(mode=0o700)
    shutil.copy2(__file__, proof / 'driver.py')
    def run(argv, **kwargs):
        return subprocess.run(list(map(str, argv)), cwd=ROOT, check=True, **kwargs)
    server = proof / 'server.test'
    if retained_server is None:
        run(['go','test','-race','-c','-o',server,'./internal/server'])
    else:
        if not retained_server.is_absolute() or not retained_server.is_file():
            raise RuntimeError('Retained server binary must exist at an absolute path')
        shutil.copy2(retained_server, server)
    if retained_app is None:
        run(['cargo','build','--locked','-p','homeward-app','--bin','homeward-desktop',*profile.arguments()])
        app_source = Path(os.environ['CARGO_TARGET_DIR']) / profile.value / 'homeward-desktop'
    else:
        if not retained_app.is_absolute() or not retained_app.is_file() or not os.access(retained_app, os.X_OK):
            raise RuntimeError('Retained app binary must be executable at an absolute path')
        app_source = retained_app
    helper = proof / 'native-ui'
    modules = Path(CONFIG['frontend_cache']) / 'session-spike/swift-modules'
    run(['xcrun','swiftc','-module-cache-path',modules,ROOT/'qualification/frontend/native-ui.swift','-o',helper])
    latency_helper = None
    if scope in (QualificationScope.LATENCY, QualificationScope.LATENCY_LOAD):
        latency_helper = proof / 'native-latency'
        run(['xcrun','swiftc','-O','-parse-as-library','-module-cache-path',modules,
             ROOT/'qualification/frontend/native-latency.swift','-o',latency_helper])
    input_helper = None
    if scope is QualificationScope.IME:
        input_helper = proof / 'native-input'
        run(['xcrun','swiftc','-O','-parse-as-library','-module-cache-path',modules,
             ROOT/'qualification/frontend/native-input.swift','-o',input_helper])
    reader_helper = None
    if scope is QualificationScope.VOICEOVER:
        reader_helper = proof / 'native-voiceover'
        run(['xcrun','swiftc','-O','-parse-as-library','-module-cache-path',modules,
             ROOT/'qualification/frontend/native-voiceover.swift','-o',reader_helper])
    bundle = proof / 'Homeward Resources.app/Contents'
    (bundle / 'MacOS').mkdir(parents=True)
    binary = bundle / 'MacOS/homeward-desktop'
    shutil.copy2(app_source, binary)
    with (bundle/'Info.plist').open('wb') as output:
        plistlib.dump(dict(CFBundleExecutable=binary.name,CFBundleIdentifier=SESSION['PACKAGE'],CFBundleName='Homeward Resources',CFBundlePackageType='APPL',NSHighResolutionCapable=True),output)
    write = SESSION['write']
    artifacts = [binary,server,helper] + ([latency_helper] if latency_helper else []) + ([input_helper] if input_helper else []) + ([reader_helper] if reader_helper else [])
    write(proof/'build.json',dict(binary=str(binary),server=str(server),helper=str(helper),profile=profile.value,
        retained_app=str(retained_app) if retained_app else None,
        retained_app_requires_original_build_provenance=retained_app is not None,
        binaries={str(p.relative_to(proof)):SESSION['digest'](p) for p in artifacts}))
    write(proof/'machine.json',dict(os=platform.platform(),architecture=platform.machine(),model=subprocess.check_output(['sysctl','-n','hw.model'],text=True).strip(),ram_bytes=int(subprocess.check_output(['sysctl','-n','hw.memsize'],text=True)),cpu=subprocess.check_output(['sysctl','-n','machdep.cpu.brand_string'],text=True).strip(),physical_android=False))
    product = Path.home()/'Library/Application Support/Homeward'
    public, preferences = product/'endpoint.json', product/'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve existing product endpoint/preferences')
    if any(Path(p.strip()).name == 'homeward-desktop' for p in subprocess.check_output(['ps','-axo','comm'],text=True).splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    app = None
    written = False
    owned_preferences_digest = None
    namespace = None
    try:
        with SESSION['fixture'](proof/'desktop',server,kind=SESSION['FixtureKind'].CACHE,profile=SESSION['UIProfile'].HOME) as (private,control):
            namespace = SESSION['namespace'](private)
            found=subprocess.run(['security','find-generic-password','-s','ca.homeward.session.v1','-a',namespace],capture_output=True,timeout=10)
            if found.returncode != 44:
                raise RuntimeError('Preserve existing or unreadable synthetic credential namespace')
            product.mkdir(parents=True,exist_ok=True)
            write(public, SESSION['public_profile'](private))
            written = True
            with (proof/'app.log').open('wb') as log:
                launched = time.monotonic()
                app=subprocess.Popen([str(binary)],stdout=log,stderr=subprocess.STDOUT)
                def native(command, **fields):
                    if not isinstance(command,Command): raise TypeError('Typed native command required')
                    try:
                        result=run([helper,'-'],input=json.dumps(dict(pid=app.pid,command=command.value,syntheticOnly=True,**fields)),capture_output=True,text=True,timeout=10)
                    except subprocess.CalledProcessError as error:
                        # Do not retain input fields, particularly login data.
                        diagnostic=dict(command=command.value,returncode=error.returncode,stderr=error.stderr[-4096:])
                        write(proof/('native-failure-'+command.value+'-'+str(time.time_ns())+'.json'),diagnostic)
                        print('NATIVE_FAILURE '+json.dumps(diagnostic),flush=True)
                        raise
                    return json.loads(result.stdout) if result.stdout.strip() else None
                def tree(): return native(Command.TREE)
                def wait(label,timeout=25):
                    deadline=time.monotonic()+timeout
                    while True:
                        try:
                            rows=tree()
                            if any(label in str(row.get(key,'')) for row in rows for key in ['title','description','text']): return rows
                        except subprocess.CalledProcessError:
                            pass
                        if app.poll() is not None or time.monotonic()>=deadline:
                            if app.poll() is None:
                                capture('missing-label-before-fixture-close')
                            raise RuntimeError('Native label missing: '+label)
                        time.sleep(.1)
                def capture(name):
                    info=native(Command.INFO)
                    run(['screencapture','-x','-o','-l',str(info['window_id']),proof/(name+'.png')],capture_output=True)
                    write(proof/(name+'.tree.json'),tree())
                def sample():
                    raw=subprocess.check_output(['ps','-p',str(app.pid),'-o','rss=,time='],text=True).split()
                    if len(raw)!=2: raise RuntimeError('Owned app resource sample missing')
                    seconds=0.0
                    for field in raw[1].split(':'): seconds=seconds*60+float(field)
                    return dict(monotonic_seconds=time.monotonic(),rss_bytes=int(raw[0])*1024,cpu_seconds=seconds)
                def idle_window():
                    samples=[sample()]
                    for _ in range(20): time.sleep(1); samples.append(sample())
                    elapsed=samples[-1]['monotonic_seconds']-samples[0]['monotonic_seconds']
                    return dict(samples=samples,mean_rss_bytes=statistics.mean(s['rss_bytes'] for s in samples),cpu_percent=100*(samples[-1]['cpu_seconds']-samples[0]['cpu_seconds'])/elapsed,seconds=elapsed)
                wait('Se connecter')
                write(proof/'initial-window.json',dict(accessibility_ready_seconds=time.monotonic()-launched,cold_cached_view_budget=False))
                native(Command.FILL_LOGIN,loginName=private['login_name'],password=private['password'])
                wait('Afficher ma maison')
                native(Command.READ)
                wait(private['expected_first_alias'])
                native(Command.NAVIGATE,section=NavigationSection.HOME.value)
                wait('Ma maison'); wait('Confort principal')
                if appearance is Appearance.DARK or text_scale is TextScale.DOUBLE:
                    native(Command.NAVIGATE,section=NavigationSection.SETTINGS.value); wait('À votre mesure')
                    if appearance is Appearance.DARK:
                        native(Command.TAP,label='Thème sombre'); wait('Thème clair')
                    if text_scale is TextScale.DOUBLE:
                        native(Command.TAP,label='Texte à 200 %'); wait('Texte standard')
                    deadline=time.monotonic()+5
                    while not preferences.is_file():
                        if time.monotonic()>=deadline: raise RuntimeError('Owned UI preferences were not persisted')
                        time.sleep(.05)
                    owned_preferences_digest=SESSION['digest'](preferences)
                    capture('styled-preferences-before-home')
                    native(Command.NAVIGATE,section=NavigationSection.HOME.value); wait('Ma maison')
                capture('home-before-cycles')
                baseline=idle_window() if scope not in (QualificationScope.IME,QualificationScope.VOICEOVER,QualificationScope.RECOVERY,QualificationScope.STARTUP,QualificationScope.STARTUP_WITHDRAWN) else None
                write(proof/'baseline-resources.json',baseline)
                cycles=[]
                for cycle in range(1,scope.cycle_count()+1):
                    started=time.monotonic()
                    native(Command.MINIMIZE)
                    hidden=native(Command.LIFECYCLE)
                    if not hidden['minimized'] or hidden['on_screen']: raise RuntimeError('Window did not actually minimize')
                    time.sleep(.2)
                    native(Command.RESTORE)
                    wait('Ma maison'); wait('Confort principal')
                    visible=native(Command.LIFECYCLE)
                    if visible['minimized'] or not visible['on_screen']: raise RuntimeError('Window did not actually restore')
                    cycles.append(dict(cycle=cycle,observed_seconds=time.monotonic()-started,hidden=hidden,visible=visible,resources=sample()))
                    with (proof/'cycles.jsonl').open('a') as output:
                        output.write(json.dumps(cycles[-1])+'\n')
                    if cycle%10==0: print('CYCLES_PASS '+str(cycle),flush=True)
                after=idle_window() if cycles else baseline
                write(proof/'after-cycles-resources.json',after)
                write(proof/'cycles.json',cycles)
                capture('home-after-'+str(len(cycles))+'-cycles')
                load = None
                loaded = None
                latency = None
                composition = None
                reader_result = None
                recovery = None
                startup = None
                if scope in (QualificationScope.RECOVERY,QualificationScope.STARTUP,QualificationScope.STARTUP_WITHDRAWN):
                    checkpoints = []
                    sequence = 0
                    def control_recovery(action):
                        nonlocal sequence
                        if not isinstance(action, RecoveryAction): raise TypeError('Typed recovery action required')
                        sequence += 1
                        command = control/'command.writing'
                        write(command,dict(sequence=sequence,action=action.value)); command.replace(control/'command.json')
                        acknowledgement = control/f'ack-{sequence:02d}.json'
                        deadline = time.monotonic()+30
                        while not acknowledgement.is_file():
                            # Startup changes rights while the old app is
                            # deliberately closed. The isolated Go fixture,
                            # not that process, acknowledges this command.
                            if time.monotonic()>=deadline:
                                raise RuntimeError('Synthetic recovery command was not acknowledged')
                            time.sleep(.05)
                        result = json.loads(acknowledgement.read_text())
                        if result['sequence']!=sequence or result['action']!=action.value:
                            raise RuntimeError('Out of order recovery acknowledgement')
                        write(proof/f'recovery-ack-{sequence:02d}.json',result)
                    def checkpoint(name):
                        capture(name)
                        checkpoints.append(dict(name=name,resources=sample()))
                        write(proof/(name+'.json'),checkpoints[-1])
                if scope in (QualificationScope.STARTUP,QualificationScope.STARTUP_WITHDRAWN):
                    native(Command.CLOSE)
                    if app.wait(timeout=15)!=0: raise RuntimeError('Pre-startup application did not close normally')
                    if scope is QualificationScope.STARTUP_WITHDRAWN:
                        control_recovery(RecoveryAction.DENY)
                    with (proof/'reopened-app.log').open('wb') as reopened_log:
                        reopened_at=time.monotonic()
                        app=subprocess.Popen([str(binary)],stdout=reopened_log,stderr=subprocess.STDOUT)
                    # No read callback, mouse action or injected access token is
                    # used between process launch and the first fresh view.
                    if scope is QualificationScope.STARTUP:
                        wait('États à jour'); wait('Confort principal')
                        capture('startup-fresh-view')
                        upper_bound=time.monotonic()-reopened_at
                        startup=dict(fresh_view_upper_bound_seconds=upper_bound,
                            provisional_two_second_target_met_on_this_host=upper_bound<=2,
                            automatic_read=True,session_restored_from_os=True,
                            private_state_disk_cache=False,physical_android=False,
                            method='Process launch through completion of owned-window screenshot and AX status/data confirmation; conservative upper bound, not first-frame timing or OS disk-cache eviction')
                    else:
                        # With no remaining entity grants, OpenView returns a
                        # successful empty authorized capture, not an auth error.
                        wait('Aucun élément autorisé dans cette vue.')
                        rows=tree()
                        if any(private['expected_first_alias'] in str(row.get(key,'')) or 'Confort principal' in str(row.get(key,'')) for row in rows for key in ['title','description','text']):
                            raise RuntimeError('Startup disclosed a former protected projection after denial')
                        capture('startup-withdrawn-view-cleared')
                        native(Command.MINIMIZE)
                        hidden=native(Command.LIFECYCLE)
                        if not hidden['minimized'] or hidden['on_screen']: raise RuntimeError('Withdrawn startup did not minimize')
                        control_recovery(RecoveryAction.ALLOW)
                        native(Command.RESTORE)
                        wait('États à jour'); wait('Confort principal')
                        visible=native(Command.LIFECYCLE)
                        if visible['minimized'] or not visible['on_screen']: raise RuntimeError('Regranted startup did not restore')
                        capture('startup-regranted-fresh-view')
                        startup=dict(withdrawn_startup_has_empty_authorized_view=True,
                            fresh_view_after_regrant_and_native_resume=True,hidden=hidden,visible=visible,
                            automatic_read=True,private_state_disk_cache=False,physical_android=False,
                            fresh_view_upper_bound_seconds=None)
                    write(proof/'startup.json',startup)
                if scope is QualificationScope.RECOVERY:
                    native(Command.NAVIGATE,section=NavigationSection.INVENTORY.value)
                    wait('États à jour')
                    native(Command.CHECK_ROW,expectedRowLabel=private['expected_first_alias'])
                    checkpoint('recovery-live-before-outage')
                    try:
                        control_recovery(RecoveryAction.OUTAGE)
                        wait('Hors ligne · dernières données reçues')
                        native(Command.CHECK_ROW,expectedRowLabel=private['expected_first_alias'])
                        checkpoint('recovery-dated-view-offline')
                    finally:
                        # Restore only this isolated fixture, also after a failed UI assertion.
                        control_recovery(RecoveryAction.RESTORE)
                    wait('États à jour')
                    native(Command.CHECK_ROW,expectedRowLabel=private['expected_first_alias'])
                    checkpoint('recovery-live-after-outage')
                    control_recovery(RecoveryAction.RESTART)
                    # An actual OS suspension/resume forces a fresh capture after
                    # the acknowledged Go Run replacement. A transient live label
                    # alone cannot prove that the new epoch was consumed.
                    native(Command.MINIMIZE)
                    hidden=native(Command.LIFECYCLE)
                    if not hidden['minimized'] or hidden['on_screen']: raise RuntimeError('Recovery window did not minimize')
                    native(Command.RESTORE)
                    wait('États à jour',timeout=35)
                    native(Command.CHECK_ROW,expectedRowLabel=private['expected_first_alias'])
                    visible=native(Command.LIFECYCLE)
                    if visible['minimized'] or not visible['on_screen']: raise RuntimeError('Recovery window did not restore')
                    checkpoint('recovery-live-after-server-restart-and-resume')
                    native(Command.NAVIGATE,section=NavigationSection.HOME.value)
                    wait('Ma maison'); wait('Confort principal')
                    recovery=dict(dated_view_during_outage=True,live_after_restore=True,
                        fresh_view_after_server_restart_and_native_resume=True,
                        hidden=hidden,visible=visible,checkpoints=checkpoints,
                        rights_withdrawal_qualified=False,refresh_qualified=False,
                        rotation_qualified=False,physical_android=False)
                    write(proof/'recovery-checkpoints.json',checkpoints)
                if scope is QualificationScope.FULL:
                    # StateService load is real and lasts50s. Navigate an observed
                    # server-configured room and back while the update stream runs.
                    command=control/'command.writing'
                    write(command,dict(sequence=1,action=SESSION['CacheAction'].LOAD.value)); command.replace(control/'command.json')
                    deadline=time.monotonic()+25
                    while True:
                        candidates=[row for row in tree() if row['role']=='AXButton' and any(str(row.get(k,'')).startswith('Salon, ') for k in ['title','description']) and row['height']>5]
                        if len(candidates)==1:
                            row=candidates[0]; info=native(Command.INFO); center=row['y']+row['height']/2-info['bounds']['Y']
                            if 40<center<info['bounds']['Height']-40: break
                        if time.monotonic()>=deadline: raise RuntimeError('Observed synthetic room action missing')
                        native(Command.SCROLL,delta=-300)
                    label=next(row[k] for k in ['title','description'] if str(row.get(k,'')).startswith('Salon, '))
                    native(Command.TAP,label=label); wait('Retour'); capture('room-under-load')
                    native(Command.TAP,label='Retour'); wait('Ma maison')
                    deadline=time.monotonic()+70
                    while not (control/'load-done.json').is_file():
                        if app.poll() is not None or time.monotonic()>=deadline: raise RuntimeError('Load did not finish')
                        time.sleep(.25)
                    load=json.loads((control/'load-done.json').read_text())
                    if load['updates']!=1000 or not 49<=load['elapsed_seconds']<=70: raise RuntimeError('Expected20Hz load missing')
                    write(proof/'load.json',load)
                    capture('home-after-load')
                    loaded=idle_window()
                    native(Command.NAVIGATE,section=NavigationSection.INVENTORY.value)
                    wait(private['expected_first_alias'])
                    native(Command.CHECK_ROW,expectedRowLabel=private['expected_first_alias'])
                    capture('inventory-after-load')
                    write(proof/'after-load-resources.json',loaded)
                if scope is QualificationScope.LATENCY_LOAD:
                    command=control/'command.writing'
                    write(command,dict(sequence=1,action=SESSION['CacheAction'].LOAD.value)); command.replace(control/'command.json')
                    deadline=time.monotonic()+10
                    while not (control/'load-started.json').is_file():
                        if app.poll() is not None or time.monotonic()>=deadline: raise RuntimeError('Actual server load did not start')
                        time.sleep(.05)
                    started=json.loads((control/'load-started.json').read_text())
                    if started!={'updates':1000,'interval_milliseconds':50} or (control/'load-done.json').exists():
                        raise RuntimeError('Expected ongoing20Hz server load missing')
                    write(proof/'load-started-before-latency.json',started)
                if scope in (QualificationScope.LATENCY, QualificationScope.LATENCY_LOAD):
                    try:
                        with (proof/'latency-stdout.json').open('w') as output, (proof/'latency-stderr.log').open('w') as errors:
                            run([latency_helper],input=json.dumps(dict(pid=app.pid,syntheticOnly=True,outputDirectory=str(proof/'latency'))),
                                text=True,stdout=output,stderr=errors,timeout=60)
                        latency=json.loads((proof/'latency/result.json').read_text())
                        if LatencyStatus(latency['status']) is not LatencyStatus.MEASURED or latency['samples']!=60 or latency['capture_overflow']!=0:
                            raise RuntimeError('Compositor latency scope did not produce an admitted measurement')
                        capture('home-after-latency')
                    except BaseException:
                        capture('latency-failure-before-fixture-close')
                        raise
                if scope is QualificationScope.LATENCY_LOAD:
                    if (control/'load-done.json').exists(): raise RuntimeError('Latency proof overran the actual load window')
                    write(proof/'load-still-running-after-latency.json',dict(load_done=False))
                    deadline=time.monotonic()+70
                    while not (control/'load-done.json').is_file():
                        if app.poll() is not None or time.monotonic()>=deadline: raise RuntimeError('Actual20Hz load did not finish')
                        time.sleep(.25)
                    load=json.loads((control/'load-done.json').read_text())
                    if load['updates']!=1000 or not 49<=load['elapsed_seconds']<=70: raise RuntimeError('Expected20Hz load missing')
                    write(proof/'load.json',load)
                    capture('home-after-latency-load')
                if scope is QualificationScope.IME:
                    try:
                        with (proof/'input-stdout.json').open('w') as output, (proof/'input-stderr.log').open('w') as errors:
                            process=subprocess.Popen([str(input_helper)],cwd=ROOT,stdin=subprocess.PIPE,stdout=output,stderr=errors,text=True)
                            try:
                                process.communicate(json.dumps(dict(command=NativeInputCommand.COMPOSE.value,pid=app.pid,syntheticOnly=True,outputDirectory=str(proof/'input'))),timeout=30)
                            except subprocess.TimeoutExpired:
                                process.terminate()
                                try: process.wait(timeout=5)
                                except subprocess.TimeoutExpired: process.kill(); process.wait()
                                raise
                            if process.returncode!=0: raise RuntimeError('Native IME composition failed; inspect owned input diagnostics')
                        composition=json.loads((proof/'input/result.json').read_text())
                        capture('home-after-ime')
                    except BaseException:
                        if (proof/'input/source-lease.json').is_file():
                            run([input_helper],input=json.dumps(dict(command=NativeInputCommand.RESTORE.value,outputDirectory=str(proof/'input'))),text=True,timeout=10)
                        capture('input-failure-before-fixture-close')
                        raise
                if scope is QualificationScope.VOICEOVER:
                    try:
                        payload=dict(command=NativeReaderCommand.QUALIFY.value,pid=app.pid,syntheticOnly=True,outputDirectory=str(proof/'reader'))
                        with (proof/'reader-stdout.json').open('w') as output, (proof/'reader-stderr.log').open('w') as errors:
                            process=subprocess.Popen([str(reader_helper)],cwd=ROOT,stdin=subprocess.PIPE,stdout=output,stderr=errors,text=True)
                            try: process.communicate(json.dumps(payload),timeout=45)
                            except subprocess.TimeoutExpired:
                                process.terminate()
                                try: process.wait(timeout=8)
                                except subprocess.TimeoutExpired: process.kill(); process.wait()
                                raise
                            if process.returncode!=0: raise RuntimeError('Actual VoiceOver qualification failed; inspect owned reader diagnostics')
                        reader_result=json.loads((proof/'reader/result.json').read_text())
                        capture('after-native-reader')
                    except BaseException:
                        if (proof/'reader/reader-lease.json').is_file():
                            run([reader_helper],input=json.dumps(dict(command=NativeReaderCommand.RESTORE.value,pid=app.pid,syntheticOnly=True,outputDirectory=str(proof/'reader'))),text=True,timeout=10)
                        capture('reader-failure-before-fixture-close')
                        raise
                native(Command.NAVIGATE,section=NavigationSection.SETTINGS.value); wait('À votre mesure')
                capture('preferences-before-shutdown')
                # Settings now contains companion, notifications and account
                # sections. Find the actual visible action through its scroll
                # area instead of assuming logout is in the first AX viewport.
                for attempt in range(20):
                    rows=tree(); info=native(Command.INFO)
                    buttons=[row for row in rows if row['role']=='AXButton' and any(row.get(k)=='Se déconnecter' for k in ['title','description'])]
                    if any(row['height']>5 and 40<row['y']+row['height']/2-info['bounds']['Y']<info['bounds']['Height']-40 for row in buttons):
                        break
                    native(Command.SCROLL,delta=-500)
                else:
                    capture('logout-action-missing-before-fixture-close')
                    raise RuntimeError('Visible native logout action missing after bounded settings scroll')
                native(Command.TAP,label='Se déconnecter'); wait('Se connecter'); capture('signed-out')
                native(Command.CLOSE)
                if app.wait(timeout=15)!=0: raise RuntimeError('Native app did not close normally')
        report=json.loads((control/'result.json').read_text())
        if report['command_driver_entries']!=0 or not report['server_joined'] or report['state_read_ok']<1+scope.cycle_count() or report['state_release_ok']<1+scope.cycle_count():
            raise RuntimeError('Fresh reads, pagination cleanup and read-only shutdown not proved')
        if scope is QualificationScope.RECOVERY:
            if report['servers_started']!=2 or report['servers_joined']!=2 or report['connections_remaining']!=0 or report['state_read_ok']<2 or report['state_release_ok']<2 or report['action_rpc_calls']!=0 or report['revoke_ok']!=1:
                raise RuntimeError('Recovery did not prove joined server replacement, fresh reads and zero actions')
        if scope in (QualificationScope.STARTUP,QualificationScope.STARTUP_WITHDRAWN):
            correct_reads = report['state_read_calls']==report['state_read_ok']==report['state_release_ok']
            enough_reads = report['state_read_ok']==2 if scope is QualificationScope.STARTUP else report['state_read_ok']>=3
            if not correct_reads or not enough_reads or report['action_rpc_calls']!=0 or report['revoke_ok']!=1 or report['start_login_calls']!=1 or report['connections_remaining']!=0:
                raise RuntimeError('Startup did not prove exact automatic reads, denial/cleanup and zero replay')
        write(proof/'result.json',dict(status='PASS_PENDING_VISUAL_INSPECTION',profile=profile.value,qualification_scope=scope.value,appearance=appearance.value,text_scale=text_scale.value,actual_native_cycles=len(cycles),entity_count=private['entity_count'],load=load,baseline=baseline,after_cycles=after,after_load=loaded,cycles=cycles,server=report,children_joined=True,
            cases=[dict(platform='macos',native_rendering=True,children_joined=True)],
            scope=f'Native macOS {profile.value}, {scope.value} scope, isolated Go-race TLS; load, cycles and compositor latency only when explicitly recorded; no physical tablet result',
            provisional_rss_target_met_on_this_host=after['mean_rss_bytes']<=180*1024*1024 if after else None,
            rss_change_after_warmup_bytes=(statistics.mean(row['resources']['rss_bytes'] for row in cycles[-10:])-statistics.mean(row['resources']['rss_bytes'] for row in cycles[9:19])) if cycles else None,
            physical_tablet=False,input_to_present_p95_ms=latency['p95_ms'] if latency else None,compositor_latency=latency,
            native_composition=composition,
            native_reader=reader_result,
            native_recovery=recovery,
            native_startup=startup,
            provisional_input_target_met_on_this_host=latency['p95_ms']<=80 if latency else None,
            cold_cached_view_startup_seconds=None,all_nhf023_acceptance_closed=False))
        print('DESKTOP_RESOURCES_PASS '+str(proof),flush=True)
    except BaseException as error:
        failure=dict(status='FAILED',error_type=type(error).__name__,error=str(error),cycles_completed=len(cycles) if 'cycles' in locals() else 0)
        if app is not None:
            failure['app_exit_before_cleanup']=app.poll()
            if app.poll() is None and 'native' in locals():
                with contextlib.suppress(Exception):
                    life=native(Command.LIFECYCLE)
                    failure['actual_window_state']=life
                    if not life['minimized']: capture('failure')
        if not (proof/'result.json').exists(): write(proof/'result.json',failure)
        raise
    finally:
        SESSION['TRANSPORT']['stop'](app)
        errors=[]
        if written and public.exists():
            current=json.loads(public.read_text())
            if current==SESSION['public_profile'](private): public.unlink()
            else: errors.append('Preserve externally changed product profile')
        if namespace is not None:
            deleted=subprocess.run(['security','delete-generic-password','-s','ca.homeward.session.v1','-a',namespace],capture_output=True,timeout=10)
            if deleted.returncode not in (0,44): errors.append('Synthetic credential cleanup unconfirmed')
        if owned_preferences_digest is not None and preferences.exists():
            if SESSION['digest'](preferences)==owned_preferences_digest: preferences.unlink()
            else: errors.append('Preserve externally changed UI preferences')
        if errors: raise RuntimeError('; '.join(errors))

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profile',type=BuildProfile,choices=list(BuildProfile),default=BuildProfile.RELEASE)
    parser.add_argument('--server-binary',type=Path)
    parser.add_argument('--app-binary',type=Path,help='Reuse an exact executable; retain its original build manifest, not a claim that current sources built it')
    parser.add_argument('--scope',type=QualificationScope,choices=list(QualificationScope),default=QualificationScope.FULL)
    parser.add_argument('--appearance',type=Appearance,choices=list(Appearance),default=Appearance.LIGHT)
    parser.add_argument('--text-scale',type=TextScale,choices=list(TextScale),default=TextScale.STANDARD)
    args=parser.parse_args()
    qualify(args.profile,args.server_binary,args.scope,args.appearance,args.text_scale,args.app_binary)
