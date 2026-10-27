#!/usr/bin/env python3
"""Desktop frontend gates; Android is opt-in for final qualification."""
import argparse
from enum import Enum
import json
from pathlib import Path
import subprocess


class ValidationScope(Enum):
    DESKTOP = 'desktop'
    ALL = 'all'

    def __str__(self):
        return self.value


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--platform', type=ValidationScope, choices=list(ValidationScope),
                    default=ValidationScope.DESKTOP)
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
config = json.loads((root / 'frontend.local.json').read_text())
pro = Path(config['pro_kit'])
subprocess.run(['python3', 'scripts/check.py'], cwd=pro, check=True)
if args.platform is ValidationScope.ALL:
    subprocess.run(['cargo', 'check', '--locked', '-p', 'homeward-app', '--lib', '--target', 'aarch64-linux-android'], cwd=root, check=True)
# The free kit's official gate is mandatory only when its sources changed.
free = Path(config['free_kit'])
changed = subprocess.check_output(['git', 'status', '--porcelain'], cwd=free)
if changed:
    subprocess.run(['cargo', 'run', '-p', 'xtask', '--locked', '--', 'check'], cwd=free, check=True)

subprocess.run(['python3', 'scripts/check-frontend-dependencies.py'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/generate-frontend-transport.py', '--check'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-transport.py', '--platform', args.platform.value], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-actions.py', '--platform', args.platform.value], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-recipes.py'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-environment.py'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-lists.py'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-history.py'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-history.py', '--costs-mock'], cwd=root, check=True)
subprocess.run(['python3', 'scripts/qualify-frontend-media.py'], cwd=root, check=True)
