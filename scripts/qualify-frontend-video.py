#!/usr/bin/env python3
"""Actual authorized synthetic H264 over Rust/Go TLS and macOS hardware decode.

Invoke through frontend.py run --cargo. No Slint/Android/resource-budget claim.
"""
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))
CAMERAS = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-cameras.py'))

class Probe(Enum):
    OWNER = ('owner', 'camera_host::tls_probe::actual_camera_native_video_owner_lifecycle_and_private_frames',
             'NHF017_NATIVE_VIDEO_OWNER_TLS_PASS', ('--ignored',))

def owner_binary(proof):
    build = subprocess.run(['cargo', 'test', '--locked', '-p', 'homeward-app', '--lib',
                            '--no-run', '--message-format=json'], cwd=ROOT,
                           text=True, stdout=subprocess.PIPE, check=False)
    (proof / 'owner-build.jsonl').write_text(build.stdout)
    build.check_returncode()
    binaries = [json.loads(line)['executable'] for line in build.stdout.splitlines()
                if line.startswith('{') and json.loads(line).get('reason') == 'compiler-artifact'
                and json.loads(line).get('target', {}).get('name') == 'homeward_app'
                and json.loads(line).get('executable')]
    if len(binaries) != 1:
        raise RuntimeError('One exact native-video owner artifact required')
    return binaries[0]


def main():
    if sys.platform != 'darwin':
        print('Native VideoToolbox qualification unavailable on this host; no native video claim.')
        return
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Use frontend.py run --cargo.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf017' / ('tls-codec-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(before, indent=2) + '\n')
    server = proof / 'server.test'
    with (proof / 'go-race.log').open('wb') as output:
        subprocess.run(['go', 'test', '-race', '-count=1', '-timeout=3m', './internal/server',
                        '-run', '^TestNHF01(6CameraTLS|7VideoTLS)', '-v'], cwd=ROOT,
                       stdout=output, stderr=subprocess.STDOUT, check=True)
    subprocess.run(['go', 'test', '-race', '-c', '-o', str(server), './internal/server'], cwd=ROOT, check=True)
    build = subprocess.run(['cargo', 'build', '--locked', '-p', 'home-client-platform', '--example',
                            'native_video_tls_probe', '--message-format=json'], cwd=ROOT,
                           text=True, stdout=subprocess.PIPE, check=False)
    (proof / 'native-build.jsonl').write_text(build.stdout)
    build.check_returncode()
    binaries = [json.loads(line)['executable'] for line in build.stdout.splitlines()
                if line.startswith('{') and json.loads(line).get('reason') == 'compiler-artifact'
                and json.loads(line).get('target', {}).get('name') == 'native_video_tls_probe'
                and json.loads(line).get('executable')]
    if len(binaries) != 1:
        raise RuntimeError('One exact native-video artifact required')
    fixture = proof / 'fixture'
    process = client = None
    try:
        with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('wb') as client_log:
            process = subprocess.Popen([str(server), '-test.run=^TestNHF016CameraServer$', '-test.v', '-test.timeout=3m'],
                cwd=ROOT / 'internal/server', env=dict(os.environ,
                    HOMEWARD_NHF016_SYNTHETIC_ONLY='1', HOMEWARD_NHF016_FIXTURE=str(fixture),
                    HOMEWARD_NHF016_SOURCES=str(ROOT / 'qualification/frontend/cameras'),
                    HOMEWARD_NHF017_SYNTHETIC_ONLY='1', HOMEWARD_NHF017_VIDEO=str(ROOT / 'qualification/frontend/video')),
                stdout=server_log, stderr=subprocess.STDOUT)
            ready = SHARED['wait_file'](fixture / 'ready.json', process, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_cameras'] is False
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1'
                    and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Explicit isolated native-video fixture required')
            client = subprocess.Popen([binaries[0], str(fixture)], cwd=ROOT,
                                      stdout=client_log, stderr=subprocess.STDOUT)
            if client.wait(timeout=60) != 0:
                raise RuntimeError('Native video failed: ' + str(proof / 'client.log'))
            reports = [json.loads(line) for line in (proof / 'client.log').read_text().splitlines()
                       if line.startswith('{')]
            if len(reports) != 1:
                raise RuntimeError('One actual native-video report required')
            report = reports[0]
            if not (report['status'] == 'PASS' and report['actual_rust_go_tls']
                    and report['native_hardware_decode'] and report['server_streams_joined']
                    and len(report['sources']) == 2
                    and all(source['decoded_frames'] == 6 and source['distinct_decoded_frames'] == 6
                            and source['hardware'] and source['decoder_joined'] for source in report['sources'])):
                raise RuntimeError('Native video evidence incomplete')
            (fixture / 'stop').touch(mode=0o600)
            if process.wait(timeout=30) != 0:
                raise RuntimeError('Video fixture did not join')
            isolation = json.loads((fixture / 'result.json').read_text())
            if not (isolation['synthetic_only'] and isolation['real_device_writes'] == 0
                    and isolation['real_device_driver_instantiated'] is False):
                raise RuntimeError('Video isolation incomplete')
        owner = owner_binary(proof)
        owner_result = CAMERAS['qualify'](proof / 'owner', server, owner, Probe.OWNER,
                                          ROOT / 'qualification/frontend/video')
        if SHARED['source_inventory'](config) != before:
            raise RuntimeError('Sources changed during native video qualification')
        (proof / 'result.json').write_text(json.dumps(dict(status='PASS', native_codec=report, owner=owner_result,
            actual_rust_go_tls=True, children_joined=True, isolation=isolation,
            native_slint_rendering=False, resource_growth_measured=False,
            android_qualified=False, production_video=False), indent=2) + '\n')
        print('Native hardware video over actual Rust/Go TLS PASS ' + str(proof), flush=True)
    finally:
        SHARED['stop'](client)
        if process is not None and process.poll() is None:
            if fixture.is_dir():
                (fixture / 'stop').touch(mode=0o600)
            with contextlib.suppress(subprocess.TimeoutExpired):
                process.wait(timeout=30)
        SHARED['stop'](process)
        with contextlib.suppress(FileNotFoundError):
            (fixture / 'ready.json').unlink()


if __name__ == '__main__':
    main()
