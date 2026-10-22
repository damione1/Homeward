#!/usr/bin/env python3
"""Freeze and serially build the exact NHA-036 pair and destructive control.

Run under the Homeward build slot with its normal pinned tool/cache environment.
The output is outside the repository; it is retained evidence, never installed.
Then export HOMEWARD_NHA036_ARTIFACTS=<output>/manifest.json for the Go gate.
Use --race for the make check race gate; modes cannot share an output directory.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import subprocess

ADDITIVE = 'ALTER TABLE public.helper_values ADD COLUMN nha036_successor_note text;'
DESTRUCTIVE = 'ALTER TABLE public.helper_values DROP COLUMN boolean_value CASCADE;'
UNKNOWN_KIND = "ALTER TABLE public.helper_values DROP CONSTRAINT helper_values_payload; UPDATE public.helper_values SET kind=32767 WHERE entity_id=(SELECT id FROM public.registry_entities WHERE manifest_alias='input_boolean.away_mode');"


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


SOURCE_EXCLUSIONS = frozenset({'.git', '.tools', 'target', '__pycache__'})


def inventory(root):
    def inaccessible(error):
        raise error
    result = {}
    for directory, dirs, files in os.walk(root, followlinks=False, onerror=inaccessible):
        dirs[:] = sorted(name for name in dirs if name not in SOURCE_EXCLUSIONS)
        for name in dirs:
            if (Path(directory) / name).is_symlink():
                raise ValueError('unsupported source symlink directory')
        for name in sorted(files):
            if name in SOURCE_EXCLUSIONS:
                continue
            path = Path(directory) / name
            if path.is_symlink() or not path.is_file():
                raise ValueError('unsupported source node')
            result[path.relative_to(root).as_posix()] = digest(path)
    return result


def compiled_profile(path):
    # Parse only the seven literal declarations owned by this contract. No Go
    # tool is executed to prepare or select a profile.
    names = {'schemaWindowBaseline', 'schemaWindowSuccessor',
             'schemaWindowBaselineRelease', 'schemaWindowSuccessorRelease',
             'compiledSchemaReleaseID', 'compiledMinimumReaderID', 'compiledMinimumWriterID'}
    values = {}
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line == 'package storage' or line.startswith('//'):
            continue
        found = re.fullmatch(r'const (\w+)(?: uint32)? = ([1-9][0-9]*|"[a-z][a-z0-9.-]{0,127}")', line)
        if not found or found[1] not in names or found[1] in values:
            raise ValueError('profile must contain only its seven explicit literal constants')
        values[found[1]] = json.loads(found[2])
    if set(values) != names:
        raise ValueError('incomplete compiled profile')
    baseline, successor = values['schemaWindowBaseline'], values['schemaWindowSuccessor']
    baseline_id, successor_id = values['schemaWindowBaselineRelease'], values['schemaWindowSuccessorRelease']
    if type(baseline) is not int or type(successor) is not int or not (0 < baseline < successor <= 2**31-1) or successor != baseline + 1:
        raise ValueError('this single-expansion fixture requires an explicit consecutive schema pair')
    if not isinstance(baseline_id, str) or not isinstance(successor_id, str) or baseline_id == successor_id:
        raise ValueError('distinct explicit release IDs required')
    if any(values[key] != baseline_id for key in ['compiledSchemaReleaseID', 'compiledMinimumReaderID', 'compiledMinimumWriterID']):
        raise ValueError('stage requires the real baseline declaration, including reader and writer minima')
    profile = {'baseline_schema': baseline, 'successor_schema': successor,
               'baseline_release': baseline_id, 'successor_release': successor_id}
    return values, profile


def append_fixture(migrations, profile, sql):
    # Append to the real migration authority, independently of the last SQL
    # body's name or contents. The built baseline must attest its actual ceiling.
    begin = 'var migrations = []migration{'
    end = 'var latestVersion = migrations[len(migrations)-1].version'
    if migrations.count(begin) != 1 or migrations.count(end) != 1:
        raise ValueError('unique migration authority required')
    start, stop = migrations.index(begin), migrations.index(end)
    body = migrations[start:stop].rstrip()
    versions = re.findall(r'version:\s*Version\{([1-9][0-9]*)\}', body)
    if not versions or int(versions[-1]) != profile['baseline_schema'] or not body.endswith('}'):
        raise ValueError('real last migration disagrees with explicit baseline profile')
    head = body[:-1].rstrip()
    separator = ' ' if head.endswith(',') else ', '
    fixture = '{version: Version{' + str(profile['successor_schema']) + '}, sql: `' + sql + '`}}'
    return migrations[:start] + head + separator + fixture + '\n\n' + migrations[stop:]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--race', action='store_true')
    args = parser.parse_args()
    source = Path(__file__).resolve().parents[2]
    output = args.output.resolve()
    if output == source or source in output.parents:
        parser.error('output must be outside product sources')
    if output.exists():
        parser.error('output must be a fresh owned directory (no stale artifact reuse)')
    migrations = (source / 'internal/storage/migrations.go').read_text()
    values, profile = compiled_profile(source / 'internal/storage/schema_compatibility_release.go')
    append_fixture(migrations, profile, ADDITIVE)  # validate the real append point before output/builds
    go_version = subprocess.check_output(['go', 'version'], text=True).strip()
    if not go_version.startswith('go version go1.27.2 '):
        parser.error('requires the existing pinned Go 1.27.2 toolchain')
    original_sources = inventory(source)
    output.mkdir(parents=True)
    manifest = {'format': 1, 'race': args.race, 'go': go_version,
                'artifacts': {}, 'source': str(source), 'profile': profile}
    for role, sql in [('baseline', None), ('additive', ADDITIVE), ('destructive', DESTRUCTIVE), ('unknown-kind', UNKNOWN_KIND)]:
        tree = output / role / 'source'
        shutil.copytree(source, tree, ignore=shutil.ignore_patterns(*SOURCE_EXCLUSIONS))
        os.symlink((source / '.tools').resolve(), tree / '.tools', target_is_directory=True)
        if sql is not None:
            path = tree / 'internal/storage/migrations.go'
            path.write_text(append_fixture(migrations, profile, sql))
            release = tree / 'internal/storage/schema_compatibility_release.go'
            old = 'const compiledSchemaReleaseID = ' + json.dumps(profile['baseline_release'])
            new = 'const compiledSchemaReleaseID = ' + json.dumps(profile['successor_release'])
            if release.read_text().count(old) != 1:
                raise ValueError('exact compiled baseline release declaration required')
            release.write_text(release.read_text().replace(old, new))
        sources = inventory(tree)
        encoded = json.dumps(sources, sort_keys=True, separators=(',', ':')).encode()
        (output / role / 'sources.json').write_bytes(encoded)
        binary = output / role / 'server.test'
        homed = output / role / 'homed'
        race = ['-race'] if args.race else []
        for command in [['go', 'test', '-c', *race, '-o', str(binary), './internal/server'],
                        ['go', 'build', *race, '-o', str(homed), './cmd/homed']]:
            with (output / role / (command[1] + '.log')).open('w') as log:
                result = subprocess.run(command, cwd=tree, stdout=log, stderr=subprocess.STDOUT)
            if result.returncode:
                raise SystemExit(f'{role} build failed ({result.returncode}); inspect retained logs')
        if inventory(source) != original_sources:
            raise SystemExit('complete current source inventory changed while building')
        if inventory(tree) != sources:
            raise SystemExit('frozen source changed while building')
        identity_env = os.environ.copy()
        identity_env['NHA036_BUILD_IDENTITY'] = '1'
        identity_output = subprocess.check_output([str(binary), '-test.run=^TestNHA036BuildIdentityHelper$', '-test.v'], cwd=tree, env=identity_env, text=True)
        identities = [json.loads(line.removeprefix('NHA036_BUILD ')) for line in identity_output.splitlines() if line.startswith('NHA036_BUILD ')]
        if len(identities) != 1:
            raise SystemExit('compiled artifact did not attest its identity')
        identity = identities[0]
        expected_schema = profile['baseline_schema'] if sql is None else profile['successor_schema']
        expected_release = profile['baseline_release'] if sql is None else profile['successor_release']
        if identity['schema'] != expected_schema or identity['release'] != expected_release or identity['profile'] != profile or identity['minimum_reader'] != profile['baseline_release'] or identity['minimum_writer'] != profile['baseline_release']:
            raise SystemExit('compiled role disagrees with its frozen declaration')
        manifest['artifacts'][role] = {'test': str(binary), 'homed': str(homed), 'test_sha256': digest(binary),
            'homed_sha256': digest(homed), 'sources_sha256': hashlib.sha256(encoded).hexdigest(),
            'source_manifest': str(output / role / 'sources.json'), 'schema': identity['schema'],
            'release': identity['release'], 'plan_sha256': identity['plan'],
            'minimum_reader': identity['minimum_reader'], 'minimum_writer': identity['minimum_writer']}
    if len({a['test_sha256'] for a in manifest['artifacts'].values()}) != 4:
        raise SystemExit('distinct source roles produced identical executables')
    (output / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    print(output / 'manifest.json')


if __name__ == '__main__':
    main()
