#!/usr/bin/env python3
"""Verify the local kit source graph used by the external Homeward consumer."""
import json
import hashlib
from enum import Enum
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
config = json.loads((root / 'frontend.local.json').read_text())
proof = Path(config['frontend_cache']) / 'dependencies'
proof.mkdir(exist_ok=True)
class Target(Enum):
    MACOS = 'aarch64-apple-darwin'
    ANDROID = 'aarch64-linux-android'

vendor = root / 'client/platform/vendor/slint-android-activity'
origin = json.loads((vendor / 'ORIGIN.json').read_text())
allowed_changes = {'Cargo.toml', 'androidwindowadapter.rs'}
if set(origin['modified_files']) != allowed_changes:
    raise SystemExit('Unexpected Android backend patch scope')
for name, expected in origin['upstream_files'].items():
    if name not in allowed_changes and hashlib.sha256((vendor / name).read_bytes()).hexdigest() != expected:
        raise SystemExit('Unexpected change to vendored Android SDK source: ' + name)

for target in Target:
    raw = subprocess.check_output(['cargo', 'metadata', '--locked', '--format-version', '1', '--filter-platform', target.value], cwd=root)
    (proof / (target.value + '.json')).write_bytes(raw)
    packages = json.loads(raw)['packages']
    for name in ['slint-kit', 'slint-kit-core', 'slint-kit-slint', 'slint-kit-build']:
        matches = [package for package in packages if package['name'] == name]
        if len(matches) != 1 or matches[0]['version'] != '0.1.0' or matches[0]['source'] is not None or not Path(matches[0]['manifest_path']).resolve().is_relative_to(Path(config['free_kit'])):
            raise SystemExit(f'{target}: duplicate or unexpected free kit origin: {name}')
    for name in ['slint-kit-pro', 'slint-kit-pro-build']:
        matches = [package for package in packages if package['name'] == name]
        if len(matches) != 1 or matches[0]['source'] is not None or matches[0]['publish'] != []:
            raise SystemExit(f'{target}: premium source must be local and publish=false: {name}')
    if target is Target.ANDROID:
        matches = [package for package in packages if package['name'] == 'i-slint-backend-android-activity']
        if (len(matches) != 1 or matches[0]['version'] != '1.18.1'
                or matches[0]['source'] is not None
                or Path(matches[0]['manifest_path']).resolve() != (vendor / 'Cargo.toml').resolve()):
            raise SystemExit('Android must use the pinned keyboard-corrected Slint backend')
    tree = subprocess.check_output(['cargo', 'tree', '--locked', '--target', target.value, '-i', 'slint-kit-core'], cwd=root)
    (proof / (target.value + '.tree.txt')).write_bytes(tree)
    print(tree.decode())
