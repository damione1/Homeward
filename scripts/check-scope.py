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
if __name__=="__main__":
    parser=argparse.ArgumentParser();parser.add_argument("--vault");args=parser.parse_args()
    print("850 unique IDs / 38 sources / 19 independent diagnostics / 89 inactive logic; SHA-256",validate())
    retained,deferred=validate_logic_mvp()
    print(f"{retained} retained definitions / {deferred} deferred / no activation; source hashes: pass")
    if args.vault:compare_vault(args.vault);print("live vault equality + source hashes: pass")
