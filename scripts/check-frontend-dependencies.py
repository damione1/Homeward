#!/usr/bin/env python3
"""Verify the local kit source graph used by the external Homeward consumer."""
import json
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
config = json.loads((root / 'frontend.local.json').read_text())
proof = Path(config['frontend_cache']) / 'dependencies'
proof.mkdir(exist_ok=True)
for target in ['aarch64-apple-darwin', 'aarch64-linux-android']:
    raw = subprocess.check_output(['cargo', 'metadata', '--locked', '--format-version', '1', '--filter-platform', target], cwd=root)
    (proof / (target + '.json')).write_bytes(raw)
    packages = json.loads(raw)['packages']
    for name in ['slint-kit', 'slint-kit-core', 'slint-kit-slint', 'slint-kit-build']:
        matches = [package for package in packages if package['name'] == name]
        if len(matches) != 1 or matches[0]['version'] != '0.1.0' or matches[0]['source'] is not None or not Path(matches[0]['manifest_path']).resolve().is_relative_to(Path(config['free_kit'])):
            raise SystemExit(f'{target}: duplicate or unexpected free kit origin: {name}')
    for name in ['slint-kit-pro', 'slint-kit-pro-build']:
        matches = [package for package in packages if package['name'] == name]
        if len(matches) != 1 or matches[0]['source'] is not None or matches[0]['publish'] != []:
            raise SystemExit(f'{target}: premium source must be local and publish=false: {name}')
    tree = subprocess.check_output(['cargo', 'tree', '--locked', '--target', target, '-i', 'slint-kit-core'], cwd=root)
    (proof / (target + '.tree.txt')).write_bytes(tree)
    print(tree.decode())
