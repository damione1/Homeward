#!/usr/bin/env python3
"""Exercise refusal boundaries without allocating a disk or starting a build."""
import contextlib
import json
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('frontend', Path(__file__).with_name('frontend.py'))
frontend = importlib.util.module_from_spec(spec)
spec.loader.exec_module(frontend)


class StorageGuardTests(unittest.TestCase):
    def test_debug_signing_identity_survives_cache_changes_and_explicit_override_wins(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            key = root / 'stable.keystore'
            override = root / 'explicit.keystore'
            key.write_bytes(b'synthetic key placeholder')
            override.write_bytes(b'synthetic override placeholder')
            config = {'backend_cache': str(root / 'backend'),
                      'frontend_cache': str(root / 'cache-a'),
                      'android_sdk': str(root / 'sdk'), 'android_debug_keystore': str(key)}
            with patch.dict(frontend.os.environ, {'PATH': '/synthetic/bin'}, clear=True):
                first = frontend.environment(config)
                config['frontend_cache'] = str(root / 'cache-b')
                second = frontend.environment(config)
                self.assertNotEqual(first['ANDROID_USER_HOME'], second['ANDROID_USER_HOME'])
                self.assertEqual(first['CARGO_APK_DEV_KEYSTORE'], str(key))
                self.assertEqual(second['CARGO_APK_DEV_KEYSTORE'], str(key))
                with patch.dict(frontend.os.environ, {'CARGO_APK_DEV_KEYSTORE': str(override)}):
                    self.assertEqual(frontend.environment(config)['CARGO_APK_DEV_KEYSTORE'], str(override))
                key.unlink()
                with self.assertRaisesRegex(SystemExit, 'existing absolute file'):
                    frontend.environment(config)
                self.assertFalse(key.exists(), 'a missing identity must never be regenerated')

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

    def test_advisory_target_keeps_the_host_free_space_reserve_enforced(self):
        self.assertTrue(frontend.policy(frontend.LIMIT - 100, frontend.MIN_FREE + 100, 100))
        self.assertTrue(frontend.policy(frontend.LIMIT + 1, frontend.MIN_FREE + 100, 100))
        self.assertFalse(frontend.policy(0, frontend.MIN_FREE + 99, 100))
        self.assertFalse(frontend.policy(0, frontend.MIN_FREE, -1))

    def test_linker_reservation_may_exceed_the_target_but_cannot_exhaust_host_space(self):
        used = frontend.LIMIT - 1_000_000_000
        free = frontend.MIN_FREE + 5_000_000_000
        self.assertTrue(frontend.policy(used, free, 512 * 1024**2))
        self.assertTrue(frontend.policy(used, free, frontend.RUNNING_RESERVE_BYTES))
        self.assertFalse(frontend.policy(used, frontend.MIN_FREE + 1_000_000_000, frontend.RUNNING_RESERVE_BYTES))

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

    def test_android_retirement_checks_open_files_before_any_package_clean(self):
        for opened in [False, True]:
            with self.subTest(opened=opened), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                target = root / 'target'
                (target / 'debug/deps').mkdir(parents=True)
                (target / 'aarch64-linux-android/debug/deps').mkdir(parents=True)
                baseline = root / 'baseline.json'
                baseline.write_text('{}')
                config = {'frontend_cache': str(root), 'baseline': str(baseline)}
                calls = []
                def run(argv, **_kwargs):
                    calls.append(argv)
                    if argv[0] == 'lsof':
                        busy = opened and 'aarch64-linux-android' in argv[-1]
                        return Mock(returncode=0 if busy else 1, stdout=b'123' if busy else b'')
                    return Mock(returncode=0)
                with patch.object(frontend, 'leases', return_value=contextlib.nullcontext()), \
                     patch.object(frontend, 'measure', return_value={'within_policy': True}), \
                     patch.object(frontend, 'sources', return_value='synthetic'), \
                     patch.object(frontend, 'environment', return_value={'CARGO_TARGET_DIR': str(target)}), \
                     patch.object(frontend.subprocess, 'check_output', return_value='COMMAND\n'), \
                     patch.object(frontend.subprocess, 'run', side_effect=run):
                    if opened:
                        with self.assertRaises(SystemExit):
                            frontend.retire_evaluations(config, frontend.EvaluationPackages.APP)
                        self.assertFalse(any(call[0] == 'cargo' for call in calls))
                    else:
                        self.assertEqual(frontend.retire_evaluations(config, frontend.EvaluationPackages.APP), 0)
                        clean = [call for call in calls if call[0] == 'cargo']
                        self.assertEqual(clean, [
                            ['cargo', 'clean', '-p', 'homeward-app'],
                            ['cargo', 'clean', '-p', 'homeward-app', '--target', 'aarch64-linux-android'],
                        ])
                        result = json.loads(next(root.glob('rounds/*/result.json')).read_text())
                        self.assertTrue(result['only_owned_evaluation_packages'])
                        self.assertEqual(len(result['commands']), 2)

    def test_pro_retirement_inventories_consumer_before_scoped_clean(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            target = root / 'target'
            deps = target / 'debug/deps'
            deps.mkdir(parents=True)
            consumer = deps / 'slint_kit_pro_consumer_tests-synthetic'
            consumer.write_bytes(b'evaluation')
            runtime = deps / 'libslint_kit_pro-synthetic.rlib'
            runtime.write_bytes(b'runtime')
            baseline = root / 'baseline.json'
            baseline.write_text('{}')
            config = {'frontend_cache': str(root), 'baseline': str(baseline),
                      'pro_kit': str(root / 'pro')}
            calls = []
            def run(argv, **_kwargs):
                calls.append(argv)
                if argv[0] == 'lsof':
                    return Mock(returncode=1, stdout=b'')
                receipt = json.loads(next(root.glob('rounds/*/evaluation-artifacts-before.json')).read_text())
                self.assertEqual(set(receipt['artifacts']), {str(consumer)})
                self.assertIn('slint-kit-pro-consumer-tests', receipt['packages'])
                self.assertEqual(receipt['allocated_bytes'], consumer.stat().st_blocks * 512)
                return Mock(returncode=0)
            with patch.object(frontend, 'leases', return_value=contextlib.nullcontext()), \
                 patch.object(frontend, 'measure', return_value={'within_policy': True}), \
                 patch.object(frontend, 'sources', return_value='synthetic'), \
                 patch.object(frontend, 'environment', return_value={'CARGO_TARGET_DIR': str(target)}), \
                 patch.object(frontend.subprocess, 'check_output', return_value='COMMAND\n'), \
                 patch.object(frontend.subprocess, 'run', side_effect=run):
                self.assertEqual(frontend.retire_evaluations(config, frontend.EvaluationPackages.PRO), 0)
            self.assertEqual([call for call in calls if call[0] == 'cargo'], [[
                'cargo', 'clean', '-p', 'slint-kit-pro-tests',
                '-p', 'slint-kit-pro-examples', '-p', 'slint-kit-pro-consumer-tests',
            ]])
            self.assertTrue(runtime.is_file())

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
    for module_name, filename in [('test_prune_frontend_cache', 'test-prune-frontend-cache.py'),
                                  ('test_frontend_delivery', 'test-frontend-delivery.py')]:
        spec = importlib.util.spec_from_file_location(module_name, Path(__file__).with_name(filename))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        tests.addTests(loader.loadTestsFromModule(module))
    return tests


if __name__ == '__main__':
    unittest.main()
