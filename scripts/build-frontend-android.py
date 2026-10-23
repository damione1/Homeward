#!/usr/bin/env python3
"""Build with cargo-apk 0.10; enforce lock immutability despite its missing --locked."""
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]
lock = root / 'Cargo.lock'
before = lock.read_bytes()
subprocess.run(['cargo', 'metadata', '--locked', '--no-deps', '--format-version', '1'], cwd=root, check=True, stdout=subprocess.DEVNULL)
result = subprocess.run(['cargo', 'apk', 'build', '-p', 'homeward-app', '--lib', '--target', 'aarch64-linux-android'], cwd=root)
if lock.read_bytes() != before:
    raise SystemExit('cargo-apk changed Cargo.lock; inspect the dependency change before delivery')
raise SystemExit(result.returncode)
