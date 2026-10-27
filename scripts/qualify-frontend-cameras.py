#!/usr/bin/env python3
"""Actual camera Rust/Go TLS over a default-off, isolated synthetic-only facade.

No production camera driver, pixels on a native UI, IME or Android claim here.
Invoke through frontend.py run --cargo; all outputs stay in the owned cache.
"""
import contextlib
from enum import Enum
import json
import os
from pathlib import Path
import runpy
import subprocess
import time
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]
SHARED = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-environment.py'))


class Probe(Enum):
    CONTRACT = ('contract', 'actual_camera_tls_private_sessions_images_events_and_cancellation',
                'NHF016_CAMERA_TLS_PASS', ())
    OWNER = ('owner', 'camera_host::tls_probe::actual_camera_owner_private_pixels_viewport_and_revocation',
             'NHF016_CAMERA_OWNER_TLS_PASS', ('--ignored',))


def transport_binary(proof):
    build = subprocess.run(['cargo', 'test', '--locked', '-p', 'home-client-transport', '--test',
                            'nhf016_cameras_tls', '--no-run', '--message-format=json'],
                           cwd=ROOT, text=True, stdout=subprocess.PIPE, check=False)
    (proof / 'transport-build.jsonl').write_text(build.stdout)
    build.check_returncode()
    binaries = [json.loads(line)['executable'] for line in build.stdout.splitlines()
                if line.startswith('{') and json.loads(line).get('reason') == 'compiler-artifact'
                and json.loads(line).get('target', {}).get('name') == 'nhf016_cameras_tls'
                and json.loads(line).get('executable')]
    if len(binaries) != 1:
        raise RuntimeError('One exact Cargo camera transport artifact required')
    return binaries[0]


def qualify(proof, server, binary, probe, video_source=None):
    proof.mkdir(mode=0o700)
    fixture = proof / 'fixture'
    process = client = None
    try:
        with (proof / 'server.log').open('wb') as server_log, (proof / 'client.log').open('wb') as client_log:
            environment = dict(os.environ, HOMEWARD_NHF016_SYNTHETIC_ONLY='1',
                HOMEWARD_NHF016_FIXTURE=str(fixture), HOMEWARD_NHF016_SOURCES=str(ROOT / 'qualification/frontend/cameras'))
            if video_source is not None:
                environment.update(HOMEWARD_NHF017_SYNTHETIC_ONLY='1', HOMEWARD_NHF017_VIDEO=str(video_source))
            process = subprocess.Popen([str(server), '-test.run=^TestNHF016CameraServer$', '-test.v', '-test.timeout=6m'],
                cwd=ROOT / 'internal/server', env=environment,
                stdout=server_log, stderr=subprocess.STDOUT)
            ready = SHARED['wait_file'](fixture / 'ready.json', process, timeout=120)
            origin = urlsplit(ready['origin'])
            if not (ready['mode'] == 'synthetic-only' and ready['production_cameras'] is False
                    and origin.scheme == 'https' and origin.hostname == '127.0.0.1'
                    and origin.port and not origin.username and not origin.password):
                raise RuntimeError('Explicit isolated loopback camera fixture required')
            client = subprocess.Popen([str(binary), '--exact', probe.value[1],
                '--nocapture', '--test-threads=1', *probe.value[3]],
                cwd=ROOT, env=dict(os.environ, HOMEWARD_NHF016_TLS_FIXTURE=str(fixture)),
                stdout=client_log, stderr=subprocess.STDOUT)
            if client.wait(timeout=180) != 0:
                raise RuntimeError('Camera TLS probe failed: ' + str(proof / 'client.log'))
            if probe.value[2] not in (proof / 'client.log').read_text():
                raise RuntimeError('Actual camera TLS success marker missing')
            (fixture / 'stop').touch(mode=0o600)
            if process.wait(timeout=30) != 0:
                raise RuntimeError('Camera fixture did not join')
            final = json.loads((fixture / 'result.json').read_text())
            if not (final['synthetic_only'] and final['real_device_writes'] == 0
                    and final['real_device_driver_instantiated'] is False):
                raise RuntimeError('Camera isolation evidence incomplete')
        return dict(status='PASS', probe=probe.value[0], children_joined=True, isolation=final)
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


def main():
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        raise SystemExit('Use frontend.py run --cargo.')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'nhf016' / ('tls-owner-' + str(time.time_ns()))
    proof.mkdir(parents=True, mode=0o700)
    before = SHARED['source_inventory'](config)
    (proof / 'sources.json').write_text(json.dumps(before, indent=2) + '\n')
    server, owner = SHARED['PROTOCOL']['build'](proof, '^TestNHF016CameraTLS')
    transport = transport_binary(proof)
    probes = []
    for probe in Probe:
        match probe:
            case Probe.CONTRACT:
                binary = transport
            case Probe.OWNER:
                binary = owner
        probes.append(qualify(proof / probe.value[0], server, binary, probe))
    if SHARED['source_inventory'](config) != before:
        raise RuntimeError('Camera sources changed during qualification')
    (proof / 'result.json').write_text(json.dumps(dict(status='PASS', actual_rust_go_tls=True,
        production_cameras=False, native_rendering=False, probes=probes), indent=2) + '\n')
    print('Camera actual Rust/Go TLS and private owner PASS ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
