#!/usr/bin/env python3
"""Negative checks for the exact, fail-closed public API lint profile."""
import copy
from pathlib import Path
import runpy
import unittest

checker = runpy.run_path(str(Path(__file__).with_name('check-api.py')))


class LintProfile(unittest.TestCase):
    def setUp(self):
        self.file = 'home/api/v2/test.proto'
        self.field = dict(name='observed_at', number=1, type='TYPE_MESSAGE',
                          typeName='.google.protobuf.Timestamp')
        self.descriptor = dict(file=[dict(name=self.file, package='home.api.v2',
            messageType=[dict(name='Observation', field=[self.field])],
            sourceCodeInfo=dict(location=[dict(path=[4, 0], span=[1, 0, 5, 1]),
                dict(path=[4, 0, 2, 0], span=[3, 2, 40])]))])
        self.problem = dict(rule_id='core::0142::time-field-names', message='Use _time.',
            location=dict(start_position=dict(line_number=4, column_number=12)))
        self.report = [dict(file_path=self.file, problems=[self.problem])]
        key, digest = checker['lint_identity'](self.file, self.problem,
                                              checker['lint_symbols'](self.descriptor))
        self.entry = dict(file=key[0], rule=key[1], symbol=key[2],
            declaration_sha256=digest, classification='advisory',
            reason='The observation boundary uses the domain term observed_at.',
            reference='https://google.aip.dev/142')
        self.registry = dict(version=1, linter_version=checker['VERSION'], exceptions=[self.entry])

    def check(self):
        return checker['check_lint_profile'](self.report, self.descriptor, self.registry)

    def test_exact_reviewed_declaration_accepted(self):
        self.assertEqual(self.check(), 1)

    def test_new_diagnostic_on_same_symbol_is_not_hidden(self):
        problem = copy.deepcopy(self.problem)
        problem['rule_id'] = 'core::0140::prepositions'
        self.report[0]['problems'].append(problem)
        with self.assertRaisesRegex(AssertionError, 'unaccepted'):
            self.check()

    def test_declaration_type_number_and_behavior_changes_need_review(self):
        for changes in [dict(typeName='.google.protobuf.Duration'), dict(number=2),
                        dict(options={'[google.api.field_behavior]': ['REQUIRED']})]:
            with self.subTest(changes=changes):
                before = copy.deepcopy(self.field)
                self.field.update(changes)
                with self.assertRaisesRegex(AssertionError, 'unaccepted'):
                    self.check()
                self.field.clear()
                self.field.update(before)

    def test_same_rule_on_new_symbol_is_not_hidden(self):
        self.field['name'] = 'captured_at'
        with self.assertRaisesRegex(AssertionError, 'unaccepted'):
            self.check()

    def test_mandatory_rules_cannot_be_whitelisted(self):
        for rule in ['core::0140::numbers', 'core::0141::forbidden-types',
                     'core::0123::pattern-singular', 'core::0133::method-signature',
                     'client-libraries::4232::required-fields']:
            with self.subTest(rule=rule):
                self.entry['rule'] = self.problem['rule_id'] = rule
                with self.assertRaisesRegex(AssertionError, 'mandatory/unreviewed'):
                    self.check()

    def test_fixed_diagnostic_requires_removing_stale_exception(self):
        self.report[0]['problems'] = []
        with self.assertRaisesRegex(AssertionError, 'stale'):
            self.check()

    def test_mixed_rules_cannot_exempt_another_get_or_management_create(self):
        for rule in ['core::0131::synonyms', 'core::0133::request-id-field']:
            with self.subTest(rule=rule):
                self.entry['rule'] = self.problem['rule_id'] = rule
                with self.assertRaisesRegex(AssertionError, 'mandatory/unreviewed subject'):
                    self.check()

    def test_known_linter_duplicate_is_counted_exactly(self):
        self.entry['occurrences'] = 2
        self.report[0]['problems'].append(copy.deepcopy(self.problem))
        self.assertEqual(self.check(), 1)
        self.report[0]['problems'].pop()
        with self.assertRaisesRegex(AssertionError, 'stale diagnostic count'):
            self.check()

    def test_duplicate_exception_and_diagnostic_rejected(self):
        self.registry['exceptions'].append(copy.deepcopy(self.entry))
        with self.assertRaisesRegex(AssertionError, 'duplicate lint exception'):
            self.check()
        self.registry['exceptions'].pop()
        self.report[0]['problems'].append(copy.deepcopy(self.problem))
        with self.assertRaisesRegex(AssertionError, 'duplicate lint diagnostic'):
            self.check()

    def test_missing_primary_reason_rejected(self):
        self.entry['reason'] = ''
        with self.assertRaisesRegex(AssertionError, 'justification'):
            self.check()

    def test_line_movement_does_not_change_declaration_identity(self):
        for location in self.descriptor['file'][0]['sourceCodeInfo']['location']:
            location['span'][0] += 10
            if len(location['span']) == 4:
                location['span'][2] += 10
        self.problem['location']['start_position']['line_number'] += 10
        self.assertEqual(self.check(), 1)

    def test_unknown_location_fails_closed(self):
        self.problem['location']['start_position']['line_number'] = 100
        with self.assertRaisesRegex(AssertionError, 'unresolved'):
            self.check()


if __name__ == '__main__':
    unittest.main()
