#!/usr/bin/env python3
"""Build with cargo-apk 0.10; enforce lock immutability despite its missing --locked."""
import argparse
from enum import Enum
from pathlib import Path
import subprocess


class BuildProfile(Enum):
    DEBUG = 'debug'
    RELEASE = 'release'

    def __str__(self):
        return self.value

    def arguments(self):
        match self:
            case BuildProfile.DEBUG:
                return []
            case BuildProfile.RELEASE:
                return ['--release']


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--profile', type=BuildProfile, choices=list(BuildProfile), default=BuildProfile.DEBUG)
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
lock = root / 'Cargo.lock'
before = lock.read_bytes()
subprocess.run(['cargo', 'metadata', '--locked', '--no-deps', '--format-version', '1'], cwd=root, check=True, stdout=subprocess.DEVNULL)
result = subprocess.run(['cargo', 'apk', 'build', '-p', 'homeward-app', '--lib', '--target', 'aarch64-linux-android',
                         *args.profile.arguments()], cwd=root)
if lock.read_bytes() != before:
    raise SystemExit('cargo-apk changed Cargo.lock; inspect the dependency change before delivery')
raise SystemExit(result.returncode)
