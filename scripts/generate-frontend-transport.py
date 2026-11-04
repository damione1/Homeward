#!/usr/bin/env python3
"""Generate service stubs against existing Buffa messages, without a second codec."""
import argparse
import hashlib
import json
import re
from pathlib import Path
import subprocess
import tempfile
ROOT = Path(__file__).resolve().parents[1]
GENERATOR_SHA256 = '269ea76c84a63677a5a42c6aeb1537edafa91267096dafce164994f097de22f9'
OUTPUT = ROOT / 'client/transport/src/generated'
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--check', action='store_true')
args = parser.parse_args()
generator = ROOT / '.tools/bin/protoc-gen-connect-rust'
if not generator.is_file() or hashlib.sha256(generator.read_bytes()).hexdigest() != GENERATOR_SHA256:
    raise SystemExit('Install the pinned protoc-gen-connect-rust 0.9.0 darwin-aarch64 generator first.')
with tempfile.TemporaryDirectory(prefix='homeward-connect-') as temp:
    directory = Path(temp)
    generated = directory / 'generated'
    template = directory / 'buf.gen.json'
    template.write_text(json.dumps({'version': 'v2', 'plugins': [{
        'local': str(generator), 'strategy': 'all', 'out': str(generated),
        'opt': ['file_per_package', 'no_json', 'buffa_module=::home_wire_contract',
                'extern_path=.google.protobuf=::buffa_types::google::protobuf'],
    }]}))
    subprocess.run(['buf', 'generate', '--template', str(template), '--path',
                    'proto/home/api/v2', '--path',
                    'proto/home/testing/logic/v1/logic.proto', '--path',
                    'proto/home/testing/companion/v1/companion.proto', '--path',
                    'proto/home/testing/notifications/v1/notifications.proto'], cwd=ROOT, check=True)
    locked = re.findall(r'name: buf\.build/googleapis/googleapis\s+commit: ([a-f0-9]{32})\b', (ROOT / 'buf.lock').read_text())
    if len(locked) != 1:
        raise SystemExit('expected exactly one locked Google APIs dependency')
    subprocess.run(['buf', 'generate', 'buf.build/googleapis/googleapis:' + locked[0],
                    '--template', str(template), '--path', 'google/longrunning/operations.proto'],
                   cwd=ROOT, check=True)
    files = {p.name: p.read_bytes() for p in generated.iterdir() if p.is_file()}
    if set(files) != {'google.longrunning.rs', 'home.api.v2.rs', 'home.testing.notifications.v1.rs', 'home.testing.companion.v1.rs', 'home.testing.logic.v1.rs'}:
        raise SystemExit('Unexpected service-stub output; inspect before accepting it.')
    if args.check:
        existing = {p.name: p.read_bytes() for p in OUTPUT.iterdir() if p.is_file()}
        if existing != files:
            raise SystemExit('Rust service stubs differ from the pinned generator.')
    else:
        OUTPUT.mkdir(parents=True, exist_ok=True)
        for name, data in files.items():
            (OUTPUT / name).write_bytes(data)
print('Rust service stubs match the existing Buffa message namespace.')
