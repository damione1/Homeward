#!/usr/bin/env python3
"""Pinned Android development SDK, invoked through frontend.py's storage guard."""
import os
import subprocess
import shutil
from pathlib import Path

PACKAGES = ['platforms;android-35', 'build-tools;35.0.0', 'ndk;27.2.12479018',
            'emulator', 'system-images;android-35;google_apis;arm64-v8a']
if not os.environ.get('ANDROID_HOME') or not os.environ.get('CARGO_TARGET_DIR'):
    raise SystemExit('Run through scripts/frontend.py run to reserve storage and shared tools.')
subprocess.run(['sdkmanager', '--licenses'], input='y\n' * 50, text=True, check=True)
subprocess.run(['sdkmanager', *PACKAGES], input='y\n' * 50, text=True, check=True)
subprocess.run(['rustup', 'target', 'add', '--toolchain', '1.98.1', 'aarch64-linux-android'], check=True)

# Reuse Homebrew's existing platform-tools instead of downloading another adb.
platform_tools = Path(os.environ['ANDROID_HOME']) / 'platform-tools'
if not platform_tools.exists():
    adb = shutil.which('adb')
    if adb is None:
        subprocess.run(['brew', 'install', '--cask', 'android-platform-tools'], check=True)
        adb = shutil.which('adb')
    platform_tools.symlink_to(Path(adb).resolve().parent, target_is_directory=True)
