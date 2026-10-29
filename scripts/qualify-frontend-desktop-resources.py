"""Native desktop release resources and 100 actual minimize/restore cycles.

Run through frontend.py run --cargo. Isolated Go-race TLS/core and OS input only.
No video, physical-tablet, cold-cache startup or input-to-present verdict.
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

class QualificationScope(Enum):
    FULL = 'full'
    SHUTDOWN = 'shutdown'
    def __str__(self): return self.value
    def cycle_count(self):
        match self:
            case QualificationScope.FULL: return 100
            case QualificationScope.SHUTDOWN: return 0
        raise AssertionError('Unhandled qualification scope')

class BuildProfile(Enum):
    DEBUG = 'debug'
    RELEASE = 'release'
    def __str__(self): return self.value
    def arguments(self):
        match self:
            case BuildProfile.DEBUG: return []
            case BuildProfile.RELEASE: return ['--release']
        raise AssertionError('Unhandled build profile')


def qualify(profile, retained_server, scope):
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
    run(['cargo','build','--locked','-p','homeward-app','--bin','homeward-desktop',*profile.arguments()])
    helper = proof / 'native-ui'
    modules = Path(CONFIG['frontend_cache']) / 'session-spike/swift-modules'
    run(['xcrun','swiftc','-module-cache-path',modules,ROOT/'qualification/frontend/native-ui.swift','-o',helper])
    bundle = proof / 'Homeward Resources.app/Contents'
    (bundle / 'MacOS').mkdir(parents=True)
    binary = bundle / 'MacOS/homeward-desktop'
    os.link(Path(os.environ['CARGO_TARGET_DIR']) / profile.value / 'homeward-desktop', binary)
    with (bundle/'Info.plist').open('wb') as output:
        plistlib.dump(dict(CFBundleExecutable=binary.name,CFBundleIdentifier=SESSION['PACKAGE'],CFBundleName='Homeward Resources',CFBundlePackageType='APPL',NSHighResolutionCapable=True),output)
    write = SESSION['write']
    write(proof/'build.json',dict(binary=str(binary),server=str(server),helper=str(helper),profile=profile.value,binaries={str(p.relative_to(proof)):SESSION['digest'](p) for p in [binary,server,helper]}))
    write(proof/'machine.json',dict(os=platform.platform(),architecture=platform.machine(),model=subprocess.check_output(['sysctl','-n','hw.model'],text=True).strip(),ram_bytes=int(subprocess.check_output(['sysctl','-n','hw.memsize'],text=True)),cpu=subprocess.check_output(['sysctl','-n','machdep.cpu.brand_string'],text=True).strip(),physical_android=False))
    product = Path.home()/'Library/Application Support/Homeward'
    public, preferences = product/'endpoint.json', product/'interface.json'
    if public.exists() or preferences.exists():
        raise RuntimeError('Preserve existing product endpoint/preferences')
    if any(Path(p.strip()).name == 'homeward-desktop' for p in subprocess.check_output(['ps','-axo','comm'],text=True).splitlines()[1:]):
        raise RuntimeError('Preserve existing Homeward process')
    app = None
    written = False
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
                native(Command.TAP,label='Accueil')
                wait('Ma maison'); wait('Confort principal')
                capture('home-before-cycles')
                baseline=idle_window()
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
                    native(Command.TAP,label='Inventaire')
                    wait(private['expected_first_alias'])
                    native(Command.CHECK_ROW,expectedRowLabel=private['expected_first_alias'])
                    capture('inventory-after-load')
                    write(proof/'after-load-resources.json',loaded)
                native(Command.TAP,label='Préférences'); wait('À votre mesure')
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
        write(proof/'result.json',dict(status='PASS_PENDING_VISUAL_INSPECTION',profile=profile.value,qualification_scope=scope.value,actual_native_cycles=len(cycles),entity_count=private['entity_count'],load=load,baseline=baseline,after_cycles=after,after_load=loaded,cycles=cycles,server=report,children_joined=True,
            cases=[dict(platform='macos',native_rendering=True,children_joined=True)],
            scope=f'Native macOS {profile.value}, {scope.value} scope, isolated Go-race TLS; load and cycles only when explicitly recorded; no physical tablet or input-to-present result',
            provisional_rss_target_met_on_this_host=after['mean_rss_bytes']<=180*1024*1024,
            rss_change_after_warmup_bytes=(statistics.mean(row['resources']['rss_bytes'] for row in cycles[-10:])-statistics.mean(row['resources']['rss_bytes'] for row in cycles[9:19])) if cycles else None,
            physical_tablet=False,input_to_present_p95_ms=None,cold_cached_view_startup_seconds=None,all_nhf023_acceptance_closed=False))
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
        if errors: raise RuntimeError('; '.join(errors))

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profile',type=BuildProfile,choices=list(BuildProfile),default=BuildProfile.RELEASE)
    parser.add_argument('--server-binary',type=Path)
    parser.add_argument('--scope',type=QualificationScope,choices=list(QualificationScope),default=QualificationScope.FULL)
    args=parser.parse_args()
    qualify(args.profile,args.server_binary,args.scope)
