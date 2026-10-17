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
    if args.vault:compare_vault(args.vault);print("live vault equality + source hashes: pass")
