#!/usr/bin/env python3
"""Check public v2 AIP descriptors and complete v1 migration accounting."""
from enum import Enum
import hashlib
import io
import json
from pathlib import Path
import platform
import re
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
    PENDING = 'pending-replacement'
    PRIVATE = 'retained-private'
    MIGRATED = 'migrated-v2'
    QUALIFICATION = 'replaced-private-qualification'
    OPERATOR = 'replaced-private-operator'
    WITHDRAWN = 'withdrawn-unimplemented-declaration'


# Immutable audit identity, not a wire-compatibility baseline. Destinations,
# migration states and evidence evolve; deleting or reclassifying an original
# public method must never make its migration obligation disappear.
HISTORICAL_RPC_COUNT = 135
HISTORICAL_INVENTORY_SHA256 = '4c2b1540378a7487e7b72a609d6135c8e99dc43d932eff1185d88ef8ca3f522e'


def check_historical_inventory(manifest):
    rows = manifest['rows']
    assert manifest['baseline']['rpc_count'] == HISTORICAL_RPC_COUNT, 'historical RPC baseline changed'
    assert len(rows) == HISTORICAL_RPC_COUNT, 'historical RPC baseline changed'
    assert len({row['rpc'] for row in rows}) == len(rows), 'duplicate historical RPC mapping'
    identity = [{key: row[key] for key in ('rpc', 'source', 'owner', 'visibility')}
                for row in sorted(rows, key=lambda row: row['rpc'])]
    encoded = json.dumps(identity, sort_keys=True, separators=(',', ':')).encode()
    assert hashlib.sha256(encoded).hexdigest() == HISTORICAL_INVENTORY_SHA256, 'historical RPC audit identity changed'



def check_historical_sources(refs, root):
    """Verify archived implementation evidence without restoring old API code."""
    assert isinstance(refs, list) and refs, 'missing historical implementation evidence'
    for ref in refs:
        path = Path(ref['file'])
        assert not path.is_absolute() and '..' not in path.parts, 'invalid historical evidence path'
        revision = ref.get('revision', '')
        assert re.fullmatch(r'[0-9a-f]{40}', revision), 'historical revision must be exact'
        source = subprocess.check_output(['git', 'show', revision + ':' + path.as_posix()], cwd=root)
        assert hashlib.sha256(source).hexdigest() == ref.get('sha256'), 'historical source digest changed'
        text = source.decode()
        required, absent = ref.get('required_symbols'), ref.get('absent_symbols')
        assert required and absent, 'historical implementation boundary missing'
        assert all(symbol in text for symbol in required), 'historical implemented route missing'
        assert all(symbol not in text for symbol in absent), 'withdrawn declaration had implementation evidence'


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


def check_migrations(manifest, descriptor, root):
    check_historical_inventory(manifest)
    _check_migration_rows(manifest, descriptor, root)


def _check_migration_rows(manifest, descriptor, root):
    """The baseline is historical accounting, never a quota of old endpoints.

    Replacements must remove the old RPC and name existing destinations plus
    registration/qualification evidence. This structural check complements the
    named executable tests; a generated descriptor alone is insufficient.
    """
    rows = manifest['rows']
    assert len(rows) == manifest['baseline']['rpc_count'], 'historical RPC baseline changed'
    assert len({row['rpc'] for row in rows}) == len(rows), 'duplicate historical RPC mapping'
    methods = {}
    for file in descriptor['file']:
        for service in file.get('service', []):
            for method in service.get('method', []):
                methods[file['package'] + '.' + service['name'] + '/' + method['name']] = file['name']
    accounted = set()
    def evidence(refs):
        assert isinstance(refs, list) and refs, 'missing implementation evidence'
        for ref in refs:
            path = Path(ref['file'])
            assert not path.is_absolute() and '..' not in path.parts, 'invalid evidence path'
            source = root / path
            assert source.is_file() and ref.get('symbol'), 'missing evidence source/symbol'
            assert ref['symbol'] in source.read_text(), 'missing registration/test symbol'
    for row in rows:
        assert row['source'].startswith('proto/') and row['owner'] and row['target'], 'historical owner/destination missing'
        state = MigrationState(row['state'])
        private = row['visibility'] == 'private'
        if private:
            assert state is MigrationState.PRIVATE and row['migration_ticket'] is None
        else:
            assert row['visibility'] == 'public' and row['migration_ticket']
            assert state is not MigrationState.PRIVATE
        assert row['v2_implemented'] == (state is MigrationState.MIGRATED), 'schema-only work is not migration'
        if state in (MigrationState.PENDING, MigrationState.PRIVATE):
            assert row['rpc'] in methods, 'pending/private RPC unexpectedly removed: ' + row['rpc']
            assert row['source'] == 'proto/' + methods[row['rpc']], 'historical source mismatch'
            assert (root / row['owner']).exists(), 'current owner missing'
            accounted.add(row['rpc'])
            continue
        assert row['rpc'] not in methods, 'replaced public RPC retained for compatibility: ' + row['rpc']
        replacement = row.get('replacement', {})
        if state is MigrationState.WITHDRAWN:
            assert replacement.get('classification_reason'), 'withdrawal reason missing'
            assert not replacement.get('rpcs'), 'withdrawn declaration must not claim equivalent RPC implementation'
            check_historical_sources(replacement.get('historical_sources'), root)
            workflow = replacement.get('workflow', {})
            targets = workflow.get('rpcs')
            assert isinstance(targets, list) and targets and len(set(targets)) == len(targets), 'missing workflow RPCs'
            assert all(target in methods and target.startswith('home.api.v2.') for target in targets), 'workflow RPC missing or private'
            assert workflow.get('relationship'), 'workflow is not a fabricated CRUD equivalence'
            evidence(workflow.get('registrations'))
            evidence(workflow.get('source_evidence'))
            accounted.update(targets)
            continue
        targets = replacement.get('rpcs')
        assert isinstance(targets, list) and targets and len(set(targets)) == len(targets), 'missing/duplicate exact replacement RPCs'
        assert all(target in methods for target in targets), 'replacement RPC missing from descriptor'
        if state is MigrationState.OPERATOR:
            assert all(target.startswith('home.operator.v1.') for target in targets), 'operator replacement must remain private'
            assert replacement.get('transport') == 'unix-same-uid', 'operator transport boundary missing'
            assert replacement.get('classification_reason'), 'operator reclassification must be explicit'
            evidence(replacement.get('registrations'))
            evidence(replacement.get('transport_evidence'))
            evidence(replacement.get('tests'))
        elif state is MigrationState.QUALIFICATION:
            assert all(target.startswith('home.testing.') for target in targets), 'qualification exposed as product API'
            assert replacement.get('classification_reason'), 'private qualification reclassification must be explicit'
            evidence(replacement.get('qualification_tests'))
        else:
            assert all(target.startswith(('home.api.v2.', 'google.longrunning.Operations/')) for target in targets), 'invalid public replacement namespace'
            evidence(replacement.get('registrations'))
            evidence(replacement.get('tests'))
        accounted.update(targets)
    remaining = {rpc for rpc in methods if rpc.startswith('home.') and not rpc.startswith('home.api.v2.')}
    assert remaining <= accounted, 'unaccounted legacy/private RPCs: ' + ', '.join(sorted(remaining - accounted))


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
            # File-level declarations name native relationships that have no
            # separately managed resource message in this service.
            for resource in file.get('options', {}).get('[google.api.resource_definition]', []):
                assert resource['type'].startswith(manifest['resource_namespace'] + '/')
                assert resource.get('pattern'), 'missing resource definition pattern'
                if resource['type'] in types:
                    assert types[resource['type']] == resource['pattern'], 'conflicting resource definition'
                types[resource['type']] = resource['pattern']
            for message in file.get('messageType', []):
                resource = message.get('options', {}).get('[google.api.resource]')
                if resource:
                    assert resource['type'].startswith(manifest['resource_namespace'] + '/')
                    assert resource['type'] not in types, 'duplicate resource type'
                    assert resource.get('pattern') and resource.get('singular') and resource.get('plural')
                    types[resource['type']] = resource['pattern']
    check_migrations(manifest, descriptor, ROOT)
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
    # under their current schema checks until their owning lot replaces them.
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
