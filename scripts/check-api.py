#!/usr/bin/env python3
"""Check public v2 AIP descriptors and complete v1 migration accounting."""
from enum import Enum
import hashlib
import io
import json
from pathlib import Path
import platform
import subprocess
import tarfile
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
VERSION = '2.4.1'
# Official release digests, pinned with the tool version. Never run downloaded
# code until verified. Reuse the shared tools directory rather than Cargo/Go
# compiler caches or an installation per ticket.
DIGESTS = {
    'darwin-arm64': 'b95b59611c3391b54d10b10bdd95ced2af6c514bed44f8d631049da2acc3f725',
    'darwin-amd64': 'eddeacc5c8f9ea4537bacc08bbc606e1acae8215424ee49bb291386a691d9b0d',
    'linux-amd64': 'adf6b94b01bf1d82723feb2790109b69fc74af65f9be334e418dcc9b9d9ddd03',
    'linux-arm64': '1fe17f676a8d69bc6cebb95e4e7f523ca39979451599c2c5c495081a210d3a7f',
}


class MigrationState(Enum):
    COMPATIBILITY = 'compatibility-v1'
    PRIVATE = 'retained-private'
    MIGRATED = 'migrated-v2'


def linter():
    machine = {'aarch64': 'arm64', 'arm64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
    target = platform.system().lower() + '-' + str(machine)
    if target not in DIGESTS:
        raise RuntimeError('unsupported API Linter platform: ' + target)
    binary = ROOT / '.tools' / ('api-linter-' + VERSION) / target / 'api-linter'
    if not binary.exists():
        url = f'https://github.com/googleapis/api-linter/releases/download/v{VERSION}/api-linter-{VERSION}-{target}.tar.gz'
        with urllib.request.urlopen(url, timeout=60) as response:
            archive = response.read()
        if hashlib.sha256(archive).hexdigest() != DIGESTS[target]:
            raise RuntimeError('API Linter release digest mismatch')
        with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
            member = tar.getmember('api-linter')
            if not member.isfile():
                raise RuntimeError('unexpected API Linter archive member')
            data = tar.extractfile(member).read()
        binary.parent.mkdir(parents=True, exist_ok=True)
        # Atomic publication also leaves simultaneous readers a complete file.
        with tempfile.NamedTemporaryFile(dir=binary.parent, delete=False) as output:
            output.write(data)
            temporary = Path(output.name)
        temporary.chmod(0o755)
        temporary.replace(binary)
    version = subprocess.check_output([str(binary), '--version'], text=True).strip()
    if version != 'api-linter ' + VERSION:
        raise RuntimeError('unexpected API Linter version: ' + version)
    return binary


def main():
    descriptor = json.loads(subprocess.check_output(
        ['buf', 'build', '--as-file-descriptor-set', '-o', '-#format=json'], cwd=ROOT))
    manifest = json.loads((ROOT / 'contracts/api_migration.json').read_text())
    rows = manifest['rows']
    actual = {}
    public_files = []
    types = {}
    for file in descriptor['file']:
        if file.get('package', '').startswith('home.') and file['package'] != 'home.api.v2':
            for service in file.get('service', []):
                for method in service.get('method', []):
                    actual[file['package'] + '.' + service['name'] + '/' + method['name']] = file['name']
        if file.get('package') == 'home.api.v2':
            public_files.append(file)
            for message in file.get('messageType', []):
                resource = message.get('options', {}).get('[google.api.resource]')
                if resource:
                    assert resource['type'].startswith(manifest['resource_namespace'] + '/')
                    assert resource['type'] not in types, 'duplicate resource type'
                    assert resource.get('pattern') and resource.get('singular') and resource.get('plural')
                    types[resource['type']] = resource['pattern']
    assert len(actual) == manifest['baseline']['rpc_count'], 'RPC count drift requires explicit accounting'
    assert len(rows) == len(actual) and {row['rpc'] for row in rows} == set(actual), 'missing or duplicate RPC mapping'
    for row in rows:
        assert row['source'] == 'proto/' + actual[row['rpc']]
        assert (ROOT / row['owner']).exists() and row['target'], 'owner/destination missing'
        state = MigrationState(row['state'])
        if row['visibility'] == 'private':
            assert state is MigrationState.PRIVATE and row['migration_ticket'] is None
        else:
            assert row['visibility'] == 'public' and row['migration_ticket']
            assert state is not MigrationState.PRIVATE
        assert row['v2_implemented'] == (state is MigrationState.MIGRATED), 'schema-only work is not migration'
    assert public_files, 'missing public v2 descriptor'
    for file in public_files:
        comments = {tuple(loc['path']) for loc in file['sourceCodeInfo']['location']
                    if loc.get('leadingComments', '').strip() or loc.get('trailingComments', '').strip()}
        for index, service in enumerate(file.get('service', [])):
            assert (6, index) in comments, 'undocumented service'
            for method_index, method in enumerate(service.get('method', [])):
                assert '[google.api.http]' in method.get('options', {}), 'missing HTTP annotation'
                assert (6, index, 2, method_index) in comments, 'undocumented RPC'
        for index, message in enumerate(file.get('messageType', [])):
            assert (4, index) in comments, 'undocumented message: ' + message['name']
            for field_index, field in enumerate(message.get('field', [])):
                assert (4, index, 2, field_index) in comments, 'undocumented field: ' + field['name']
                reference = field.get('options', {}).get('[google.api.resource_reference]')
                if reference:
                    assert reference.get('type', reference.get('childType')) in types, 'unresolved resource reference'
        for index, enum in enumerate(file.get('enumType', [])):
            assert (5, index) in comments, 'undocumented enum'
            for value_index, value in enumerate(enum['value']):
                assert (5, index, 2, value_index) in comments, 'undocumented enum value'
    # Lint only the public v2 contract. Dependencies, v1 and private Host remain
    # under their existing Buf/compatibility policies; never export them here.
    with tempfile.TemporaryDirectory(prefix='homeward-aip-') as directory:
        public = Path(directory) / 'public.binpb'
        subprocess.run(['buf', 'build', '--path', 'proto/home/api/v2', '--as-file-descriptor-set', '-o', str(public)], cwd=ROOT, check=True)
        exported = json.loads(subprocess.check_output(['buf', 'build', '--path', 'proto/home/api/v2', '--as-file-descriptor-set', '-o', '-#format=json'], cwd=ROOT))
        assert all(not f.get('package', '').startswith('home.') or f['package'] == 'home.api.v2' for f in exported['file']), 'private/legacy descriptor leaked'
        subprocess.run([str(linter()), '--skip-compilation', '--descriptor-set-in', str(public),
                        '--set-exit-status', '--output-format', 'summary',
                        *[file['name'] for file in public_files]], cwd=ROOT, check=True)
    print(f'{len(rows)} RPC mappings, documented public resources and API Linter {VERSION}: pass')


if __name__ == '__main__':
    main()
