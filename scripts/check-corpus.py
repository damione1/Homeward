#!/usr/bin/env python3
"""Narrow fixture oracle and structural checks. Does NOT execute HA or a Go runtime."""
import copy, datetime, json, pathlib, re
from zoneinfo import ZoneInfo
ROOT=pathlib.Path(__file__).resolve().parents[1]/"compat/ha"
def oracle(config,case):
    i=case["inputs"];kind=case["kind"];a=config.get("automation",[{}])[0]
    if kind=="runs":
        mode=a["mode"];delay=a["actions"][0]["delay"];out=[];active=None;queue=[]
        def complete(t):
            nonlocal active
            while active and active["end"]<=t:
                end=active["end"];out.append(dict(at=end,event="action",run=active["run"]));active=None
                if queue:
                    nxt=queue.pop(0);active=dict(run=nxt["run"],end=end+delay);out.append(dict(at=end,event="start",run=nxt["run"]))
        for trigger in i["triggers"]:
            t=trigger["at"];complete(t);run=trigger["run"]
            if active and mode=="restart":out.append(dict(at=t,event="cancel",run=active["run"]));active=None
            if active:
                if len(queue)+1>=a["max"]:out.append(dict(at=t,event="reject_full",run=run))
                else:queue.append(trigger);out.append(dict(at=t,event="enqueue",run=run))
            else:active=dict(run=run,end=t+delay);out.append(dict(at=t,event="start",run=run))
        complete(i["until"]);return out
    if kind=="wait":
        deadline=i["start"]+a["actions"][0]["timeout"]
        release=next((e["at"] for e in i["release_events"] if i["start"]<=e["at"]<deadline),None)
        return [] if release is None else [dict(at=release,event="action")]
    if kind=="mqtt":
        trigger=a["triggers"][0]
        return [dict(at=e["at"],event="increment") for e in i["messages"] if e["topic"]==trigger["topic"] and e["payload"]==trigger["payload"]]
    if kind=="scene":
        state=i["initial"];saved=None;out=[]
        for action in a["actions"]:
            name=action["action"]
            if name=="scene.create":saved=state
            elif name=="input_boolean.turn_on":state="on"
            elif name=="scene.turn_on":state=saved
            else:raise AssertionError(name)
            out.append(state)
        return out
    if kind=="dates":
        now=datetime.datetime.fromisoformat(i["now"]).astimezone(ZoneInfo(i["timezone"]))
        seed=datetime.datetime.fromisoformat(i["seed_date"]).replace(tzinfo=ZoneInfo(i["timezone"]))
        return dict(calendar_days=(now.date()-seed.date()).days,elapsed_hours=(now.timestamp()-seed.timestamp())/3600)
    if kind in ("state","for"):
        trigger=a["triggers"][0];delay=trigger.get("for",0);previous=None;deadline=None;out=[]
        for report in i["reports"]:
            if deadline is not None and deadline<=report["at"]:out.append(dict(at=deadline,event="action"));deadline=None
            if report["state"]!=previous:
                deadline=report["at"]+delay if previous is not None and report["state"]==trigger["to"] else None
                previous=report["state"]
                if deadline is not None and delay==0:out.append(dict(at=deadline,event="action"));deadline=None
        if deadline is not None and deadline<=i["until"]:out.append(dict(at=deadline,event="action"))
        return out
    if kind=="scripts":
        scripts=config["script"];delay=scripts["nha_child"]["sequence"][0]["delay"]
        assert scripts["nha_direct"]["sequence"][0]["action"]=="script.nha_child"
        assert scripts["nha_detached"]["sequence"][0]["action"]=="script.turn_on"
        return dict(direct_after=i["start"]+delay,detached_after=i["start"],child_action=i["start"]+delay)
    if kind=="default":
        assert "float(0)" in a["actions"][0]["data"]["value"]
        try:value=float(i["state"])
        except ValueError:value=0
        return dict(value=value,migration_quality="unknown/stale remains a separate business decision")
    raise AssertionError(kind)
def check():
    catalogue=json.loads((ROOT/"catalogue.json").read_text())
    assert len(catalogue["source_documents"])==4 and catalogue["observations"] and catalogue["non_executable"]
    assert all(o["execution_status"]=="non_executable" and o["replacement"] for o in catalogue["non_executable"])
    ids=json.loads((ROOT/"fixtures/index.json").read_text())["fixtures"];assert len(ids)==len(set(ids))==10
    for id in ids:
        config_text=(ROOT/f"fixtures/{id}.yaml").read_text();case_text=(ROOT/f"fixtures/{id}.case.json").read_text()
        config=json.loads(config_text);case=json.loads(case_text)
        assert case["synthetic"] is True and case["ha_reference_executed"] is False
        assert case["observed_sources"] and case["expected"] is not None
        assert not re.search(r"EXPURG|!input|!include|latitude|longitude|token|password|https?://|rtsp://|192\.168\.|10\.0\.",config_text+case_text,re.I),id
        # Reject real imported entity IDs: only the explicit nha namespace is permitted.
        for entity in re.findall(r"\b(?:input_boolean|input_number|counter|scene|script)\.[a-zA-Z0-9_]+",config_text):
            assert entity.split(".")[1].startswith("nha_") or entity in {"input_boolean.turn_on", "input_number.set_value", "counter.increment", "scene.create", "scene.turn_on", "script.turn_on"},entity
        actual=oracle(config,case);assert actual==case["expected"],f"{id}: expected {case['expected']}, got {actual}"
        bad=copy.deepcopy(case);bad["expected"]={"deliberate":"corruption"}
        assert oracle(config,bad)!=bad["expected"],"negative oracle check did not detect corruption"
        print(id,": expected trace matches narrow oracle; HA reference NOT run")
if __name__=="__main__":check()
