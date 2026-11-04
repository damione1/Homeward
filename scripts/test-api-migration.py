#!/usr/bin/env python3
"""Regression checks for removal accounting, with synthetic descriptors."""
import copy
import hashlib
import json
from pathlib import Path
import runpy
import tempfile
import unittest
from unittest import mock

checker = runpy.run_path(str(Path(__file__).with_name('check-api.py')))
# Small synthetic descriptors exercise row transitions separately from the
# production audit identity. The public gate always verifies both.
check = checker['_check_migration_rows']


class HistoricalInventory(unittest.TestCase):
    def setUp(self):
        root = Path(__file__).resolve().parents[1]
        self.manifest = json.loads((root / 'contracts/api_migration.json').read_text())

    def test_current_audit_accepts_evolving_destinations_and_states(self):
        for row in self.manifest['rows']:
            row.update(target='New resource destination', state='migrated-v2',
                       v2_implemented=True, replacement={'rpcs': ['home.api.v2.Service/Read']})
        checker['check_historical_inventory'](self.manifest)

    def test_removing_a_row_cannot_shrink_the_baseline_with_it(self):
        self.manifest['rows'].pop()
        self.manifest['baseline']['rpc_count'] -= 1
        with self.assertRaisesRegex(AssertionError, 'baseline changed'):
            checker['check_historical_inventory'](self.manifest)

    def test_public_method_cannot_be_rewritten_as_historically_private(self):
        row = next(row for row in self.manifest['rows'] if row['visibility'] == 'public')
        row.update(visibility='private', state='retained-private', migration_ticket=None)
        with self.assertRaisesRegex(AssertionError, 'audit identity changed'):
            checker['check_historical_inventory'](self.manifest)

    def test_original_rpc_source_and_owner_cannot_be_substituted(self):
        for key in ('rpc', 'source', 'owner'):
            with self.subTest(key=key):
                changed = copy.deepcopy(self.manifest)
                changed['rows'][0][key] += '-replacement'
                with self.assertRaisesRegex(AssertionError, 'audit identity changed'):
                    checker['check_historical_inventory'](changed)


class MigrationAccounting(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        (self.root / 'owner.go').write_text('func registerCurrent() {}\nfunc TestCurrent() {}')
        self.row = dict(rpc='home.old.v1.Service/Read', source='proto/old.proto',
                        owner='owner.go', visibility='public', migration_ticket='NHA-201',
                        target='Current resource', state='pending-replacement', v2_implemented=False)
        self.manifest = dict(baseline=dict(rpc_count=1), rows=[self.row])

    def descriptor(self, *pairs):
        return dict(file=[dict(package=package, name=package + '.proto',
                               service=[dict(name='Service', method=[dict(name='Read')])])
                          for package in pairs])

    def replacement(self, private=False):
        target = 'home.testing.fixture.v1' if private else 'home.api.v2'
        self.row.update(state='replaced-private-qualification' if private else 'migrated-v2',
                        v2_implemented=not private,
                        replacement=dict(rpcs=[target + '.Service/Read']))
        proof = [dict(file='owner.go', symbol='TestCurrent')]
        if private:
            self.row['replacement'].update(classification_reason='Only synthetic test registration existed.',
                                            qualification_tests=proof)
        else:
            self.row['replacement'].update(registrations=[dict(file='owner.go', symbol='registerCurrent')],
                                            tests=proof)
        return target

    def test_pending_requires_old_method(self):
        with self.assertRaisesRegex(AssertionError, 'unexpectedly removed'):
            check(self.manifest, self.descriptor('home.api.v2'), self.root)

    def test_replacement_accepts_removed_old_source_owner(self):
        target = self.replacement()
        self.row['owner'] = 'removed-owner.go'
        check(self.manifest, self.descriptor(target), self.root)

    def test_replacement_rejects_compatibility_route(self):
        target = self.replacement()
        with self.assertRaisesRegex(AssertionError, 'compatibility'):
            check(self.manifest, self.descriptor(target, 'home.old.v1'), self.root)

    def test_descriptor_alone_is_insufficient(self):
        target = self.replacement()
        del self.row['replacement']['registrations']
        with self.assertRaisesRegex(AssertionError, 'evidence'):
            check(self.manifest, self.descriptor(target), self.root)

    def test_private_reclassification_preserves_historical_public_visibility(self):
        target = self.replacement(private=True)
        check(self.manifest, self.descriptor(target), self.root)
        self.assertEqual(self.row['visibility'], 'public')
        self.assertFalse(self.row['v2_implemented'])
        changed = copy.deepcopy(self.manifest)
        changed['rows'][0]['visibility'] = 'private'
        with self.assertRaises(AssertionError):
            check(changed, self.descriptor(target), self.root)

    def test_new_private_route_requires_accounting(self):
        target = self.replacement(private=True)
        with self.assertRaisesRegex(AssertionError, 'unaccounted'):
            check(self.manifest, self.descriptor(target, 'home.testing.unmapped.v1'), self.root)


    def operator_replacement(self):
        self.row.update(state='replaced-private-operator', v2_implemented=False,
                        replacement=dict(rpcs=['home.operator.v1.Service/Read'],
                                         classification_reason='Existing native owner on protected Unix only.',
                                         transport='unix-same-uid',
                                         registrations=[dict(file='owner.go', symbol='registerCurrent')],
                                         transport_evidence=[dict(file='owner.go', symbol='registerCurrent')],
                                         tests=[dict(file='owner.go', symbol='TestCurrent')]))

    def test_operator_preserves_public_history_without_claiming_v2(self):
        self.operator_replacement()
        check(self.manifest, self.descriptor('home.operator.v1'), self.root)
        self.assertEqual(self.row['visibility'], 'public')
        self.assertFalse(self.row['v2_implemented'])
        with self.assertRaisesRegex(AssertionError, 'compatibility'):
            check(self.manifest, self.descriptor('home.operator.v1', 'home.old.v1'), self.root)

    def test_operator_rejects_public_destination_and_missing_unix_boundary(self):
        self.operator_replacement()
        self.row['replacement']['rpcs'] = ['home.api.v2.Service/Read']
        with self.assertRaisesRegex(AssertionError, 'remain private'):
            check(self.manifest, self.descriptor('home.api.v2'), self.root)
        self.operator_replacement()
        del self.row['replacement']['transport']
        with self.assertRaisesRegex(AssertionError, 'transport boundary'):
            check(self.manifest, self.descriptor('home.operator.v1'), self.root)

    def withdrawal(self):
        archived = b'func registeredReadOnly() {}'
        self.row.update(state='withdrawn-unimplemented-declaration', v2_implemented=False,
                        replacement=dict(classification_reason='Declaration had no registered mutation.',
                                         historical_sources=[dict(file='old.go', revision='a' * 40,
                                             sha256=hashlib.sha256(archived).hexdigest(),
                                             required_symbols=['registeredReadOnly'], absent_symbols=['declaredMutation'])],
                                         workflow=dict(rpcs=['home.api.v2.Service/Read'],
                                             relationship='Related configuration workflow; no equivalent CRUD claim.',
                                             registrations=[dict(file='owner.go', symbol='registerCurrent')],
                                             source_evidence=[dict(file='owner.go', symbol='TestCurrent')])))
        return archived

    def test_withdrawal_requires_verified_archive_and_distinct_workflow(self):
        archived = self.withdrawal()
        with mock.patch.object(checker['subprocess'], 'check_output', return_value=archived):
            check(self.manifest, self.descriptor('home.api.v2'), self.root)
            self.row['replacement']['rpcs'] = ['home.api.v2.Service/Read']
            with self.assertRaisesRegex(AssertionError, 'must not claim equivalent'):
                check(self.manifest, self.descriptor('home.api.v2'), self.root)

    def test_withdrawal_rejects_changed_archive_and_implemented_mutation(self):
        archived = self.withdrawal()
        with mock.patch.object(checker['subprocess'], 'check_output', return_value=archived + b' changed'):
            with self.assertRaisesRegex(AssertionError, 'digest changed'):
                check(self.manifest, self.descriptor('home.api.v2'), self.root)
        self.row['replacement']['historical_sources'][0]['absent_symbols'] = ['registeredReadOnly']
        with mock.patch.object(checker['subprocess'], 'check_output', return_value=archived):
            with self.assertRaisesRegex(AssertionError, 'had implementation'):
                check(self.manifest, self.descriptor('home.api.v2'), self.root)

    def test_withdrawal_cannot_retain_old_rpc_or_claim_v2_implementation(self):
        self.withdrawal()
        with self.assertRaisesRegex(AssertionError, 'compatibility'):
            check(self.manifest, self.descriptor('home.api.v2', 'home.old.v1'), self.root)
        self.row['v2_implemented'] = True
        with self.assertRaisesRegex(AssertionError, 'schema-only'):
            check(self.manifest, self.descriptor('home.api.v2'), self.root)


def load_tests(loader, tests, pattern):
    # Keep the existing release gate command authoritative for both accounting
    # and the public lint profile's negative checks.
    lint = runpy.run_path(str(Path(__file__).with_name('test-api-lint.py')))
    tests.addTests(loader.loadTestsFromTestCase(lint['LintProfile']))
    return tests


if __name__ == '__main__':
    unittest.main()
