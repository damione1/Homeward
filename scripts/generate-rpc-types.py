#!/usr/bin/env python3
"""Generate Google's RPC error bindings with the existing pinned Buffa tool.

Use the Google APIs commit in buf.lock as the single dependency authority.
Only these three standard schemas are emitted; no Host, API annotations,
Google service clients or copies of well-known runtime types are generated.
"""
import json
from pathlib import Path
import re
import subprocess
import sys

root = Path(__file__).resolve().parents[1]
if len(sys.argv) != 2:
    raise SystemExit('usage: generate-rpc-types.py OUTPUT_DIRECTORY')
locked = re.findall(r'name: buf\.build/googleapis/googleapis\s+commit: ([a-f0-9]{32})\b', (root / 'buf.lock').read_text())
if len(locked) != 1:
    raise SystemExit('expected exactly one locked Google APIs dependency')
template = {'version': 'v2', 'plugins': [{
    'local': 'protoc-gen-buffa', 'strategy': 'all', 'out': sys.argv[1],
    'opt': ['json=false', 'file_per_package=true'],
}]}
subprocess.run(['buf', 'generate', 'buf.build/googleapis/googleapis:' + locked[0],
                '--path', 'google/rpc/code.proto', '--path', 'google/rpc/status.proto',
                '--path', 'google/rpc/error_details.proto', '--template', json.dumps(template)], cwd=root, check=True)
