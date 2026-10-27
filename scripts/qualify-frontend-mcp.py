#!/usr/bin/env python3
"""Opt-in Slint MCP inspection of the native, unconfigured development shell.

Invoke through frontend.py run --cargo. No production profile or credentials
are loaded, and no server/device action is issued. MCP is a CLI-only Cargo
feature, absent from the normal application build. Proofs live in the cache.
"""
import argparse
import base64
from enum import Enum
import json
import os
from pathlib import Path
import socket
import subprocess
import time
from urllib.error import URLError
from urllib.request import ProxyHandler, Request, build_opener

ROOT = Path(__file__).resolve().parents[1]


class Tool(Enum):
    WINDOWS = 'list_windows'
    PROPERTIES = 'get_window_properties'
    TREE = 'get_element_tree'
    SCREENSHOT = 'take_screenshot'
    CLICK = 'click_element'
    VALUE = 'set_element_value'
    FIND = 'find_elements_by_id'
    SCROLL = 'dispatch_pointer_scroll'
    KEY = 'dispatch_key_event'


class Mcp:
    def __init__(self, port):
        self.url = f'http://127.0.0.1:{port}/mcp'
        self.sequence = 0
        self.http = build_opener(ProxyHandler({}))

    def rpc(self, method, params):
        self.sequence += 1
        request = Request(self.url, method='POST', data=json.dumps({
            'jsonrpc': '2.0', 'id': self.sequence, 'method': method, 'params': params,
        }).encode(), headers={'Content-Type': 'application/json',
                             'Accept': 'application/json, text/event-stream'})
        with self.http.open(request, timeout=10) as response:
            payload = response.read(16 * 1024 * 1024 + 1)
            if len(payload) > 16 * 1024 * 1024:
                raise RuntimeError('MCP response exceeds the diagnostic budget')
        result = json.loads(payload)
        if result.get('id') != self.sequence or 'error' in result:
            raise RuntimeError('MCP request failed: ' + str(result.get('error')))
        return result['result']

    def tool(self, tool, **arguments):
        if not isinstance(tool, Tool):
            raise TypeError('Inspection requires an explicit tool')
        result = self.rpc('tools/call', {'name': tool.value, 'arguments': arguments})
        if result.get('isError'):
            raise RuntimeError('MCP inspection failed: ' + str(result.get('content')))
        return result


def structured(result):
    if 'structuredContent' in result:
        return result['structuredContent']
    for item in result.get('content', []):
        if item.get('type') == 'text':
            return json.loads(item['text'])
    raise RuntimeError('Missing structured MCP result')


def build_native(proof):
    command = ['cargo', 'build', '-p', 'homeward-app', '--bin', 'homeward-desktop',
               '--features', 'slint/mcp', '--message-format=json', '--locked']
    build = subprocess.run(command, cwd=ROOT, env=dict(os.environ, SLINT_EMIT_DEBUG_INFO='1'),
                           text=True, stdout=subprocess.PIPE, check=False)
    (proof / 'build.jsonl').write_text(build.stdout)
    build.check_returncode()
    binaries = [json.loads(line)['executable'] for line in build.stdout.splitlines()
                if line.startswith('{') and json.loads(line).get('reason') == 'compiler-artifact'
                and json.loads(line).get('target', {}).get('name') == 'homeward-desktop'
                and json.loads(line).get('executable')]
    if len(binaries) != 1:
        raise RuntimeError('Expected one Cargo-provided native artifact')
    return binaries[0]


def qualify():
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    proof = Path(config['frontend_cache']) / 'mcp' / str(time.time_ns())
    proof.mkdir(parents=True, mode=0o700)
    product = Path.home() / 'Library/Application Support/Homeward'
    for name in ('endpoint.json', 'interface.json'):
        if (product / name).exists():
            raise RuntimeError('Preserve existing Homeward profile: ' + name)
    processes = subprocess.check_output(['ps', '-axo', 'comm'], text=True)
    if any(Path(row.strip()).name == 'homeward-desktop' for row in processes.splitlines()[1:]):
        raise RuntimeError('Preserve the existing Homeward process')
    binary = build_native(proof)
    with socket.socket() as reservation:
        reservation.bind(('127.0.0.1', 0))
        port = reservation.getsockname()[1]
    app = None
    try:
        with (proof / 'app.log').open('wb') as log:
            app = subprocess.Popen([binary], cwd=ROOT, stdout=log, stderr=subprocess.STDOUT,
                                   env=dict(os.environ, SLINT_BACKEND='winit-software', SLINT_MCP_PORT=str(port)))
            client = Mcp(port)
            deadline = time.monotonic() + 30
            while True:
                if app.poll() is not None:
                    raise RuntimeError('Owned diagnostic app exited before MCP opened')
                try:
                    initialized = client.rpc('initialize', {'protocolVersion': '2025-06-18',
                        'capabilities': {}, 'clientInfo': {'name': 'homeward-local-qualification', 'version': '1'}})
                    break
                except (URLError, TimeoutError):
                    if time.monotonic() >= deadline:
                        raise RuntimeError('Owned native MCP did not become ready')
                    time.sleep(.1)
            listeners = subprocess.check_output(['lsof', '-nP', '-a', '-p', str(app.pid), '-iTCP', '-sTCP:LISTEN'], text=True)
            rows = listeners.strip().splitlines()[1:]
            if len(rows) != 1 or f'127.0.0.1:{port} (LISTEN)' not in rows[0]:
                raise RuntimeError('MCP listener must belong to the owned app and bind only loopback')
            (proof / 'listeners.txt').write_text(listeners)
            tools = client.rpc('tools/list', {})
            (proof / 'tools.json').write_text(json.dumps(tools, indent=2))
            names = {tool['name'] for tool in tools['tools']}
            if not {tool.value for tool in Tool}.issubset(names):
                raise RuntimeError('Native MCP inspection tools are incomplete')
            windows = structured(client.tool(Tool.WINDOWS))
            (proof / 'windows.json').write_text(json.dumps(windows, indent=2))
            handles = windows.get('windows', windows.get('windowHandles', []))
            if len(handles) != 1:
                raise RuntimeError('Expected exactly one owned diagnostic window')
            handle = handles[0]
            properties = structured(client.tool(Tool.PROPERTIES, windowHandle=handle))
            (proof / 'properties.json').write_text(json.dumps(properties, indent=2))
            root = properties['rootElementHandle']
            tree = structured(client.tool(Tool.TREE, elementHandle=root, maxElements=80))
            (proof / 'tree.json').write_text(json.dumps(tree, indent=2))
            image = client.tool(Tool.SCREENSHOT, windowHandle=handle)
            png = next(item for item in image['content'] if item.get('type') == 'image' and item.get('mimeType') == 'image/png')
            (proof / 'native.png').write_bytes(base64.b64decode(png['data'], validate=True))
            result = {'status': 'PASS', 'native_mcp': True, 'loopback_only': True,
                      'slint_version': '1.18.1', 'feature': 'slint/mcp', 'tools': sorted(names),
                      'server': initialized['serverInfo'], 'port': port, 'pid': app.pid,
                      'network_profile': False, 'real_devices': False, 'calendar_ui_qualified': False,
                      'screenshot': str(proof / 'native.png')}
            (proof / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
            print(json.dumps(result), flush=True)
    finally:
        if app is not None and app.poll() is None:
            app.terminate()
            try:
                app.wait(timeout=10)
            except subprocess.TimeoutExpired:
                app.kill()
                app.wait(timeout=10)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    qualify()
