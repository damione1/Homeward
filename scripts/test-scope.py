#!/usr/bin/env python3
import copy, importlib.util, pathlib, unittest
spec=importlib.util.spec_from_file_location("scope",pathlib.Path(__file__).with_name("check-scope.py"));scope=importlib.util.module_from_spec(spec);spec.loader.exec_module(scope)
class ScopeTests(unittest.TestCase):
    def test_exact_frozen_scope(self):scope.validate()
    def test_excluded_candidate_rejected_with_reason(self):
        allowed,reason=scope.admit("ai_task.ollama_ai_task")
        self.assertFalse(allowed);self.assertIn("outside frozen allowlist",reason)
        m=scope.load("scope/manifest.json");m["entities"][0]["entity_id"]="ai_task.ollama_ai_task"
        with self.assertRaisesRegex(AssertionError,"scope difference"):scope.validate(m)
    def test_duplicates_rejected(self):
        m=scope.load("scope/manifest.json");m["entities"].append(copy.deepcopy(m["entities"][0]))
        with self.assertRaises(AssertionError):scope.validate(m)
    def test_diagnostic_not_hidden_by_driver_status(self):
        m=scope.load("scope/manifest.json");next(e for e in m["entities"] if e["diagnostic_open"])["diagnostic_open"]=False
        with self.assertRaisesRegex(AssertionError,"diagnostic"):scope.validate(m)
    def test_logic_activation_rejected(self):
        l=scope.load("scope/logic.json");l["objects"][0]["active"]=True
        with self.assertRaisesRegex(AssertionError,"activated"):scope.validate(logic=l)
if __name__=="__main__":unittest.main()
