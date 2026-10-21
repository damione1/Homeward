#!/usr/bin/env python3
"""Verify the frozen allowlist against independently extracted documentary sets."""
import argparse, csv, hashlib, json, pathlib, re
ROOT = pathlib.Path(__file__).resolve().parents[1]
def load(name):return json.loads((ROOT / name).read_text())
def validate(manifest=None, logic=None):
    manifest=load("scope/manifest.json") if manifest is None else manifest
    logic=load("scope/logic.json") if logic is None else logic
    rows=list(csv.DictReader((ROOT/"scope/sources/selection.tsv").read_text().splitlines(),delimiter="\t"))
    coverage=list(csv.DictReader((ROOT/"scope/sources/coverage.tsv").read_text().splitlines(),delimiter="\t"))
    selected={r["entity_id"]:r["source"] for r in rows if r["decision"]=="garder"}
    covered={r["entity_id"]:r["source"] for r in coverage}
    entities=manifest["entities"];actual={e["entity_id"]:e["source"] for e in entities}
    assert len(rows)==len({r["entity_id"] for r in rows})==5990,"duplicate/missing selection"
    assert len(coverage)==len(covered)==850,"duplicate/missing coverage"
    assert len(entities)==len(actual)==850,"duplicate/missing manifest IDs"
    assert actual==selected==covered,f"scope difference: added={sorted(actual.keys()-selected.keys())}, missing={sorted(selected.keys()-actual.keys())}"
    assert len(set(actual.values()))==38,"source count"
    digest=hashlib.sha256(("\n".join(sorted(actual))+"\n").encode()).hexdigest()
    assert digest==manifest["ids_sha256"],"ID digest"
    diagnostics=set((ROOT/"scope/sources/diagnostics.txt").read_text().splitlines())
    assert len(diagnostics)==19 and diagnostics=={e["entity_id"] for e in entities if e["diagnostic_open"]},"diagnostic markers"
    assert all(type(e["diagnostic_open"]) is bool and e["driver_support"]=="unqualified" for e in entities),"diagnosis/support must stay independent"
    objects=logic["objects"];pending={r["entity_id"] for r in rows if r["decision"]=="a-decider"}
    assert len(objects)==len(pending)==89 and {o["entity_id"] for o in objects}==pending,"logic set"
    assert all(o["active"] is False and o["decision"]=="pending" for o in objects),"logic activated without business decision"
    assert not pending & actual.keys(),"logic leaked into allowlist"
    return digest

def validate_logic_mvp(amendment=None):
    baseline_bytes=(ROOT/"scope/logic.json").read_bytes()
    baseline=json.loads(baseline_bytes)
    manifest=load("scope/manifest.json")
    amendment=load("scope/logic_mvp_v1.json") if amendment is None else amendment
    assert set(amendment)=={"version","baseline_logic_sha256","manifest_ids_sha256",
                           "decision_document","lot_document","source_documents","retained","deferred",
                           "deferred_decision_ref"},"logic MVP shape"
    assert amendment["version"]==1,"logic MVP version"
    assert amendment["baseline_logic_sha256"]==hashlib.sha256(baseline_bytes).hexdigest(),"logic MVP baseline digest"
    assert amendment["manifest_ids_sha256"]==manifest["ids_sha256"],"logic MVP manifest digest"
    assert amendment["decision_document"]["file"]=="MVP serveur - décisions et questions.md","logic MVP decision source"
    assert re.fullmatch(r"[0-9a-f]{64}",amendment["decision_document"]["sha256"]),"logic MVP decision digest"
    assert amendment["lot_document"]["file"]=="MVP serveur - premier lot de logique proposé.md","logic MVP lot source"
    assert re.fullmatch(r"[0-9a-f]{64}",amendment["lot_document"]["sha256"]),"logic MVP lot digest"
    # The catalogue is already a product asset with the documentary source
    # fingerprints. This check does not need access to the Obsidian vault.
    catalogue=load("compat/ha/catalogue.json")
    catalogue_docs={d["file"]:d["sha256"] for d in catalogue["source_documents"]}
    docs={d["file"]:d["sha256"] for d in amendment["source_documents"]}
    assert len(amendment["source_documents"])==len(docs)==2,"logic MVP duplicate source document"
    assert set(docs)=={"Automatisations.md","Scripts et scènes.md"},"logic MVP source documents"
    assert all(docs[name]==catalogue_docs[name] for name in docs),"logic MVP source document digest"
    assert isinstance(amendment["deferred_decision_ref"],str) and amendment["deferred_decision_ref"],"logic MVP report decision"
    objects=baseline["objects"]
    known={o["entity_id"]:o["source"] for o in objects}
    assert len(objects)==len(known)==89,"logic MVP baseline IDs"
    assert {s:sum(o["source"]==s for o in objects) for s in ("automation","script","homeassistant")}=={
        "automation":50,"script":22,"homeassistant":17},"logic MVP baseline counts"
    assert all(o["entity_id"].startswith(("scene." if o["source"]=="homeassistant" else o["source"]+".")) for o in objects),"logic MVP baseline kind"
    retained=amendment["retained"];deferred=amendment["deferred"]
    assert all(set(item)=={"entity_id","active","decision_ref","definition"} for item in retained),"logic MVP retained shape"
    assert all(set(item)=={"entity_id","active"} for item in deferred),"logic MVP deferred shape"
    retained_ids=[item["entity_id"] for item in retained]
    deferred_ids=[item["entity_id"] for item in deferred]
    assert len(retained_ids)==len(set(retained_ids)) and len(deferred_ids)==len(set(deferred_ids)),"logic MVP duplicate ID"
    assert not set(retained_ids)&set(deferred_ids),"logic MVP overlap"
    assert set(retained_ids+deferred_ids)<=set(known),"logic MVP unknown ID"
    assert set(retained_ids+deferred_ids)==set(known),"logic MVP missing ID"
    assert len(retained)==17 and len(deferred)==72,"logic MVP count"
    assert sum(known[id]=="homeassistant" for id in retained_ids)==12 and sum(known[id]=="automation" for id in retained_ids)==5,"logic MVP retained kinds"
    assert all(item["active"] is False for item in retained+deferred),"logic MVP activation"
    assert all(o["active"] is False and o["decision"]=="pending" for o in objects),"logic MVP baseline decisions"
    source_paths=set()
    for item in retained:
        definition=item["definition"];alias=item["entity_id"];kind=known[alias]
        assert set(definition)=={"path","sha256","source_document","source_section"},"logic MVP definition shape"
        expected_doc="Automatisations.md" if kind=="automation" else "Scripts et scènes.md"
        expected_path=f"scope/logic_mvp_sources/{alias}.yaml"
        assert definition["source_document"]==expected_doc and definition["source_section"]==alias and definition["path"]==expected_path,"logic MVP provenance path"
        source_paths.add(expected_path)
        assert re.fullmatch(r"[0-9a-f]{64}",definition["sha256"]),"logic MVP definition digest format"
        assert hashlib.sha256((ROOT/expected_path).read_bytes()).hexdigest()==definition["sha256"],"logic MVP definition digest"
    assert source_paths=={p.relative_to(ROOT).as_posix() for p in (ROOT/"scope/logic_mvp_sources").glob("*.yaml")},"logic MVP source files"
    return len(retained),len(deferred)

def admit(entity_id):
    if entity_id in {e["entity_id"] for e in load("scope/manifest.json")["entities"]}:return True,"selected; driver qualification still required"
    return False,"outside frozen allowlist; explicit scope decision required"

def compare_vault(vault):
    root=pathlib.Path(vault)
    text=(root/"Sélection des entités.md").read_text()
    selected=set(re.findall(r"^\| garder \| ([^ |]+) \|",text,re.M))
    covered=set(re.findall(r"^\| `([^`]+)` \|",(root/"Architecture/Couverture des 850 entités.md").read_text(),re.M))
    frozen={e["entity_id"] for e in load("scope/manifest.json")["entities"]}
    assert selected==covered==frozen,"live vault set differs"
    for file,record in load("scope/sources/provenance.json").items():
        assert hashlib.sha256((root/file).read_bytes()).hexdigest()==record["sha256"],f"document changed: {file}"
# NHA-137 has a separate closed decoder and validator. The frozen validate()
# above remains unchanged; accepting Ecobee never widens its 850-ID oracle.
ECOBEE_SHA256="2780049835aa84e16ff6e17ee01c0dafdc6f0f838ff61273505f738886c65f1c"
BASELINE_SHA256="d2f15358239b29797906a31f9318d6ad7a8b81be76f9d39421b0aa6d22f52cfa"

def decode_ecobee(data):
    def members(pairs):
        result={}
        for key,value in pairs:
            assert key not in result,"Ecobee duplicate member"
            result[key]=value
        return result
    def constant(value):
        raise AssertionError("Ecobee invalid numeric constant")
    assert isinstance(data,(bytes,str)) and 0<len(data)<=64*1024,"Ecobee input size"
    try: return json.loads(data,object_pairs_hook=members,parse_constant=constant)
    except (ValueError,TypeError) as exc: raise AssertionError("Ecobee invalid JSON") from exc

def validate_ecobee(amendment=None):
    blob=(ROOT/"scope/ecobee_mvp_v1.json").read_bytes()
    assert hashlib.sha256(blob).hexdigest()==ECOBEE_SHA256,"Ecobee immutable amendment digest"
    a=decode_ecobee(blob) if amendment is None else amendment
    def strict_types(value):
        assert type(value) in (dict,list,str,int,bool),"Ecobee null or numeric type"
        if type(value) is dict:
            for key,child in value.items():
                assert type(key) is str,"Ecobee member type"
                strict_types(child)
        elif type(value) is list:
            for child in value:strict_types(child)
    strict_types(a)
    assert type(a) is dict and set(a)=={"schema","revision","baseline","decision","qualification","additions","removals","aliases","profile","consumers","technical_consumers"},"Ecobee shape"
    assert a["schema"]=="homeward.scope-amendment/v1" and a["revision"]=="ecobee_mvp_v1","Ecobee revision"
    baseline_bytes=(ROOT/"scope/manifest.json").read_bytes();baseline=json.loads(baseline_bytes)
    assert hashlib.sha256(baseline_bytes).hexdigest()==BASELINE_SHA256,"Ecobee complete baseline"
    assert a["baseline"]=={"revision":"v1","manifest_sha256":BASELINE_SHA256,"ids_sha256":baseline["ids_sha256"]},"Ecobee baseline reference"
    assert a["decision"]=={"file":"Décisions serveur et priorités.md","section":"Amendement Ecobee au périmètre initial","line":52,"sha256":"8ed66c2d5f9ff44af5b209a9b76a04d073f1cc79de5843eb9f57a2be4053cd84"},"Ecobee decision provenance"
    assert a["qualification"]=={"ticket":"NHA-009","commit":"cf65857901c2c761c6d8526fb49f8742b6af76c5","profile":"qualification/ecobee/profile.go","profile_sha256":"a004d734b27cbc7ddbbade33ed98d5b6188b0c5c87ad1cd54838a2d5c57164cb"},"Ecobee qualification provenance"
    assert hashlib.sha256((ROOT/a["qualification"]["profile"]).read_bytes()).hexdigest()==a["qualification"]["profile_sha256"],"Ecobee qualified profile bytes"
    assert a["additions"]==[{"entity_id":"climate.home","source":"homekit_controller"}] and a["removals"]==[] and a["aliases"]==[],"Ecobee exact ID diff"
    original={e["entity_id"]:e["source"] for e in baseline["entities"]}
    added={e["entity_id"]:e["source"] for e in a["additions"]}
    assert not original.keys()&added.keys(),"Ecobee baseline collision"
    selection=list(csv.DictReader((ROOT/"scope/sources/selection.tsv").read_text().splitlines(),delimiter="\t"))
    assert any(r["entity_id"]=="climate.home" and r["source"]=="homekit_controller" and r["decision"]!="garder" for r in selection),"Ecobee historical provenance"
    effective=original|added
    assert len(effective)==len(original)+len(added)==851 and len(set(effective.values()))==39,"Ecobee union counts"
    profile=a["profile"]
    assert type(profile) is dict and set(profile)=={"thermostat_service","linked_fan_services","fields","limits"},"Ecobee profile shape"
    assert profile["thermostat_service"]=="4A" and profile["linked_fan_services"]==["40","B7"],"Ecobee targeted services"
    rules=[("thermal_activity","F","uint8","",True,False),("requested_mode","33","uint8","",True,True),("current_temperature","11","float","celsius",True,False),("target_temperature","35","float","celsius",True,True),("display_units","36","uint8","",True,False),("relative_humidity","10","float","percentage",False,False),("heat_threshold","12","float","celsius",False,True),("cool_threshold","D","float","celsius",False,True),("observed_fan","AF","uint8","",False,False),("target_fan","BF","uint8","",False,True)]
    assert type(profile["fields"]) is list and len(profile["fields"])==10,"Ecobee field count"
    for field,rule in zip(profile["fields"],rules):
        assert type(field) is dict and set(field)=={"name","hap_type","format","unit","required","qualified_control","limitation"},"Ecobee field shape"
        assert type(field["required"]) is bool and type(field["qualified_control"]) is bool,"Ecobee boolean type"
        assert tuple(field[k] for k in ("name","hap_type","format","unit","required","qualified_control"))==rule,"Ecobee approved field"
        assert type(field["limitation"]) is str and field["limitation"],"Ecobee field limitation"
    assert type(profile["limits"]) is list and len(profile["limits"])==6 and all(type(v) is str and v for v in profile["limits"]),"Ecobee limits"
    assert "cannot execute" in profile["fields"][9]["limitation"] and "future action contract" in profile["fields"][6]["limitation"],"Ecobee current action API limit"
    expected=[("scene.climate_day_temperature",[],False,4),("scene.climate_night_temperature",[],False,4),("automation.turn_on_hvac",[],False,1735),("automation.turn_off_hvac",["scene.climate_night_temperature"],False,1650),("automation.good_morning_work_day",["scene.climate_day_temperature"],False,671),("automation.morning_routine",["scene.climate_day_temperature"],False,968),("script.morning_routine_weekend",["scene.climate_day_temperature"],True,229),("script.morning_routine_work_day",["scene.climate_day_temperature"],True,268),("automation.automatic_morning_routine",["script.morning_routine_work_day","scene.climate_day_temperature"],True,244)]
    known={v["entity_id"] for v in load("scope/logic.json")["objects"]}
    docs={d["file"]:d["sha256"] for d in load("compat/ha/catalogue.json")["source_documents"]}
    assert type(a["consumers"]) is list and len(a["consumers"])==len(expected),"Ecobee consumer count"
    for row,(identity,via,disabled,line) in zip(a["consumers"],expected):
        assert type(row) is dict and set(row)=={"entity_id","via","disabled_action_in_path","source"},"Ecobee consumer shape"
        assert row["entity_id"]==identity and row["via"]==via and type(row["disabled_action_in_path"]) is bool and row["disabled_action_in_path"]==disabled,"Ecobee sourced dependency edge"
        assert identity in known and set(via)<=known,"Ecobee consumer outside logic baseline"
        source=row["source"]
        assert type(source) is dict and set(source)=={"file","section","line","sha256"} and type(source["line"]) is int,"Ecobee consumer source shape"
        path=f"scope/logic_mvp_sources/{identity}.yaml" if identity.startswith("scene.") else ("Automatisations.md" if identity.startswith("automation.") else "Scripts et scènes.md")
        digest=hashlib.sha256((ROOT/path).read_bytes()).hexdigest() if path.startswith("scope/") else docs[path]
        assert source=={"file":path,"section":identity,"line":line,"sha256":digest},"Ecobee consumer provenance"
    tech=a["technical_consumers"]
    assert type(tech) is list and len(tech)==2,"Ecobee technical consumer count"
    for row,identity,via,ticket in zip(tech,["sensor.hvac_power_consumption","sensor.hvac_energy_consumption"],[["climate.home"],["sensor.hvac_power_consumption","climate.home"]],["NHA-107","NHA-108"]):
        assert type(row) is dict and set(row)=={"entity_id","via","ticket","source"},"Ecobee technical consumer shape"
        assert row["entity_id"]==identity and row["via"]==via and row["ticket"]==ticket and identity in original,"Ecobee power to energy chain"
        assert type(row["source"]) is dict and set(row["source"])=={"file","section","line","sha256"} and type(row["source"]["line"]) is int and re.fullmatch(r"[0-9a-f]{64}",row["source"]["sha256"]),"Ecobee technical provenance"
    digest=hashlib.sha256(("\n".join(sorted(effective))+"\n").encode()).hexdigest()
    return len(effective),len(set(effective.values())),digest

if __name__=="__main__":
    parser=argparse.ArgumentParser();parser.add_argument("--vault");args=parser.parse_args()
    print("850 unique IDs / 38 sources / 19 independent diagnostics / 89 inactive logic; SHA-256",validate())
    retained,deferred=validate_logic_mvp()
    print(f"{retained} retained definitions / {deferred} deferred / no activation; source hashes: pass")
    entities,sources,digest=validate_ecobee()
    print(f"Ecobee amendment: {entities} unique IDs / {sources} documentary sources; no activation; SHA-256 {digest}")
    if args.vault:compare_vault(args.vault);print("live vault equality + source hashes: pass")
