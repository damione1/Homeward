from enum import Enum
import argparse,hashlib,json,os,plistlib,runpy,subprocess,time
from pathlib import Path
root=Path(__file__).resolve().parents[1]
config=json.loads((root/'frontend.local.json').read_text())
pro=Path(config['pro_kit'])
base=Path(config['frontend_cache'])/'charts'
base.mkdir(parents=True,exist_ok=True)
class Domain(Enum):
 MACHINE=('charts-machine','Machine telemetry','Worker A load','%')
 PROCESSING=('charts-processing','Processing delays','Parsing latency','ms')
class Case(Enum):
 MACHINE=(Domain.MACHINE,False)
 MACHINE_DOUBLE=(Domain.MACHINE,True)
 PROCESSING=(Domain.PROCESSING,False)
 PROCESSING_DOUBLE=(Domain.PROCESSING,True)
def parse_case(value):
 try:return Case[value]
 except KeyError:raise argparse.ArgumentTypeError('expected one of '+', '.join(c.name for c in Case)) from None
parser=argparse.ArgumentParser(description='Synthetic native chart qualification; no server or devices')
parser.add_argument('--case',type=parse_case,default=Case.MACHINE)
case=parser.parse_args().case
domain,double=case.value
binary,heading,primary,unit=domain.value
proof=base/(case.name.lower()+'-'+str(time.time_ns()));proof.mkdir(mode=0o700)
(proof/'harness.py').write_bytes(Path(__file__).read_bytes())
config=json.loads((root/'frontend.local.json').read_text())
module=runpy.run_path(str(root/'scripts/qualify-frontend-environment.py'))
before=module['source_inventory'](config)
(proof/'sources.json').write_text(json.dumps(before,indent=2)+'\n')
def sha(path):
 with path.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
subprocess.run(['cargo','build','--locked','-p','slint-kit-pro-examples','--bin',binary],cwd=pro,check=True)
helper=proof/'native-ui';source=proof/'native-ui.swift';source.write_bytes((root/'qualification/frontend/native-ui.swift').read_bytes())
subprocess.run(['xcrun','swiftc','-module-cache-path',str(Path(config['frontend_cache'])/'session-spike/swift-modules'),str(source),'-o',str(helper)],check=True)
class Command(Enum):
 INFO='info';FOCUS='focus';TREE='tree';TAP='tap';KEY='key';CLOSE='close';CANVAS='canvas';SCROLL='scroll'
class Key(Enum):HOME='home';END='end';SHIFT_LEFT='shift-left';ESCAPE='escape'
class Gesture(Enum):INSPECT='inspect';SELECT='select'
contents=proof/'Charts.app/Contents';(contents/'MacOS').mkdir(parents=True)
exe=contents/'MacOS'/binary;os.link(Path(os.environ['CARGO_TARGET_DIR'])/'debug'/binary,exe)
with (contents/'Info.plist').open('wb') as file:plistlib.dump(dict(CFBundleExecutable=binary,CFBundleIdentifier='ca.slintkit.pro.charts.poc',CFBundleName='Pro Charts',CFBundlePackageType='APPL',NSHighResolutionCapable=True),file)
app=None
with (proof/'app.log').open('wb') as log:
 try:
  app=subprocess.Popen([str(exe)]+(['--text-200'] if double else []),stdout=log,stderr=subprocess.STDOUT,env=dict(os.environ,SLINT_BACKEND='winit-software'))
  def native(command,**fields):
   assert isinstance(command,Command)
   deadline=time.monotonic()+3
   while True:
    result=subprocess.run([str(helper),'-'],input=json.dumps(dict(command=command.value,pid=app.pid,syntheticOnly=True,**fields)),capture_output=True,text=True,timeout=10)
    if result.returncode==0:break
    with (proof/'native-observation-errors.jsonl').open('a') as out:out.write(json.dumps(dict(command=command.value,pid=app.pid,process_returncode=app.poll(),stderr=result.stderr))+chr(10))
    if command not in {Command.INFO,Command.TREE} or app.poll() is not None or time.monotonic()>deadline:
     (proof/(command.name.lower()+'.stderr')).write_text(result.stderr)
     raise RuntimeError('native '+command.value+' failed: '+result.stderr)
    time.sleep(.1)
   return json.loads(result.stdout) if result.stdout.strip() else None
  def tree():return native(Command.TREE)
  def labels():return [value for row in tree() for value in (row['title'],row['description'],row['text'])]
  def wait(prefix):
   deadline=time.monotonic()+15
   while True:
    if any(value.startswith(prefix) for value in labels()):return
    if app.poll() is not None or time.monotonic()>deadline:
     (proof/'failed.tree.json').write_text(json.dumps(tree(),indent=2)+'\n');raise RuntimeError('missing native label '+prefix)
    time.sleep(.1)
  def capture(name):
   info=native(Command.INFO);subprocess.run(['screencapture','-x','-o','-l',str(info['window_id']),str(proof/(name+'.png'))],check=True,capture_output=True)
   (proof/(name+'.tree.json')).write_text(json.dumps(tree(),indent=2)+'\n')
  def canvas(gesture,fraction,end=None):
   assert isinstance(gesture,Gesture)
   rows=[r for r in tree() if r['role']=='AXGroup' and r['width']>100 and r['height']>100 and any(v.startswith(heading+' · ') for v in (r['title'],r['description']))]
   assert len(rows)==1,'unique chart canvas missing'
   label=next(v for v in (rows[0]['title'],rows[0]['description']) if v.startswith(heading+' · '))
   fields=dict(label=label,canvasGesture=gesture.value,fraction=fraction)
   if end is not None:fields['endFraction']=end
   native(Command.CANVAS,**fields)
  deadline=time.monotonic()+20
  while True:
   try:
    info=native(Command.INFO);assert info['accessibility_trusted'] and info['post_events_allowed'];native(Command.FOCUS);break
   except RuntimeError:
    if app.poll() is not None or time.monotonic()>deadline:raise
    time.sleep(.1)
  wait('Curve chart · two series');capture('initial')
  canvas(Gesture.INSPECT,.5);wait(primary+' · no observation · gap');capture('gap')
  native(Command.KEY,key=Key.HOME.value);wait(primary+' · original 00:00:00.000 · 0 '+unit);capture('zero')
  native(Command.KEY,key=Key.END.value);wait(primary+' · original 06:03:19.013 · 98.375 '+unit);capture('last')
  native(Command.KEY,key=Key.SHIFT_LEFT.value);wait('Selected interval ');capture('keyboard-interval')
  native(Command.KEY,key=Key.ESCAPE.value)
  canvas(Gesture.SELECT,.2,.8);wait('Selected interval ');capture('pointer-interval')
  native(Command.TAP,label='Show bars');wait('Bar chart · two series');capture('bars')
  native(Command.TAP,label='Toggle theme');time.sleep(.5)
  deadline=time.monotonic()+10
  while any(value.startswith('Preparing the updated view') for value in labels()):
   if time.monotonic()>deadline:raise RuntimeError('theme preparation did not finish')
   time.sleep(.05)
  wait('Bar chart · two series');capture('alternate-theme')
  native(Command.TAP,label='Show offline cache');wait('Offline · cache from 2026-10-26 18:00 UTC · synthetic snapshot');capture('offline')
  if double:
   native(Command.SCROLL,delta=-900);capture('double-text-inspection')
 except BaseException as error:
  (proof/'failure.json').write_text(json.dumps(dict(error=str(error),pid=app.pid if app else None,process_returncode=app.poll() if app else None),indent=2)+chr(10))
  raise
 finally:
  if app is not None and app.poll() is None:
   try:native(Command.CLOSE)
   except Exception:app.terminate()
  if app is not None:
   try:app.wait(timeout=10)
   except subprocess.TimeoutExpired:app.kill();app.wait(timeout=10)
lines=(proof/'app.log').read_text().splitlines();assert len(lines)==1,'unexpected native log content'
report=json.loads(lines[0]);assert app.returncode==0 and report['failure']=='none' and report['accepted_frames']>=3 and report['source_samples']==40000 and report['all_prepared_off_ui'] and report['worker_joined'] and report['range_commits']>=1 and report['inspected_gaps']>=1 and report['inspected_values']>=2 and report['ui_timer_gap_p95_ms']<80
assert module['source_inventory'](config)==before,'source changed during native proof'
(proof/'result.json').write_text(json.dumps(dict(status='INTERACTIONS_PASS_VISUAL_INSPECTION_PENDING',case=case.name,binary_sha256=sha(exe),helper_sha256=sha(helper),report=report,scope='native macOS winit-software chart demo; synthetic data only, no server/device; timer gap is not input-to-present latency; Android unqualified'),indent=2)+'\n')
print('NHF012 native chart smoke PASS '+str(proof),flush=True)
