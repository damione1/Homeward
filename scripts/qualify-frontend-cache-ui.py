#!/usr/bin/env python3
"""Qualify the actual native application with the isolated 850-entity core.

Uses Keychain/Keystore, real TLS and synthetic observations only. Result captures
require visual inspection separately from protocol and cache queue measurements.
"""
import argparse
import json
import os
from pathlib import Path
import runpy
import time

ROOT = Path(__file__).resolve().parents[1]
session = runpy.run_path(str(ROOT / 'scripts/qualify-frontend-session.py'))
PlatformSelection = session['PlatformSelection']
Platform = session['Platform']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--platform', type=PlatformSelection, choices=list(PlatformSelection), default=PlatformSelection.ALL)
    args = parser.parse_args()
    if not os.environ.get('CARGO_TARGET_DIR') or not os.environ.get('GOCACHE'):
        parser.error('Run through scripts/frontend.py run --cargo.')
    proof = Path(session['CONFIG']['frontend_cache']) / 'cache-ui' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    server = proof / 'server.test'
    session['run'](['go', 'test', '-c', '-race', '-o', server, './internal/server'])
    for platform in args.platform.platforms():
        match platform:
            case Platform.DESKTOP:
                session['desktop'](proof, server, lifecycle=True, kind=session['FixtureKind'].CACHE)
            case Platform.ANDROID:
                session['android'](proof, server, lifecycle=True, kind=session['FixtureKind'].CACHE)
    session['write'](proof / 'result.json', {
        'status': 'AUTOMATED_CHECKS_PASS', 'server_sha256': session['digest'](server),
        'platforms': [platform.value for platform in args.platform.platforms()],
        'screenshots_require_visual_inspection': True,
    })
    print('Native cache UI automated checks: ' + str(proof), flush=True)


if __name__ == '__main__':
    main()
