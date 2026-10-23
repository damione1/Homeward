#!/usr/bin/env python3
"""Frontend gates; called inside the reserved make-check round."""
import json
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
config = json.loads((root / 'frontend.local.json').read_text())
pro = Path(config['pro_kit'])
subprocess.run(['python3', 'scripts/check.py'], cwd=pro, check=True)
subprocess.run(['cargo', 'check', '--locked', '-p', 'homeward-app', '--lib', '--target', 'aarch64-linux-android'], cwd=root, check=True)
# The free kit's official gate is mandatory only when its sources changed.
free = Path(config['free_kit'])
changed = subprocess.check_output(['git', 'status', '--porcelain'], cwd=free)
if changed:
    subprocess.run(['cargo', 'run', '-p', 'xtask', '--locked', '--', 'check'], cwd=free, check=True)

subprocess.run(['python3', 'scripts/check-frontend-dependencies.py'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/generate-frontend-transport.py', '--check'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-transport.py'], cwd=root, check=True)
