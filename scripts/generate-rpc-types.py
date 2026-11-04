#!/usr/bin/env python3
"""Generate Google's RPC error bindings with the existing pinned Buffa tool.

Use the Google APIs commit in buf.lock as the single dependency authority.
Standard RPC errors, long-running operations and civil value types are emitted;
no Host, API annotations, service clients or duplicate WKT runtimes are generated.
"""
import json
from pathlib import Path
import re
import subprocess
import sys

root = Path(__file__).resolve().parents[1]
if len(sys.argv) not in (2, 3):
    raise SystemExit('usage: generate-rpc-types.py RUST_OUTPUT_DIRECTORY [GO_OUTPUT_DIRECTORY]')
locked = re.findall(r'name: buf\.build/googleapis/googleapis\s+commit: ([a-f0-9]{32})\b', (root / 'buf.lock').read_text())
if len(locked) != 1:
    raise SystemExit('expected exactly one locked Google APIs dependency')
template = {'version': 'v2', 'plugins': [{
    'local': 'protoc-gen-buffa', 'strategy': 'all', 'out': sys.argv[1],
    'opt': ['json=false', 'file_per_package=true'],
}]}
subprocess.run(['buf', 'generate', 'buf.build/googleapis/googleapis:' + locked[0],
                '--path', 'google/rpc/code.proto', '--path', 'google/rpc/status.proto',
                '--path', 'google/rpc/error_details.proto', '--path', 'google/type/date.proto',
                '--path', 'google/type/timeofday.proto', '--path', 'google/longrunning/operations.proto', '--template', json.dumps(template)], cwd=root, check=True)

# Generate the standard Operations Connect interface against Google's Go
# messages. Never copy Operation into Homeward's public API namespace.
go_output = sys.argv[2] if len(sys.argv) == 3 else str(root / 'gen')
go_template = {'version': 'v2', 'plugins': [{
    'local': 'protoc-gen-connect-go', 'out': go_output,
    'opt': ['paths=source_relative'],
}]}
subprocess.run(['buf', 'generate', 'buf.build/googleapis/googleapis:' + locked[0],
                '--path', 'google/longrunning/operations.proto',
                '--template', json.dumps(go_template)], cwd=root, check=True)
