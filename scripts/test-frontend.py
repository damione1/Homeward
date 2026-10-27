#!/usr/bin/env python3
"""Exercise refusal boundaries without allocating a disk or starting a build."""
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('frontend', Path(__file__).with_name('frontend.py'))
frontend = importlib.util.module_from_spec(spec)
spec.loader.exec_module(frontend)


class StorageGuardTests(unittest.TestCase):
    def test_budget_stop_joins_a_child_that_exited_before_the_signal(self):
        process = Mock(pid=123)
        process.poll.side_effect = [None, 0]
        with patch.object(frontend.os, 'killpg', side_effect=PermissionError):
            frontend.stop_owned_process(process)
        process.wait.assert_called_once_with(timeout=10)

    def test_budget_stop_does_not_hide_a_live_child_signal_failure(self):
        process = Mock(pid=123)
        process.poll.return_value = None
        with patch.object(frontend.os, 'killpg', side_effect=PermissionError):
            with self.assertRaises(PermissionError):
                frontend.stop_owned_process(process)

    def test_combined_growth_and_free_space_reserve(self):
        self.assertTrue(frontend.policy(frontend.LIMIT - 100, frontend.MIN_FREE + 100, 100))
        self.assertFalse(frontend.policy(frontend.LIMIT - 99, frontend.MIN_FREE + 100, 100))
        self.assertFalse(frontend.policy(0, frontend.MIN_FREE + 99, 100))
        self.assertFalse(frontend.policy(0, frontend.MIN_FREE, -1))

    def test_linker_growth_is_refused_before_the_hard_limit(self):
        used = frontend.LIMIT - 1_000_000_000
        free = frontend.MIN_FREE + 5_000_000_000
        self.assertTrue(frontend.policy(used, free, 512 * 1024**2))
        self.assertFalse(frontend.policy(used, free, frontend.RUNNING_RESERVE_BYTES))

    def test_unreadable_subtree_refuses_measurement(self):
        def denied(_root, **options):
            options['onerror'](PermissionError('unreadable cache subtree'))
        with tempfile.TemporaryDirectory() as temp:
            with patch.object(frontend.os, 'walk', side_effect=denied):
                with self.assertRaises(PermissionError):
                    frontend.inventory([Path(temp)])

    def test_retired_descendant_can_disappear_during_walk(self):
        def retired(root, **options):
            options['onerror'](FileNotFoundError(2, 'retired', str(root / 'retired')))
            return iter([])
        with tempfile.TemporaryDirectory() as temp:
            with patch.object(frontend.os, 'walk', side_effect=retired):
                self.assertEqual(frontend.inventory([Path(temp)]), {})

    def test_missing_root_is_not_a_retired_descendant(self):
        def missing(root, **options):
            options['onerror'](FileNotFoundError(2, 'missing root', str(root)))
        with tempfile.TemporaryDirectory() as temp:
            with patch.object(frontend.os, 'walk', side_effect=missing):
                with self.assertRaises(FileNotFoundError):
                    frontend.inventory([Path(temp)])

    def test_reuse_hardlinks_and_external_symlinks(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            shared = root / 'shared'
            shared.mkdir()
            payload = shared / 'payload'
            payload.write_bytes(b'x' * 8192)
            (shared / 'alias').hardlink_to(payload)
            external = root / 'external'
            external.mkdir()
            (external / 'outside').write_bytes(b'x' * 16384)
            (shared / 'link').symlink_to(external, target_is_directory=True)
            rows = frontend.inventory([shared, shared])
            self.assertEqual(len(rows), 1)
            self.assertEqual(sum(row[-1] for row in rows.values()), payload.stat().st_blocks * 512)


def load_tests(loader, tests, _pattern):
    spec = importlib.util.spec_from_file_location(
        'test_prune_frontend_cache', Path(__file__).with_name('test-prune-frontend-cache.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    tests.addTests(loader.loadTestsFromModule(module))
    return tests


if __name__ == '__main__':
    unittest.main()
