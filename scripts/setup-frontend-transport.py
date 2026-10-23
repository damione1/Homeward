#!/usr/bin/env python3
"""Install the official pinned service generator on the evaluated Homebrew host."""
import hashlib
import os
from pathlib import Path
import platform
import urllib.request

if not os.environ.get('CARGO_TARGET_DIR'):
    raise SystemExit('Run through scripts/frontend.py run to reserve shared tools/storage.')
if (platform.system(), platform.machine()) != ('Darwin', 'arm64'):
    raise SystemExit('Only the evaluated darwin-aarch64 generator is pinned here.')

# No Homebrew formula exists for this generator; use the official release asset.
asset = 'protoc-gen-connect-rust-v0.9.0-darwin-aarch64'
expected = '269ea76c84a63677a5a42c6aeb1537edafa91267096dafce164994f097de22f9'
destination = Path(__file__).resolve().parents[1] / '.tools/bin/protoc-gen-connect-rust'
if destination.is_file() and hashlib.sha256(destination.read_bytes()).hexdigest() == expected:
    print('Pinned Connect Rust generator already verified.')
else:
    url = 'https://github.com/connectrpc/connect-rust/releases/download/v0.9.0/' + asset
    with urllib.request.urlopen(url, timeout=30) as response:
        data = response.read(4_000_001)
    if len(data) > 4_000_000 or hashlib.sha256(data).hexdigest() != expected:
        raise SystemExit('Official generator checksum mismatch; existing installation preserved.')
    destination.parent.mkdir(parents=True, exist_ok=True)
    temporary = destination.with_suffix('.installing')
    with temporary.open('xb') as output:
        output.write(data)
    temporary.chmod(0o755)
    temporary.replace(destination)
    print('Verified and installed official Connect Rust service generator 0.9.0.')
