#!/usr/bin/env python3
"""Launch one isolated TLS mock; caller owns shutdown and the public CA path."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
local = json.loads((root / 'frontend.local.json').read_text())
parent = Path(local['frontend_cache']) / 'mock-instances'
parent.mkdir(parents=True, exist_ok=True)
instance = Path(tempfile.mkdtemp(prefix='instance-', dir=parent))
subprocess.run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256',
                '-nodes', '-days', '2', '-subj', '/CN=Homeward development mock',
                '-addext', 'subjectAltName=IP:127.0.0.1,DNS:localhost',
                '-keyout', str(instance / 'key.pem'), '-out', str(instance / 'ca.pem')], check=True)
(instance / 'key.pem').chmod(0o600)
print(json.dumps({'mode': 'mock', 'ca_file': str(instance / 'ca.pem')}), flush=True)
os.execvp('go', ['go', 'run', './cmd/homeward-dev', '--mock', '--data-dir', str(instance / 'data'),
                  '--tls-cert', str(instance / 'ca.pem'), '--tls-key', str(instance / 'key.pem')])
