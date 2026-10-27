#!/usr/bin/env python3
"""Retention safety: old build payloads can go; source and active proofs cannot."""
import importlib.util
import io
import json
import lzma
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('prune', Path(__file__).with_name('prune-frontend-cache.py'))
prune = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prune)


class RetentionTests(unittest.TestCase):
    def test_unversioned_active_stage_and_last_delivery_survive_actual_pruning(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cache = root / 'cache'
            payload = b'\x7fELF' + b'x' * 1_000_000
            stages = {}
            for name in ('migrations-race-nhf013-v3', 'migrations-race-nhf016', 'migrations-race-nhf017'):
                stage = cache / name
                artifacts = {}
                for role in ('baseline', 'additive', 'destructive', 'unknown-kind'):
                    (stage / role).mkdir(parents=True)
                    test, homed = stage / role / 'server.test', stage / role / 'homed'
                    test.write_bytes(payload)
                    homed.write_bytes(payload)
                    artifacts[role] = dict(test=str(test), homed=str(homed))
                (stage / 'manifest.json').write_text(json.dumps(dict(artifacts=artifacts)))
                stages[name] = stage
            delivery = cache / 'nhf016' / 'delivery.json'
            delivery.parent.mkdir()
            delivery.write_text(json.dumps(dict(status='DELIVERED',
                fresh_sql_manifest=str(stages['migrations-race-nhf016'] / 'manifest.json'))))
            baseline = cache / 'storage-baseline.json'
            baseline.write_text('{}')
            (root / 'frontend.local.json').write_text(json.dumps(dict(
                frontend_cache=str(cache), baseline=str(baseline))))
            with patch.object(prune, 'ROOT', root), patch.dict(os.environ, GOCACHE=str(root)), \
                    patch.object(prune.subprocess, 'check_output', return_value=''), \
                    patch.object(prune.subprocess, 'run', return_value=Mock(returncode=1, stdout=b'', stderr=b'')):
                prune.main(['--apply', '--keep-recent-hours', '0'])
            for name in ('migrations-race-nhf016', 'migrations-race-nhf017'):
                for role in ('baseline', 'additive', 'destructive', 'unknown-kind'):
                    for binary in ('server.test', 'homed'):
                        self.assertEqual((stages[name] / role / binary).read_bytes(), payload)
            # An obsolete successor can be rebuilt; baseline and delivery source
            # evidence remain. The test must exercise both preservation and trim.
            self.assertFalse((stages['migrations-race-nhf013-v3'] / 'additive/homed').exists())

    def test_hardlink_pruning_preserves_source_active_and_baseline_aliases(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cache = root / 'cache'
            (cache / 'active').mkdir(parents=True)
            (cache / 'source').mkdir()
            payload = b'\x7fELF' + b'x' * 1_000_000
            first = cache / 'old.test'
            first.write_bytes(payload)
            alias = cache / 'old-alias.test'
            alias.hardlink_to(first)
            active = cache / 'active/current.test'
            active.write_bytes(payload)
            source = cache / 'source/fixture.elf'
            source.write_bytes(payload)
            backend = root / 'backend.test'
            backend.write_bytes(payload)
            preserved = cache / 'baseline-alias.test'
            preserved.hardlink_to(backend)
            info = backend.stat()
            baseline = cache / 'storage-baseline.json'
            baseline.write_text(json.dumps({str(backend): [info.st_dev, info.st_ino,
                info.st_size, info.st_mtime_ns, info.st_blocks * 512]}))
            (root / 'frontend.local.json').write_text(json.dumps(dict(
                frontend_cache=str(cache), baseline=str(baseline))))
            with patch.object(prune, 'ROOT', root), patch.dict(os.environ, GOCACHE=str(root)), \
                    patch.object(prune.subprocess, 'check_output', return_value=''), \
                    patch.object(prune.subprocess, 'run', return_value=Mock(returncode=1, stdout=b'', stderr=b'')):
                prune.main(['--apply', '--keep', 'active', '--keep-recent-hours', '0'])
            self.assertFalse(first.exists())
            self.assertFalse(alias.exists())
            for path in (source, active, preserved, backend):
                self.assertEqual(path.read_bytes(), payload)

    def test_unreadable_archive_preserves_original_and_removes_only_its_temporary(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'truncated.tar.xz'
            with tarfile.open(path, 'w:xz') as archive:
                payload = b'\x7fELF' + b'x' * 1_000_000
                member = tarfile.TarInfo('old/server.test')
                member.size = len(payload)
                archive.addfile(member, io.BytesIO(payload))
            encoded = path.read_bytes()
            path.write_bytes(encoded[:len(encoded) // 2])
            before = path.read_bytes()
            with self.assertRaises((EOFError, lzma.LZMAError, tarfile.ReadError)):
                prune.prune_archive(path, True)
            self.assertEqual(path.read_bytes(), before)
            self.assertFalse(path.with_name(path.name + '.retention-writing').exists())

    def test_source_and_active_sql_payloads_are_protected(self):
        cache = Path('/cache')
        self.assertFalse(prune.compiled('role/source/fixture.elf', 2_000_000, b'\x7fELF'))
        self.assertTrue(prune.compiled('old/server.test', 2_000_000, b'\x7fELF'))
        self.assertFalse(prune.compiled('old/screenshot.png', 2_000_000, b'\x89PNG'))
        self.assertTrue(prune.protected(cache / 'migrations-race-old/baseline/homed', cache, []))
        self.assertTrue(prune.protected(cache / 'current/server.test', cache, [cache / 'current']))
        self.assertFalse(prune.protected(cache / 'old/server.test', cache, [cache / 'current']))

    def test_archive_keeps_source_logs_links_and_capture_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'old.tar.gz'
            content = {'old/server.test': b'\x7fELF' + b'x' * 1_000_000,
                       'old/source/test-fixture.elf': b'\x7fELF' + b'y' * 1_000_000,
                       'old/result.json': b'{"pass":true}',
                       'old/screenshot.png': b'\x89PNG' + b'z' * 100}
            with tarfile.open(path, 'w:gz') as archive:
                for name, payload in content.items():
                    member = tarfile.TarInfo(name)
                    member.size = len(payload)
                    archive.addfile(member, io.BytesIO(payload))
                link = tarfile.TarInfo('old/server-alias.test')
                link.type, link.linkname = tarfile.LNKTYPE, 'old/server.test'
                archive.addfile(link)
            before = path.read_bytes()
            audit = prune.prune_archive(path, False)
            self.assertEqual(path.read_bytes(), before)
            self.assertEqual(len(audit['removed']), 2)
            record = prune.prune_archive(path, True)
            self.assertTrue(path.with_name(path.name + '.retention.json').exists())
            self.assertEqual(len(record['removed']), 2)
            with tarfile.open(path, 'r:gz') as archive:
                self.assertEqual(set(archive.getnames()), set(content) - {'old/server.test'})
                for member in archive:
                    self.assertEqual(archive.extractfile(member).read(), content[member.name])


if __name__ == '__main__':
    unittest.main()
