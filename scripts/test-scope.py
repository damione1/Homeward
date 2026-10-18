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
    def test_logic_mvp_exact_sources_and_partition(self):
        self.assertEqual(scope.validate_logic_mvp(),(17,72))
    def test_logic_mvp_missing_duplicate_overlap_unknown(self):
        for mutation,diagnostic in [
            (lambda a:a["deferred"].pop(),"missing ID"),
            (lambda a:a["deferred"].append(copy.deepcopy(a["deferred"][0])),"duplicate ID"),
            (lambda a:a["deferred"].append({"entity_id":a["retained"][0]["entity_id"],"active":False}),"overlap"),
            (lambda a:a["deferred"][0].update(entity_id="scene.foreign"),"unknown ID"),
        ]:
            with self.subTest(diagnostic=diagnostic):
                amendment=scope.load("scope/logic_mvp_v1.json");mutation(amendment)
                with self.assertRaisesRegex(AssertionError,diagnostic):scope.validate_logic_mvp(amendment)
    def test_logic_mvp_provenance_and_activation(self):
        for mutation,diagnostic in [
            (lambda a:a["retained"][0]["definition"].update(sha256="0"*64),"definition digest"),
            (lambda a:a["source_documents"][0].update(sha256="0"*64),"source document digest"),
            (lambda a:a["retained"][0].update(active=True),"activation"),
            (lambda a:a["deferred"][0].pop("active"),"deferred shape"),
        ]:
            with self.subTest(diagnostic=diagnostic):
                amendment=scope.load("scope/logic_mvp_v1.json");mutation(amendment)
                with self.assertRaisesRegex(AssertionError,diagnostic):scope.validate_logic_mvp(amendment)
if __name__=="__main__":unittest.main()
