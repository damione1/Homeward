#!/usr/bin/env python3
"""Generate service stubs against existing Buffa messages, without a second codec."""
import argparse
import hashlib
import json
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
                    'proto/home/state/v1/state.proto', '--path',
                    'proto/home/auth/v1/auth.proto', '--path',
                    'proto/home/dashboard/v1/dashboard.proto', '--path',
                    'proto/home/lists/v1/lists.proto', '--path',
                    'proto/home/history/v1/history.proto', '--path',
                    'proto/home/actions/v1/actions_rpc.proto'], cwd=ROOT, check=True)
    files = {p.name: p.read_bytes() for p in generated.iterdir() if p.is_file()}
    if set(files) != {'home.state.v1.rs', 'home.auth.v1.rs', 'home.dashboard.v1.rs', 'home.actions.v1.rs', 'home.lists.v1.rs', 'home.history.v1.rs'}:
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
