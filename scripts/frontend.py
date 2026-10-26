#!/usr/bin/env python3
"""Local native-client setup and serialized builds; machine paths stay untracked."""
import argparse
import contextlib
from enum import Enum
import fcntl
import getpass
import hashlib
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import time

REPO = Path(__file__).resolve().parents[1]
LOCAL = REPO / 'frontend.local.json'
LIMIT = 40_000_000_000
# The shared host's latest reserve supersedes the original 40 GiB floor.
MIN_FREE = 50_000_000_000
PACKAGES = ['home-client-model', 'home-wire-contract', 'homeward-app', 'home-client-transport', 'home-client-cache',
            'home-client-session', 'home-client-platform', 'home-client-actions', 'i-slint-backend-android-activity',
            'slint-kit', 'slint-kit-core', 'slint-kit-slint', 'slint-kit-build',
            'slint-kit-pro', 'slint-kit-pro-build']


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temp = path.with_suffix(path.suffix + '.writing')
    temp.write_text(json.dumps(value, indent=2) + '\n')
    temp.replace(path)


def inventory(roots):
    """Physical file bytes, deduplicated by inode; directory symlinks stay local."""
    def inaccessible(error):
        # Builders retire temporary directories concurrently with a sample.
        # Only a confirmed missing descendant has no remaining bytes to charge.
        if isinstance(error, FileNotFoundError) and error.filename:
            missing = Path(error.filename)
            if missing != root and missing.is_relative_to(root) and not os.path.lexists(missing):
                return
        raise error
    result, seen = {}, set()
    for root in roots:
        if not root.exists():
            continue
        entries = [(root.parent, [], [root.name])] if root.is_file() else os.walk(root, followlinks=False, onerror=inaccessible)
        for directory, dirs, files in entries:
            dirs[:] = [name for name in dirs if not (Path(directory) / name).is_symlink()]
            for name in files:
                path = Path(directory) / name
                try:
                    info = path.lstat()
                except FileNotFoundError:
                    continue  # A retired temporary has no allocated bytes to charge.
                identity = (info.st_dev, info.st_ino)
                if identity in seen:
                    continue
                seen.add(identity)
                result[str(path)] = [*identity, info.st_size, info.st_mtime_ns, info.st_blocks * 512]
    return result


def policy(used, free, reserve):
    return 0 <= used and 0 <= free and 0 <= reserve and used + reserve <= LIMIT and free - reserve >= MIN_FREE


def measure(config, reserve=0):
    before = json.loads(Path(config['baseline']).read_text())
    now = inventory([Path(p) for p in config['shared_roots'] + config['owned_roots']])
    used = sum(value[-1] for path, value in now.items() if before.get(path) != value)
    fs = os.statvfs(REPO)
    free = fs.f_bavail * fs.f_frsize
    result = {'utc_unix': time.time(), 'frontend_bytes': used, 'limit_bytes': LIMIT,
              'free_bytes': free, 'min_free_bytes': MIN_FREE, 'reserve_bytes': reserve,
              'within_policy': policy(used, free, reserve)}
    print(json.dumps(result), flush=True)
    return result


def sources(config, proof):
    manifests = {}
    for name, repo in [('homeward', REPO), ('free', Path(config['free_kit'])),
                       ('pro', Path(config['pro_kit']))]:
        def git(*args):
            return subprocess.run(['git', '-C', str(repo), *args], capture_output=True, check=True).stdout
        head = subprocess.run(['git', '-C', str(repo), 'rev-parse', '--verify', 'HEAD'],
                              capture_output=True, text=True)
        paths = git('ls-files', '--cached', '--others', '--exclude-standard', '-z').split(b'\0')
        files = {}
        for raw in paths:
            if raw:
                path = repo / os.fsdecode(raw)
                if path.is_file():
                    files[os.fsdecode(raw)] = hashlib.sha256(path.read_bytes()).hexdigest()
        for raw in git('ls-files', '--others', '--exclude-standard', '-z').split(b'\0'):
            if raw:
                relative = Path(os.fsdecode(raw))
                path = repo / relative
                if path.is_file():
                    destination = proof / 'untracked' / name / relative
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(path, destination)
        patch = git('diff', '--binary', 'HEAD') if head.returncode == 0 else b''
        (proof / (name + '.patch')).write_bytes(patch)
        manifests[name] = {'path': str(repo), 'head': head.stdout.strip() or None,
                           'status': git('status', '--short').decode(), 'files': files}
    write(proof / 'sources.json', manifests)
    return hashlib.sha256(json.dumps(manifests, sort_keys=True).encode()).hexdigest()


def environment(config):
    base = Path(config['backend_cache'])
    ndk = Path(config['android_sdk']) / 'ndk/27.2.12479018'
    android_tools = ndk / 'toolchains/llvm/prebuilt/darwin-x86_64/bin'
    env = os.environ.copy()
    env.update(PATH=str(REPO / '.tools/bin') + ':' + env['PATH'], GOTOOLCHAIN='local',
               GOCACHE=str(base / 'go-build'), GOTMPDIR=str(base / 'go-tmp'), TMPDIR=str(base / 'go-tmp'),
               CARGO_TARGET_DIR=str(base / 'workspace/target'), CARGO_INCREMENTAL='0',
               CARGO_PROFILE_DEV_DEBUG='line-tables-only', CARGO_PROFILE_TEST_DEBUG='line-tables-only',
               ANDROID_HOME=config['android_sdk'], ANDROID_NDK_ROOT=str(ndk),
               CC_aarch64_linux_android=str(android_tools / 'aarch64-linux-android26-clang'),
               CXX_aarch64_linux_android=str(android_tools / 'aarch64-linux-android26-clang++'),
               AR_aarch64_linux_android=str(android_tools / 'llvm-ar'),
               CARGO_TARGET_AARCH64_LINUX_ANDROID_LINKER=str(android_tools / 'aarch64-linux-android26-clang'),
               ANDROID_AVD_HOME=config['frontend_cache'] + '/android-avd',
               ANDROID_USER_HOME=config['frontend_cache'] + '/android-user',
               HOMEBREW_CACHE=config['frontend_cache'] + '/homebrew',
               HOMEBREW_NO_AUTO_UPDATE='1', HOMEBREW_NO_INSTALL_CLEANUP='1',
               JAVA_HOME='/opt/homebrew/opt/openjdk/libexec/openjdk.jdk/Contents/Home',
               PYTHONDONTWRITEBYTECODE='1')
    return env


@contextlib.contextmanager
def leases(config):
    handles = []
    try:
        for name in ['cohort', 'go', 'cargo', 'tools']:
            path = Path(config['backend_cache']) / 'qualification' / ('parallel-' + name + '.lock')
            handle = path.open('a')
            handles.append(handle)
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        for handle in reversed(handles):
            handle.close()


def configure(args):
    if LOCAL.exists():
        raise SystemExit('already configured; preserve the original storage baseline')
    free, pro, cache = args.free_kit.resolve(), args.pro_kit.resolve(), args.backend_cache.resolve()
    frontend = cache / 'frontend'
    if frontend.exists():
        raise SystemExit('frontend cache already exists; inspect before creating a baseline')
    for path in [free / 'crates/kit/Cargo.toml', pro / 'crates/kit-pro/Cargo.toml']:
        if not path.is_file():
            raise SystemExit('missing kit manifest: ' + str(path))
    links = REPO / '.tools/frontend-kits'
    links.mkdir(exist_ok=True)
    for name, target in [('free', free), ('pro', pro)]:
        link = links / name
        if not link.exists():
            link.symlink_to(target, target_is_directory=True)
        elif not link.is_symlink() or link.resolve() != target:
            raise SystemExit('preserve existing kit link: ' + str(link))
    patch = '[patch.crates-io]\n'
    for name in ['kit', 'kit-core', 'kit-slint', 'kit-build', 'kit-native']:
        patch += f'slint-{name} = {{ path = {json.dumps(str(free / "crates" / name))} }}\n'
    for repo in [REPO, pro]:
        config = repo / '.cargo/config.toml'
        if config.exists():
            raise SystemExit('preserve existing Cargo config: ' + str(config))
        config.parent.mkdir(exist_ok=True)
        config.write_text(patch)
    config = {'free_kit': str(free), 'pro_kit': str(pro), 'backend_cache': str(cache),
              'frontend_cache': str(frontend), 'android_sdk': '/opt/homebrew/share/android-commandlinetools',
              'baseline': str(frontend / 'storage-baseline.json'),
              'shared_roots': [str(cache / p) for p in ['workspace/target', 'go-build', 'go-tmp']] +
                  [str(REPO / '.tools'), str(Path.home() / '.cargo/registry')],
              'owned_roots': [str(frontend), str(Path.home() / 'Library/Application Support/Homeward'),
                              str(Path('/tmp') / ('android-' + getpass.getuser())),
                              str(Path.home() / 'Library/Caches/TemporaryItems/avd'), '/opt/homebrew/share/android-commandlinetools',
                              '/opt/homebrew/Caskroom/android-commandlinetools',
                              '/opt/homebrew/Caskroom/android-platform-tools',
                              str(Path.home() / '.android/debug.keystore'),
                              str(Path.home() / '.rustup/toolchains/1.98.1-aarch64-apple-darwin/lib/rustlib/aarch64-linux-android')]}
    # Existing backend allocations are baseline-only, never a second frontend charge.
    baseline = inventory([Path(p) for p in config['shared_roots'] + config['owned_roots']])
    write(Path(config['baseline']), baseline)
    write(LOCAL, config)
    print('Configured local kit paths and immutable pre-frontend storage baseline.')


def stop_owned_process(process):
    """Join the child even if it exits between the budget sample and signal."""
    if process.poll() is None:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        except PermissionError:
            # Darwin can reject a group signal after its leader has exited.
            # Suppress that race only after reaping our actual child.
            if process.poll() is None:
                raise
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()


def execute(config, argv, reserve, uses_cargo=False):
    if argv[:1] == ['--']:
        argv = argv[1:]
    if not argv:
        raise SystemExit('a command is required after --')
    env = environment(config)
    proof = Path(config['frontend_cache']) / 'rounds' / str(time.time_ns())
    proof.mkdir(parents=True)
    with leases(config):
        before = measure(config, reserve)
        write(proof / 'before.json', before)
        if not before['within_policy']:
            raise SystemExit('frontend storage reserve unavailable; command not started')
        fingerprint = sources(config, proof)
        owner_path = Path(config['backend_cache']) / 'qualification/parallel-cargo-owner.json'
        previous = json.loads(owner_path.read_text()) if owner_path.exists() else {}
        write(proof / 'previous-cargo-owner.json', previous)
        uses_cargo = uses_cargo or argv[0] in {'cargo', 'make'}
        if uses_cargo:
            write(owner_path, {'owner': str(REPO), 'key': fingerprint, 'status': 'invalid'})
        if uses_cargo and (previous.get('owner') != str(REPO) or previous.get('status') != 'valid' or previous.get('key') != fingerprint):
            clean = ['cargo', 'clean']
            for name in PACKAGES:
                clean.extend(['-p', name])
            subprocess.run(clean, cwd=REPO, env=env, check=True)
        write(proof / 'command.json', {'argv': argv, 'cwd': str(REPO), 'source_manifest': fingerprint})
        started = time.monotonic()
        with (proof / 'output.log').open('wb') as log:
            process = subprocess.Popen(argv, cwd=REPO, env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            print(f'PID {process.pid}; log {proof / "output.log"}', flush=True)
            failure = None
            try:
                while True:
                    try:
                        code = process.wait(timeout=20)
                        break
                    except subprocess.TimeoutExpired:
                        sample = measure(config, 512 * 1024**2)
                        with (proof / 'storage.jsonl').open('a') as output:
                            output.write(json.dumps(sample) + '\n')
                        if not sample['within_policy']:
                            raise RuntimeError('storage margin exhausted; stopping owned process group')
            except BaseException as error:
                failure = str(error)
                stop_owned_process(process)
                code = 1
            after = measure(config)
            if uses_cargo and code == 0:
                write(owner_path, {'owner': str(REPO), 'key': fingerprint, 'status': 'valid'})
            write(proof / 'result.json', {'returncode': code, 'failure': failure,
                  'elapsed_seconds': time.monotonic() - started, 'after': after})
        print((proof / 'output.log').read_text(errors='replace')[-6000:])
        return code if after['within_policy'] else 1


class EvaluationPackages(Enum):
    APP = 'app'
    FREE = 'free'
    PRO = 'pro'

    def __str__(self):
        return self.value


def retire_evaluations(config, selection):
    """Recover storage without admitting an arbitrary command over budget."""
    match selection:
        case EvaluationPackages.APP:
            repository, packages = REPO, ['homeward-app']
        case EvaluationPackages.FREE:
            repository, packages = Path(config['free_kit']), ['kit-tests', 'kit-showcase']
        case EvaluationPackages.PRO:
            repository, packages = Path(config['pro_kit']), ['slint-kit-pro-tests', 'slint-kit-pro-examples']
    proof = Path(config['frontend_cache']) / 'rounds' / str(time.time_ns())
    proof.mkdir(parents=True)
    with leases(config):
        before = measure(config)
        write(proof / 'before.json', before)
        baseline = json.loads(Path(config['baseline']).read_text())
        markers = [marker for package in packages for marker in (package, package.replace('-', '_'))]
        if any(marker in path for path in baseline for marker in markers):
            raise SystemExit('evaluation package has baseline artifacts; refusing retirement')
        processes = subprocess.check_output(['ps', '-axo', 'comm'], text=True).splitlines()[1:]
        if any(Path(process.strip()).name in {'cargo', 'rustc', 'go'} for process in processes):
            raise SystemExit('a builder is active; refusing retirement')
        env = environment(config)
        target = Path(env['CARGO_TARGET_DIR'])
        opened = subprocess.run(['lsof', '-t', '+D', str(target / 'debug/deps')],
                                capture_output=True, timeout=45)
        if opened.returncode != 1 or opened.stdout:
            raise SystemExit('target quiescence is unproven; refusing retirement')
        fingerprint = sources(config, proof)
        command = ['cargo', 'clean']
        for package in packages:
            command.extend(['-p', package])
        write(proof / 'command.json', {'argv': command, 'cwd': str(repository),
              'source_manifest': fingerprint, 'evaluation_packages': selection.value,
              'recovery_only': True, 'baseline_had_no_matching_artifacts': True})
        started = time.monotonic()
        with (proof / 'output.log').open('wb') as log:
            result = subprocess.run(command, cwd=repository, env=env,
                                    stdout=log, stderr=subprocess.STDOUT)
        after = measure(config)
        write(proof / 'result.json', {'returncode': result.returncode,
              'elapsed_seconds': time.monotonic() - started, 'after': after,
              'only_owned_evaluation_packages': True, 'all_four_leases_held': True})
        print(proof, flush=True)
        return result.returncode if after['within_policy'] else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    setup = commands.add_parser('configure')
    setup.add_argument('--free-kit', type=Path, required=True)
    setup.add_argument('--pro-kit', type=Path, required=True)
    setup.add_argument('--backend-cache', type=Path, required=True)
    commands.add_parser('storage')
    run = commands.add_parser('run')
    run.add_argument('--reserve-gb', type=float, default=2)
    run.add_argument('--cargo', action='store_true', help='command invokes Cargo indirectly')
    run.add_argument('--retire-evaluations', type=EvaluationPackages, choices=list(EvaluationPackages),
                     help='retire only non-baseline app/free/pro evaluation packages, including over-budget recovery')
    run.add_argument('argv', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command == 'configure':
        configure(args)
        return 0
    config = json.loads(LOCAL.read_text())
    if args.command == 'storage':
        return 0 if measure(config)['within_policy'] else 1
    if not 0 <= args.reserve_gb <= 40:
        parser.error('--reserve-gb must be between 0 and 40')
    if args.retire_evaluations is not None:
        if args.argv or args.cargo:
            parser.error('--retire-evaluations cannot run another command')
        return retire_evaluations(config, args.retire_evaluations)
    return execute(config, args.argv, int(args.reserve_gb * 1_000_000_000), args.cargo)


if __name__ == '__main__':
    sys.exit(main())
