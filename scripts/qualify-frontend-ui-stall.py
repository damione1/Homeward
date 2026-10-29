#!/usr/bin/env python3
"""Opt-in real TLS/cache/list adapter proof with only Slint's test loop stalled.

Use frontend.py run --cargo. No native frame, reader or physical-device claim.
"""
import argparse
from enum import Enum
import hashlib
import json
import os
from pathlib import Path
import runpy
import selectors
import shutil
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]
SESSION = runpy.run_path(str(ROOT/'scripts/qualify-frontend-session.py'))
TEST = 'list_host::stall_probe::real_tls_ui_stall_keeps_latest_projection_and_task_confirmation'


class Control(Enum):
    LOAD = 'load'
    SNAPSHOT = 'snapshot'


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def qualify(retained_server,retained_client,with_command):
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise RuntimeError('Run inside frontend.py run --cargo')
    config = json.loads((ROOT/'frontend.local.json').read_text())
    proof = Path(config['frontend_cache'])/'nhf023'/f'ui-stall-{time.time_ns()}'
    proof.mkdir(mode=0o700)
    (proof/'driver.py').write_bytes(Path(__file__).read_bytes())
    server = proof/'server.test'
    if retained_server:
        shutil.copy2(retained_server,server)
    else:
        subprocess.run(['go','test','-race','-c','-o',str(server),'./internal/server'],cwd=ROOT,check=True)
    if retained_client:
        binaries=[retained_client]
    else:
        with (proof/'build.jsonl').open('w') as output:
            subprocess.run(['cargo','test','--locked','-p','homeward-app','--lib','--no-run','--message-format=json'],cwd=ROOT,stdout=output,check=True)
        artifacts = [json.loads(line) for line in (proof/'build.jsonl').read_text().splitlines() if line.startswith('{')]
        binaries = [Path(item['executable']) for item in artifacts if item.get('reason')=='compiler-artifact'
                    and item.get('executable') and item['target']['name']=='homeward_app']
        if len(binaries)!=1: raise RuntimeError('One exact app test executable required')
    client=proof/'client.test'
    shutil.copy2(binaries[0],client)
    build=dict(server_sha256=sha(server),client_sha256=sha(client),driver_sha256=sha(proof/'driver.py'),
               retained_server=str(retained_server) if retained_server else None,
               retained_server_requires_original_provenance=bool(retained_server),
               retained_client=str(retained_client) if retained_client else None,
               retained_client_requires_original_provenance=bool(retained_client))
    (proof/'build.json').write_text(json.dumps(build,indent=2)+'\n')
    report=None
    commands=[]
    kind = SESSION['FixtureKind'].WORKLOAD_ACTIONS if with_command else SESSION['FixtureKind'].WORKLOAD
    with SESSION['fixture'](proof/'server',server,kind=kind,
                            profile=SESSION['UIProfile'].HOME) as (_,control):
        with (proof/'client.log').open('w') as log:
            child=subprocess.Popen([str(client),'--exact',TEST,'--ignored','--nocapture','--test-threads=1'],
                cwd=ROOT,env=dict(os.environ,HOMEWARD_NHF023_READY=str(control/'ready.json')),
                stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,bufsize=0)
            try:
                pending=b''; deadline=time.monotonic()+150
                with selectors.DefaultSelector() as selector:
                    selector.register(child.stdout,selectors.EVENT_READ)
                    while True:
                        if time.monotonic()>=deadline: raise RuntimeError('Stall probe deadline')
                        if not selector.select(.2):
                            if child.poll() is not None: break
                            continue
                        chunk=os.read(child.stdout.fileno(),65536)
                        if not chunk: break
                        pending+=chunk
                        while b'\n' in pending:
                            raw,pending=pending.split(b'\n',1)
                            line=raw.decode(); log.write(line+'\n'); log.flush()
                            if 'NHF023_STALL_CONTROL ' in line:
                                request=json.loads(line.split('NHF023_STALL_CONTROL ',1)[1])
                                action=Control(request['action'])
                                expected=(Control.LOAD,Control.SNAPSHOT)
                                if len(commands)>=len(expected) or action is not expected[len(commands)] or request['sequence']!=len(commands)+1:
                                    raise RuntimeError('Unbound/excessive stall control')
                                writing=control/'command.writing'
                                with open(writing,'x',opener=lambda path,flags:os.open(path,flags,0o600)) as output: json.dump(request,output)
                                writing.replace(control/'command.json')
                                ack=control/f"ack-{request['sequence']:02d}.json"
                                while not ack.is_file():
                                    if child.poll() is not None or time.monotonic()>=deadline: raise RuntimeError('Missing stall acknowledgement')
                                    time.sleep(.02)
                                reply=json.loads(ack.read_text())
                                if reply['sequence']!=request['sequence'] or reply['action']!=action.value: raise RuntimeError('Wrong stall acknowledgement')
                                commands.append(request)
                                child.stdin.write((json.dumps(reply)+'\n').encode()); child.stdin.flush()
                            if 'NHF023_STALL_REPORT ' in line:
                                if report is not None: raise RuntimeError('Duplicate stall result')
                                report=json.loads(line.split('NHF023_STALL_REPORT ',1)[1])
                if child.wait(timeout=10)!=0 or report is None: raise RuntimeError('Slint adapter stall probe failed; inspect '+str(proof/'client.log'))
            finally:
                SESSION['TRANSPORT']['stop'](child)
        load=json.loads((control/'load-done.json').read_text())
        if load['updates']!=1000 or not 49<=load['elapsed_seconds']<=70: raise RuntimeError('20Hz workload not proved')
    final=json.loads((control/'result.json').read_text())
    expected_drivers = 1 if with_command else 0
    if final['command_driver_entries']!=expected_drivers or final['connections_remaining']!=0 or not final['server_joined']:
        raise RuntimeError('Isolated fixture cleanup/safety failed')
    if with_command:
        if final.get('fake_driver_calls')!=1 or final.get('real_device_writes')!=0 or final['action_rpc_calls']<1 or not report['device_actions'] or not report['terminal_notice_after_thaw']:
            raise RuntimeError('Exact fake command confirmation missing/replayed')
    elif final['action_rpc_calls']!=0 or report['device_actions']:
        raise RuntimeError('Unexpected command in read-only fixture')
    if final['workload']!=dict(item_count=10000,completed_items=1,list_revision=10002): raise RuntimeError('Task write replayed or lost')
    result=dict(status='PASS_SLINT_ADAPTER_UI_STALL_SCOPE_ONLY',report=report,load=load,server=final,build=build,
                scope='Real Rust TLS/cache/list owners and Slint test-loop adapters; optional real ActionController with a command-bound fake protocol receipt. UI-only stall, headless, programmatic intent; no native frame, physical input, physical device action or tablet budget.',
                native_rendering=False,physical_tablet=False,all_nhf023_acceptance_closed=False)
    (proof/'result.json').write_text(json.dumps(result,indent=2)+'\n')
    print('UI_STALL_PASS '+str(proof),flush=True)


if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server-binary',type=Path)
    parser.add_argument('--client-binary',type=Path)
    parser.add_argument('--with-command',action='store_true',help='Explicitly opt in to one transport-free fake switch command')
    args=parser.parse_args()
    qualify(args.server_binary,args.client_binary,args.with_command)
