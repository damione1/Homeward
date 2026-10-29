#!/usr/bin/env python3
"""Freeze private source artifacts and build identified local evaluation apps.

Invoke through frontend.py run --cargo. Source archives contain tracked files
only; signing credentials remain in the caller's environment. No publication,
installation, production migration or device operation is performed here.
"""
import argparse
from enum import Enum
import hashlib
import json
import os
from pathlib import Path
import platform
import plistlib
import re
import shutil
import subprocess
import sys
import tarfile
import tomllib
import zipfile

ROOT = Path(__file__).resolve().parent
if ROOT.name == 'scripts':
    ROOT = ROOT.parent


class Source(Enum):
    HOMEWARD = 'homeward'
    FREE = 'slint-kit'
    PRO = 'slint-kit-pro'


class Platform(Enum):
    MACOS = 'macos'
    ANDROID = 'android'
    ALL = 'all'

    def __str__(self):
        return self.value

    def targets(self):
        match self:
            case Platform.MACOS:
                return ('aarch64-apple-darwin',)
            case Platform.ANDROID:
                return ('aarch64-linux-android',)
            case Platform.ALL:
                return ('aarch64-apple-darwin', 'aarch64-linux-android')


class Command(Enum):
    STAGE = 'stage'
    RESTORE = 'restore'
    BUILD = 'build'
    VERIFY = 'verify'


def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def write(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + '\n')


def execute(argv, cwd=ROOT, log=None, env=None):
    if log is None:
        return subprocess.check_output(list(map(str, argv)), cwd=cwd, text=True, env=env)
    with log.open('wb') as output:
        subprocess.run(list(map(str, argv)), cwd=cwd, env=env, stdout=output,
                       stderr=subprocess.STDOUT, check=True)


def files(root):
    result = {}
    root = root.resolve()
    def entry(path):
        if path.is_symlink():
            if not path.resolve(strict=True).is_relative_to(root):
                raise RuntimeError('Source symlink escapes its identified tree')
            return dict(symlink=os.readlink(path))
        return sha(path)
    for directory, dirs, names in os.walk(root, followlinks=False):
        dirs[:] = sorted(name for name in dirs if name not in ('.git', '.tools', 'target', '__pycache__'))
        for name in dirs:
            path = Path(directory) / name
            if path.is_symlink():
                result[path.relative_to(root).as_posix()] = entry(path)
        for name in sorted(names):
            path = Path(directory) / name
            if path.is_symlink() or path.is_file():
                result[path.relative_to(root).as_posix()] = entry(path)
    return result


def empty_destination(path):
    path = path.expanduser().absolute()
    if path.exists() or path.is_symlink():
        raise RuntimeError('Preserve the existing output: select a fresh directory')
    # Avoid creating a release inside this checkout or any of its parents.
    resolved = path.resolve()
    if resolved == ROOT or ROOT in resolved.parents or resolved in ROOT.parents:
        raise RuntimeError('Delivery output must be outside product sources')
    path.mkdir(mode=0o700, parents=True)
    return path


def configure_sources(stage):
    home = stage / 'sources' / Source.HOMEWARD.value
    free = stage / 'sources' / Source.FREE.value
    pro = stage / 'sources' / Source.PRO.value
    tools = home / '.tools/frontend-kits'
    tools.mkdir(parents=True)
    (tools / 'pro').symlink_to(os.path.relpath(pro, tools), target_is_directory=True)
    config = home / '.cargo/config.toml'
    config.parent.mkdir(exist_ok=True)
    if config.exists():
        raise RuntimeError('Review an exported Cargo config before overriding it')
    crates = {'slint-kit': 'kit', 'slint-kit-core': 'kit-core',
              'slint-kit-slint': 'kit-slint', 'slint-kit-build': 'kit-build',
              'slint-kit-native': 'kit-native'}
    lines = ['[patch.crates-io]']
    for crate, directory in crates.items():
        # Cargo reads these paths relative to the source workspace root.
        path = os.path.relpath(free / 'crates' / directory, home)
        lines.append(crate + ' = { path = ' + json.dumps(path) + ' }')
    config.write_text('\n'.join(lines) + '\n')


def stage_sources(destination):
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    roots = {Source.HOMEWARD: ROOT, Source.FREE: Path(config['free_kit']), Source.PRO: Path(config['pro_kit'])}
    identities = {}
    for source, root in roots.items():
        if execute(['git', 'status', '--porcelain'], root):
            raise RuntimeError('Commit the configured source before freezing: ' + source.value)
        identities[source.value] = dict(revision=execute(['git', 'rev-parse', 'HEAD'], root).strip(),
                                       tree=execute(['git', 'rev-parse', 'HEAD^{tree}'], root).strip())
    stage = empty_destination(destination)
    archives = stage / 'source-archives'
    archives.mkdir()
    (stage / 'sources').mkdir()
    for source, root in roots.items():
        archive = archives / (source.value + '-' + identities[source.value]['revision'][:12] + '.tar')
        execute(['git', 'archive', '--format=tar', '--prefix=' + source.value + '/',
                 '--output=' + str(archive), identities[source.value]['revision']], root)
        with tarfile.open(archive) as stream:
            stream.extractall(stage / 'sources', filter='data')
        identities[source.value].update(archive=str(archive.relative_to(stage)), archive_sha256=sha(archive))
    configure_sources(stage)
    for source in Source:
        identities[source.value]['files'] = files(stage / 'sources' / source.value)
    bootstrap = stage / 'frontend-delivery.py'
    shutil.copyfile(stage / 'sources/homeward/scripts/frontend-delivery.py',bootstrap)
    record = dict(format=1,mode='versioned-source-artifacts',distribution='local-evaluation-only',
                  sources=identities,free_remote_availability='not asserted; exact local source archive supplied',
                  bootstrap_sha256=sha(bootstrap),
                  private_source_notice='Pro source archive remains private. No remote availability, commercial license clearance or store publication is asserted.',
                  no_signing_credentials_exported=True)
    write(stage / 'source-record.json', record)
    # Keep premium sources separate from the installable-binary directory.
    private_bundle = stage / 'PRIVATE-source-artifacts.tar.gz'
    with tarfile.open(private_bundle,'w:gz') as stream:
        for path in [stage / 'source-record.json',bootstrap,*sorted(archives.iterdir())]:
            stream.add(path,arcname=str(path.relative_to(stage)),recursive=False)
    (stage / 'PRIVATE-source-artifacts.sha256').write_text(sha(private_bundle)+'  '+private_bundle.name+'\n')
    print(stage / 'source-record.json')


def restore_sources(stage):
    stage = stage.expanduser().resolve()
    record = json.loads((stage / 'source-record.json').read_text())
    if record.get('format') != 1 or record.get('mode') != 'versioned-source-artifacts':
        raise RuntimeError('Unsupported source artifact format')
    if (stage / 'sources').exists() or (stage / 'sources').is_symlink():
        raise RuntimeError('Preserve existing source trees; restore in a fresh directory')
    for source in Source:
        item = record['sources'][source.value]
        archive = stage / item['archive']
        if not archive.resolve().is_relative_to(stage) or sha(archive) != item['archive_sha256']:
            raise RuntimeError('Source archive checksum mismatch: ' + source.value)
    (stage / 'sources').mkdir()
    for source in Source:
        with tarfile.open(stage / record['sources'][source.value]['archive']) as stream:
            stream.extractall(stage / 'sources',filter='data')
    configure_sources(stage)
    verify_sources(stage)


def verify_sources(stage):
    stage = stage.expanduser().resolve()
    record = json.loads((stage / 'source-record.json').read_text())
    if record.get('format') != 1 or record.get('mode') != 'versioned-source-artifacts':
        raise RuntimeError('Unsupported source artifact format')
    if set(record['sources']) != {source.value for source in Source}:
        raise RuntimeError('Exactly three identified source trees are required')
    if sha(stage / 'frontend-delivery.py') != record['bootstrap_sha256']:
        raise RuntimeError('Source bootstrap checksum mismatch')
    for source in Source:
        item = record['sources'][source.value]
        archive = stage / item['archive']
        if not archive.resolve().is_relative_to(stage) or sha(archive) != item['archive_sha256']:
            raise RuntimeError('Source archive checksum mismatch: ' + source.value)
        if files(stage / 'sources' / source.value) != item['files']:
            raise RuntimeError('Source tree changed: ' + source.value)
    home = stage / 'sources' / Source.HOMEWARD.value
    pro = stage / 'sources' / Source.PRO.value
    if (home / '.tools/frontend-kits/pro').resolve() != pro.resolve():
        raise RuntimeError('Premium source link changed')
    return stage, record


def verify_artifacts(stage):
    stage, record = verify_sources(stage)
    destination = stage / 'artifacts'
    if not destination.exists():
        return
    manifest = destination / 'SHA256SUMS'
    if not manifest.is_file():
        raise RuntimeError('Artifact build is incomplete: SHA256SUMS missing')
    checksums = {}
    for line in manifest.read_text().splitlines():
        digest, name = line.split('  ', 1)
        path = destination / name
        if not re.fullmatch('[a-f0-9]{64}', digest) or not path.resolve().is_relative_to(destination):
            raise RuntimeError('Unsafe artifact checksum entry')
        if name in checksums or not path.is_file() or sha(path) != digest:
            raise RuntimeError('Artifact checksum mismatch: ' + name)
        checksums[name] = digest
    actual = {str(path.relative_to(destination)) for path in destination.rglob('*') if path.is_file() and path != manifest}
    if actual != set(checksums):
        raise RuntimeError('Unlisted or missing delivery artifact')


def license_inventory(stage, metadata, destination):
    rows = []
    texts = destination / 'license-texts'
    texts.mkdir(exist_ok=True)
    for package in metadata['packages']:
        root = Path(package['manifest_path']).parent.resolve()
        allowed = [root]
        allowed += [stage / 'sources' / source.value for source in Source if root.is_relative_to(stage / 'sources' / source.value)]
        candidates = [p for p in root.iterdir() if p.name.upper().startswith(('LICENSE', 'LICENCE', 'COPYING', 'NOTICE')) and p.is_file()]
        if package.get('license_file'):
            candidates.append(root / package['license_file'])
        copied = []
        for path in sorted(set(candidates)):
            resolved = path.resolve()
            if not resolved.is_file() or not any(resolved.is_relative_to(base.resolve()) for base in allowed):
                continue
            digest = sha(resolved)
            target = texts / (digest + '.txt')
            if not target.exists():
                shutil.copyfile(resolved, target)
            copied.append(dict(name=path.name, sha256=digest, file=str(target.relative_to(destination))))
        rows.append(dict(name=package['name'], version=package['version'], license_expression=package.get('license'), texts=copied))
    repositories = []
    for source in Source:
        for path in (stage / 'sources' / source.value).iterdir():
            if path.is_file() and path.name.upper().startswith(('LICENSE', 'LICENCE', 'COPYING', 'NOTICE')):
                digest = sha(path)
                shutil.copyfile(path, texts / (digest + '.txt'))
                repositories.append(dict(source=source.value,name=path.name,sha256=digest,file='license-texts/' + digest + '.txt'))
    write(destination / 'licenses.json',dict(scope='Resolved locked workspace graph, including build/dev dependencies; SPDX metadata and discovered package/root license texts. Not a final-binary SBOM or commercial-distribution license clearance.',packages=rows,repositories=repositories))


def macos_app(stage, record, destination, env):
    if platform.system() != 'Darwin' or platform.machine() != 'arm64':
        raise RuntimeError('This packaging path qualifies only native Apple Silicon macOS')
    home = stage / 'sources' / Source.HOMEWARD.value
    execute(['cargo','build','--locked','--release','-p','homeward-app','--bin','homeward-desktop'], home, destination / 'macos-build.log', env)
    binary = Path(env['CARGO_TARGET_DIR']) / 'release/homeward-desktop'
    dependencies = execute(['otool','-L',binary]).splitlines()[1:]
    if any(not line.strip().startswith(('/System/Library/', '/usr/lib/')) for line in dependencies):
        raise RuntimeError('Review/bundle non-system dynamic libraries before claiming an installable app')
    load = execute(['otool','-l',binary])
    minimum = re.search(r'cmd LC_BUILD_VERSION\n(?:[^\n]*\n)*?\s+minos (\d+(?:\.\d+){1,2})',load)
    if not minimum:
        raise RuntimeError('Mach-O minimum OS version unavailable')
    bundle = destination / 'Homeward.app/Contents'
    (bundle / 'MacOS').mkdir(parents=True)
    (bundle / 'Resources').mkdir()
    shutil.copy2(binary,bundle / 'MacOS/homeward-desktop')
    manifest = tomllib.loads((home / 'client/app/Cargo.toml').read_text())
    info = dict(CFBundleExecutable='homeward-desktop',CFBundleIdentifier='ca.homeward.frontend.dev',
                CFBundleName='Homeward',CFBundleDisplayName='Homeward',CFBundlePackageType='APPL',
                CFBundleShortVersionString=manifest['package']['version'],CFBundleVersion=manifest['package']['version'],
                NSHighResolutionCapable=True,LSMinimumSystemVersion=minimum[1],
                HomewardSourceRevision=record['sources'][Source.HOMEWARD.value]['revision'])
    with (bundle / 'Info.plist').open('wb') as output:
        plistlib.dump(info,output)
    write(bundle / 'Resources/source-identity.json',dict(sources={name:{key:value[key] for key in ('revision','tree')} for name,value in record['sources'].items()},distribution='local-evaluation-only'))
    shutil.copyfile(destination / 'licenses.json',bundle / 'Resources/licenses.json')
    shutil.copytree(destination / 'license-texts',bundle / 'Resources/license-texts')
    execute(['codesign','--force','--sign','-','--timestamp=none',bundle.parent], log=destination / 'macos-sign.log')
    execute(['codesign','--verify','--deep','--strict',bundle.parent], log=destination / 'macos-sign-verify.log')
    execute(['ditto','-c','-k','--keepParent',bundle.parent,destination / 'Homeward-macos-arm64.zip'])
    return dict(bundle='Homeward.app',archive='Homeward-macos-arm64.zip',archive_sha256=sha(destination / 'Homeward-macos-arm64.zip'),
                executable_sha256=sha(bundle / 'MacOS/homeward-desktop'),version=manifest['package']['version'],minimum_os=minimum[1],
                architecture='arm64',signature='ad-hoc local evaluation; not notarized or store distribution',system_libraries=[line.strip() for line in dependencies])


def android_app(stage, destination, env):
    if not env.get('CARGO_APK_RELEASE_KEYSTORE') or 'CARGO_APK_RELEASE_KEYSTORE_PASSWORD' not in env:
        raise RuntimeError('Supply Android release keystore and password externally; do not place them in source or delivery manifests')
    key = Path(env['CARGO_APK_RELEASE_KEYSTORE']).expanduser().resolve(strict=True)
    if key.is_relative_to(stage) or key.is_relative_to(ROOT):
        raise RuntimeError('Keep the signing keystore outside source and delivery trees')
    home = stage / 'sources' / Source.HOMEWARD.value
    execute([sys.executable,'scripts/build-frontend-android.py','--profile','release'],home,destination / 'android-build.log',env)
    apk = destination / 'Homeward-android-arm64.apk'
    shutil.copy2(Path(env['CARGO_TARGET_DIR']) / 'release/apk/homeward-dev.apk',apk)
    tools = Path(env['ANDROID_HOME']) / 'build-tools/35.0.0'
    badging = execute([tools / 'aapt','dump','badging',apk])
    if 'application-debuggable' in badging or "native-code: 'arm64-v8a'" not in badging:
        raise RuntimeError('Expected an optimized non-debuggable ARM64 release APK')
    signer = execute([tools / 'apksigner','verify','--print-certs',apk])
    (destination / 'android-manifest.txt').write_text(badging)
    (destination / 'android-signer.txt').write_text(signer)
    return dict(apk=apk.name,sha256=sha(apk),bytes=apk.stat().st_size,architecture='arm64-v8a',debuggable=False,
                signing='external release keystore; see public certificate in android-signer.txt; production signing not claimed')


def build_apps(stage, selection):
    if not os.environ.get('CARGO_TARGET_DIR'):
        raise RuntimeError('Use the shared frontend.py build reservation')
    stage, record = verify_sources(stage)
    destination = stage / 'artifacts'
    if destination.exists():
        raise RuntimeError('Preserve existing build artifacts; select a fresh source stage')
    destination.mkdir(mode=0o700)
    home = stage / 'sources' / Source.HOMEWARD.value
    env = dict(os.environ,PYTHONDONTWRITEBYTECODE='1')
    # Machine-local patch/link configuration was generated during staging.
    packages = {}
    for target in selection.targets():
        metadata = json.loads(execute(['cargo','metadata','--locked','--format-version','1','--filter-platform',target],home,env=env))
        packages.update({package['id']:package for package in metadata['packages']})
    metadata = dict(packages=sorted(packages.values(),key=lambda package:(package['name'],package['version'],package['id'])))
    license_inventory(stage,metadata,destination)
    shutil.copyfile(home / 'Cargo.lock',destination / 'Cargo.lock')
    tool_versions = dict(python=platform.python_version(),host=platform.platform(),architecture=platform.machine())
    if selection is not Platform.ANDROID:
        tool_versions['macos_sdk']=execute(['xcrun','--show-sdk-version']).strip()
    if selection is not Platform.MACOS:
        # cargo-apk0.10 exposes no --version flag. Record its actual binary
        # and the Cargo installer version without copying registry URLs.
        apk_tool=shutil.which('cargo-apk')
        if apk_tool is None:
            raise RuntimeError('cargo-apk must be installed on PATH')
        apk_tool=Path(apk_tool).resolve()
        installed=apk_tool.parent.parent / '.crates.toml'
        versions=[]
        if installed.is_file():
            for key in tomllib.loads(installed.read_text()).get('v1',{}):
                found=re.fullmatch(r'cargo-apk ([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?) \(.+\)',key)
                if found:versions.append(found[1])
        tool_versions['cargo_apk']=dict(version=versions[0] if len(versions)==1 else None,binary_sha256=sha(apk_tool),
                                       version_source='Cargo installer metadata when available; CLI has no version flag')
        java_version=subprocess.run([str(Path(env['JAVA_HOME']) / 'bin/javac'),'-version'],capture_output=True,text=True,check=True)
        tool_versions['javac']=(java_version.stdout+java_version.stderr).strip()
        for name, path in [('android_build_tools',Path(env['ANDROID_HOME']) / 'build-tools/35.0.0/source.properties'),
                           ('android_ndk',Path(env['ANDROID_NDK_ROOT']) / 'source.properties')]:
            tool_versions[name]=next(line.split('=',1)[1].strip() for line in path.read_text().splitlines() if line.startswith('Pkg.Revision'))
    result = dict(status='BUILDING',source_record='../source-record.json',distribution='local-evaluation-only',
                  rustc=execute(['rustc','-Vv'],home).strip(),cargo=execute(['cargo','-V'],home).strip(),
                  tool_versions=tool_versions,
                  sources={name:{key:value[key] for key in ('revision','tree')} for name,value in record['sources'].items()},
                  component_versions=[dict(name=package['name'],version=package['version']) for package in metadata['packages'] if package['name'] in ('slint','slint-kit','slint-kit-pro','connectrpc')],
                  license_inventory_targets=selection.targets(),platforms={})
    write(destination / 'build-record.json',result)
    selected = (Platform.MACOS, Platform.ANDROID) if selection is Platform.ALL else (selection,)
    for target in selected:
        try:
            match target:
                case Platform.MACOS:
                    result['platforms'][target.value]=macos_app(stage,record,destination,env)
                case Platform.ANDROID:
                    result['platforms'][target.value]=android_app(stage,destination,env)
                case Platform.ALL:
                    raise RuntimeError('A concrete packaging platform is required')
            write(destination / 'build-record.json',result)
        except BaseException as error:
            result.update(status='FAILED',failed_platform=target.value,failure=type(error).__name__)
            write(destination / 'build-record.json',result)
            raise
    verify_sources(stage)
    if (destination / 'Cargo.lock').read_bytes() != (home / 'Cargo.lock').read_bytes():
        raise RuntimeError('Build changed the exported lockfile')
    result['status']='BUILT_NOT_INSTALLATION_QUALIFIED'
    write(destination / 'build-record.json',result)
    checksums={str(path.relative_to(destination)):sha(path) for path in sorted(destination.rglob('*')) if path.is_file() and path.name!='SHA256SUMS'}
    (destination / 'SHA256SUMS').write_text(''.join(digest+'  '+name+'\n' for name,digest in checksums.items()))
    print(destination / 'build-record.json')


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    commands=parser.add_subparsers(dest='command',required=True)
    stage=commands.add_parser(Command.STAGE.value)
    stage.add_argument('--output',required=True,type=Path)
    restore=commands.add_parser(Command.RESTORE.value)
    restore.add_argument('--stage',required=True,type=Path)
    build=commands.add_parser(Command.BUILD.value)
    build.add_argument('--stage',required=True,type=Path)
    build.add_argument('--platform',type=Platform,choices=list(Platform),default=Platform.ALL)
    verify=commands.add_parser(Command.VERIFY.value)
    verify.add_argument('--stage',required=True,type=Path)
    args=parser.parse_args()
    match Command(args.command):
        case Command.STAGE:stage_sources(args.output)
        case Command.RESTORE:restore_sources(args.stage);print('Pinned source trees restored')
        case Command.BUILD:build_apps(args.stage,args.platform)
        case Command.VERIFY:verify_artifacts(args.stage);print('Source/artifact checksums verified')


if __name__=='__main__':
    main()
