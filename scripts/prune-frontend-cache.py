#!/usr/bin/env python3
"""Prune rebuildable qualification binaries; retain source, logs and active proofs.

Call through frontend.py run --reserve-gb 0 while all builders are stopped.
--keep names cache-relative proof directories whose payloads must remain raw.
"""
import argparse
from enum import Enum
import hashlib
import json
import os
import re
from pathlib import Path
import stat
import subprocess
import tarfile
import time

ROOT = Path(__file__).resolve().parents[1]
MAGIC = (b'\x7fELF', b'\xcf\xfa\xed\xfe', b'\xce\xfa\xed\xfe',
         b'\xfe\xed\xfa\xcf', b'\xfe\xed\xfa\xce', b'\xca\xfe\xba\xbe')
SOURCE_DIRS = {'source', 'sources', '.git', '.tools'}


class DeliveryState(Enum):
    DELIVERED = 'DELIVERED'


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def compiled(name, size, header):
    if size < 1_000_000 or SOURCE_DIRS.intersection(Path(name).parts):
        return False
    return (header.startswith(MAGIC) or name.endswith('.apk')
            or (header.startswith(b'!<arch>\n') and name.endswith(('.a', '.rlib')))
            or name.endswith('.rmeta'))


def protected(path, cache, keeps):
    relative = path.relative_to(cache)
    return (any(path == keep or path.is_relative_to(keep) for keep in keeps)
            or SOURCE_DIRS.intersection(relative.parts)
            or (relative.parts[0].startswith('migrations-') and 'baseline' in relative.parts))


class PrefixReader:
    def __init__(self, prefix, source):
        self.prefix, self.source = prefix, source
        self.hash = hashlib.sha256()

    def read(self, length):
        head = self.prefix[:length]
        self.prefix = self.prefix[len(head):]
        result = head + self.source.read(length - len(head))
        self.hash.update(result)
        return result


def prune_archive(path, apply):
    before = digest(path)
    journal = path.with_name(path.name + '.retention.json')
    if journal.exists() and json.loads(journal.read_text()).get('replacement_sha256') == before:
        return None  # Already verified and stripped; do not repack source again.
    temporary = path.with_name(path.name + '.retention-writing')
    if temporary.exists():
        raise RuntimeError('Preserve an unfinished archive replacement: ' + str(temporary))
    removed, retained, dropped, seen = [], [], set(), set()
    output = (tarfile.open(temporary, 'w:xz', preset=0) if path.name.endswith('.xz')
              else tarfile.open(temporary, 'w:gz', compresslevel=1)) if apply else None
    try:
        with tarfile.open(path, 'r:*') as source:
            for member in source:
                # Generated archives use backward hardlinks. Refuse ambiguous
                # references rather than leaving a broken retained archive.
                if member.islnk() and member.linkname not in seen:
                    raise RuntimeError('Forward archive hardlink requires inspection')
                if (member.islnk() and member.linkname in dropped
                        and SOURCE_DIRS.intersection(Path(member.name).parts)):
                    raise RuntimeError('Preserve an archive containing a source link to a build payload')
                stream = source.extractfile(member) if member.isfile() else None
                header = stream.read(8) if stream else b''
                discard = ((member.islnk() and member.linkname in dropped)
                           or (member.isfile() and compiled(member.name, member.size, header)))
                seen.add(member.name)
                if discard:
                    dropped.add(member.name)
                    reader = PrefixReader(header, stream) if stream else None
                    if reader:
                        while reader.read(1024 * 1024):
                            pass
                    removed.append(dict(path=member.name, size=member.size,
                                        sha256=reader.hash.hexdigest() if reader else None,
                                        link=member.linkname))
                elif output is not None:
                    reader = PrefixReader(header, stream) if stream else None
                    output.addfile(member, reader)
                    retained.append(dict(path=member.name, kind=member.type.decode('ascii'),
                                         size=member.size, mode=member.mode,
                                         link=member.linkname,
                                         sha256=reader.hash.hexdigest() if reader else None))
                if stream:
                    stream.close()
    except BaseException:
        if output:
            output.close()
            temporary.unlink()
        raise
    finally:
        if output:
            output.close()
    if not removed:
        if apply:
            temporary.unlink()
        return None
    record = dict(path=str(path), original_sha256=before, removed=removed,
                  original_bytes=path.stat().st_size)
    if apply:
        with tarfile.open(temporary, 'r:*') as archive:
            members = archive.getmembers()
            if len(members) != len(retained):
                raise RuntimeError('Retained archive member count changed')
            for member, row in zip(members, retained):
                if (member.name, member.type.decode('ascii'), member.size, member.mode, member.linkname) != (
                        row['path'], row['kind'], row['size'], row['mode'], row['link']):
                    raise RuntimeError('Retained archive metadata changed')
                if member.isfile():
                    with archive.extractfile(member) as stream:
                        if hashlib.file_digest(stream, 'sha256').hexdigest() != row['sha256']:
                            raise RuntimeError('Retained source/log/capture bytes changed')
        if digest(path) != before:
            raise RuntimeError('Original archive changed during pruning')
        record.update(retained=retained, replacement_sha256=digest(temporary),
                      replacement_bytes=temporary.stat().st_size)
        journal = path.with_name(path.name + '.retention.json')
        journal.write_text(json.dumps(record, indent=2) + '\n')
        temporary.replace(path)
    return record


def automatic_keeps(cache):
    """One active SQL stage, the last delivery, and two useful native builds."""
    result, stages, deliveries, native = [], [], [], []
    for stage in cache.glob('migrations-race-nhf*-v*'):
        match = re.fullmatch(r'migrations-race-nhf(\d+)-v(\d+)', stage.name)
        path = stage / 'manifest.json'
        if not match or not path.is_file():
            continue
        try:
            data = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(data, dict):
            continue
        artifacts = data.get('artifacts', {})
        if (len(artifacts) == 4 and all(Path(row[field]).is_file()
                                      for row in artifacts.values() for field in ('test', 'homed'))):
            stages.append(((int(match[1]), int(match[2])), stage))
    if stages:
        result.append(max(stages)[1])
    for path in cache.glob('nhf*/delivery.json'):
        match = re.fullmatch(r'nhf(\d+)', path.parent.name)
        try:
            data = json.loads(path.read_text())
            state = DeliveryState(data.get('status'))
        except (OSError, json.JSONDecodeError, ValueError, AttributeError):
            continue
        if match and state is DeliveryState.DELIVERED and data.get('fresh_sql_manifest'):
            manifest = Path(data['fresh_sql_manifest']).resolve()
            if not manifest.is_relative_to(cache) or not manifest.is_file():
                raise RuntimeError('Preserve the last delivered SQL manifest')
            deliveries.append((int(match[1]), manifest.parent))
    if deliveries:
        result.append(max(deliveries)[1])
    for directory, dirs, names in os.walk(cache, followlinks=False):
        dirs[:] = [d for d in dirs if d not in SOURCE_DIRS | {'homebrew'}
                   and not (Path(directory) / d).is_symlink()]
        if 'build.json' not in names or 'result.json' not in names:
            continue
        proof = Path(directory)
        try:
            build = json.loads((proof / 'build.json').read_text())
            report = json.loads((proof / 'result.json').read_text())
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(build, dict) or not isinstance(report, dict):
            continue
        cases = report.get('cases', [])
        if (all(key in build for key in ('binary', 'server', 'helper', 'binaries'))
                and isinstance(cases, list) and cases and all(isinstance(row, dict) for row in cases)
                and all(row.get('native_rendering') is True and row.get('children_joined') is True for row in cases)
                and Path(build['binary']).is_file()):
            references = [Path(build[key]).resolve() for key in ('binary', 'server', 'helper')]
            native.append(((proof / 'build.json').stat().st_mtime_ns, proof, references))
    for _, proof, references in sorted(native, reverse=True)[:2]:
        result.append(proof)
        result.extend(path for path in references if path.is_relative_to(cache))
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--keep', action='append', default=[])
    parser.add_argument('--keep-recent-hours', type=float, default=1)
    args = parser.parse_args(argv)
    if not 0 <= args.keep_recent_hours <= 168:
        parser.error('--keep-recent-hours must be between 0 and 168')
    config = json.loads((ROOT / 'frontend.local.json').read_text())
    cache = Path(config['frontend_cache']).resolve()
    baseline = json.loads(Path(config['baseline']).read_text())
    baseline_inodes = {(row[0], row[1]) for row in baseline.values()}
    keeps = [cache / name for name in ('homebrew', 'android-avd', 'android-user',
                                     'session-spike/swift-modules')]
    anchors = automatic_keeps(cache)
    if args.apply and not args.keep and not anchors:
        parser.error('--apply requires an active/delivered proof or explicit --keep directories')
    keeps.extend(anchors)
    for value in args.keep:
        path = Path(value)
        path = (path if path.is_absolute() else cache / path).resolve()
        if not path.is_relative_to(cache) or not path.exists():
            parser.error('--keep must identify an existing directory inside frontend_cache')
        keeps.append(path)
    if not os.environ.get('GOCACHE'):
        parser.error('Use frontend.py run --reserve-gb 0 (four exclusive leases)')
    if any(Path(p.strip()).name in {'cargo', 'rustc', 'go'} for p in
           subprocess.check_output(['ps', '-axo', 'comm'], text=True).splitlines()[1:]):
        raise RuntimeError('Builders must stop before cache pruning')
    candidates, archives = [], []
    protected_inodes = set(baseline_inodes)
    for keep in keeps:
        if keep.is_dir():
            for directory, dirs, names in os.walk(keep, followlinks=False):
                dirs[:] = [d for d in dirs if not (Path(directory) / d).is_symlink()]
                for name in names:
                    info = (Path(directory) / name).lstat()
                    protected_inodes.add((info.st_dev, info.st_ino))
    for directory, dirs, names in os.walk(cache, followlinks=False):
        dirs[:] = [d for d in dirs if not (Path(directory) / d).is_symlink()
                   and not protected(Path(directory) / d, cache, keeps)]
        for name in names:
            path = Path(directory) / name
            info = path.lstat()
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid()
                    or (info.st_dev, info.st_ino) in protected_inodes
                    or protected(path, cache, keeps)
                    or (args.keep_recent_hours and max(info.st_mtime_ns, info.st_ctime_ns)
                        >= time.time_ns() - args.keep_recent_hours * 3600 * 1_000_000_000)):
                continue
            if name.endswith(('.tar.gz', '.tgz', '.tar.xz')):
                archives.append(path)
            else:
                with path.open('rb') as stream:
                    header = stream.read(8)
                if compiled(str(path.relative_to(cache)), info.st_size, header):
                    candidates.append((path, info, digest(path)))
    for offset in range(0, len(candidates), 150):
        opened = subprocess.run(['lsof', '-t', '--',
                                 *[str(p) for p, _, _ in candidates[offset:offset + 150]]],
                                capture_output=True, timeout=30)
        if opened.returncode != 1 or opened.stdout or opened.stderr:
            raise RuntimeError('A proof binary is open or its closure is unverified')
    proof = cache / 'maintenance' / ('retention-' + str(time.time_ns()))
    proof.mkdir(parents=True)
    record = dict(apply=args.apply, protected=[str(p) for p in keeps],
                  baseline_unchanged=True,
                  raw=[dict(path=str(p), sha256=sha, size=info.st_size)
                       for p, info, sha in candidates], archives=[], skipped=[])
    receipt = proof / 'result.json'
    receipt.write_text(json.dumps(record, indent=2) + '\n')
    for path, info, sha in candidates:
        current = path.lstat()
        # Reading a file can update atime; unlinking another owned alias can
        # update ctime/nlink. Identity, payload, mode and ownership must remain.
        fields = ('st_dev', 'st_ino', 'st_size', 'st_mtime_ns', 'st_mode', 'st_uid')
        if any(getattr(current, key) != getattr(info, key) for key in fields) or digest(path) != sha:
            raise RuntimeError('Candidate changed before pruning')
        if args.apply:
            path.unlink()
    receipt.write_text(json.dumps(record, indent=2) + '\n')
    print(json.dumps(dict(raw_payloads=len(candidates), archives_to_inspect=len(archives))), flush=True)
    for path in archives:
        opened = subprocess.run(['lsof', '-t', '--', str(path)], capture_output=True, timeout=15)
        if opened.returncode != 1 or opened.stdout or opened.stderr:
            raise RuntimeError('Archive is open or its closure is unverified')
        try:
            row = prune_archive(path, args.apply)
        except (tarfile.TarError, EOFError, OSError) as error:
            record['skipped'].append(dict(path=str(path), error=str(error), preserved=True))
            receipt.write_text(json.dumps(record, indent=2) + '\n')
            print(json.dumps(dict(archive=str(path), preserved_unreadable=True)), flush=True)
            continue
        if row:
            record['archives'].append(row)
            receipt.write_text(json.dumps(record, indent=2) + '\n')
            print(json.dumps(dict(archive=path.name, removed_payloads=len(row['removed']),
                                  before=row['original_bytes'],
                                  after=row.get('replacement_bytes'))), flush=True)
    record['status'] = 'PRUNED' if args.apply else 'AUDITED'
    receipt.write_text(json.dumps(record, indent=2) + '\n')
    print(json.dumps(dict(status=record['status'], receipt=str(receipt),
                          raw_payloads=len(record['raw']), archives=len(record['archives']))), flush=True)


if __name__ == '__main__':
    main()
