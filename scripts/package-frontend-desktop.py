#!/usr/bin/env python3
"""Create a locally signed development macOS bundle from the shared Cargo output."""
import json
import os
from pathlib import Path
import plistlib
import shutil
import subprocess

root = Path(__file__).resolve().parents[1]
config = json.loads((root / 'frontend.local.json').read_text())
subprocess.run(['cargo', 'build', '--locked', '-p', 'homeward-app', '--bin', 'homeward-desktop'], check=True)
bundle = Path(config['frontend_cache']) / 'Homeward Dev.app'
contents = bundle / 'Contents'
(contents / 'MacOS').mkdir(parents=True, exist_ok=True)
shutil.copy2(Path(os.environ['CARGO_TARGET_DIR']) / 'debug/homeward-desktop', contents / 'MacOS/homeward-desktop')
with (contents / 'Info.plist').open('wb') as stream:
    plistlib.dump({'CFBundleExecutable': 'homeward-desktop', 'CFBundleIdentifier': 'ca.homeward.frontend.dev',
                  'CFBundleName': 'Homeward Dev', 'CFBundlePackageType': 'APPL',
                  'CFBundleVersion': '1', 'NSHighResolutionCapable': True}, stream)
subprocess.run(['codesign', '--force', '--sign', '-', str(bundle)], check=True)
subprocess.run(['codesign', '--verify', '--strict', str(bundle)], check=True)
print(bundle)
subprocess.run(['open', '-n', str(bundle)], check=True)
