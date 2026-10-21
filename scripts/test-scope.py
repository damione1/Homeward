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
class EcobeeScopeTests(unittest.TestCase):
    def test_exact_separate_amendment(self):
        count,sources,digest=scope.validate_ecobee()
        self.assertEqual((count,sources),(851,39))
        self.assertNotEqual(digest,scope.validate())
    def test_reject_foreign_id_capability_alias_and_provenance(self):
        for mutation in [
            lambda a:a["additions"][0].update(entity_id="sensor.home_current_temperature"),
            lambda a:a["additions"].append({"entity_id":"climate.foreign","source":"homekit_controller"}),
            lambda a:a["aliases"].append("button.home_clear_hold"),
            lambda a:a["removals"].append("light.foreign"),
            lambda a:a["profile"]["fields"][0].update(hap_type="clear_hold"),
            lambda a:a["profile"]["fields"][0].update(required=1),
            lambda a:a["profile"]["fields"][0].update(unit="fahrenheit"),
            lambda a:a["profile"].update(thermostat_service="homekit_bridge"),
            lambda a:a["baseline"].update(manifest_sha256="0"*64),
            lambda a:a["baseline"].update(ids_sha256="0"*64),
            lambda a:a["qualification"].update(commit="0"*40),
            lambda a:a["decision"].update(sha256="0"*64),
            lambda a:a["decision"].update(line=52.0),
            lambda a:a["consumers"][0].update(entity_id="scene.foreign"),
            lambda a:a["consumers"][3].update(via=[]),
            lambda a:a.update(unapproved=True),
        ]:
            a=scope.load("scope/ecobee_mvp_v1.json");mutation(a)
            with self.subTest(amendment=a):
                with self.assertRaises(AssertionError):scope.validate_ecobee(a)
    def test_strict_duplicate_and_trailing_decoder(self):
        for raw in ['{"revision":"v1","revision":"v1"}','{} {}','{"field":NaN}']:
            with self.subTest(raw=raw):
                with self.assertRaises(AssertionError):scope.decode_ecobee(raw)

if __name__=="__main__":unittest.main()
