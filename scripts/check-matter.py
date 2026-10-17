#!/usr/bin/env python3
"""Pure documentary evidence gate. Never contacts any network or device."""
import json,pathlib
ROOT=pathlib.Path(__file__).resolve().parents[1]
def assess(e):
    eligible=e["apple_tv_variant"] in {"4k-gen3-wifi-ethernet","4k-gen2"}
    blockers=[]
    if not eligible:blockers.append("exact_eligible_tbr_variant")
    if not e["ipv6_mdns_proven"]:blockers.append("ipv6_mdns")
    if not e["server_image_digest"]:blockers.append("server_image_digest")
    if not e["commissioning_proven"]:blockers.append("commissioning")
    for key in ["thread_route_proven","fabric_ownership_proven","end_to_end_proven","recovery_proven"]:
        if not e[key]:blockers.append(key)
    available=e["observed_nodes"] if e["commissioning_proven"] and e["end_to_end_proven"] else 0
    return dict(tbr_qualified=eligible and e["ipv6_mdns_proven"] and e["thread_route_proven"],available_nodes=available,matter_ready=not blockers and available>0),blockers
if __name__=="__main__":
    e=json.loads((ROOT/"docs/matter/assessment.json").read_text());out,blockers=assess(e)
    assert out==e["expected"] and len(blockers)==8,(out,blockers)
    assert assess(dict(e,apple_tv_variant="4k-gen3-wifi-only"))[0]["tbr_qualified"] is False
    assert assess(dict(e,apple_tv_variant="4k-gen3-wifi-ethernet"))[0]["tbr_qualified"] is False
    proven=dict(e,apple_tv_variant="4k-gen3-wifi-ethernet",observed_nodes=1,ipv6_mdns_proven=True,server_image_digest="synthetic-digest",commissioning_proven=True,thread_route_proven=True,fabric_ownership_proven=True,end_to_end_proven=True,recovery_proven=True)
    assert assess(proven)==(dict(tbr_qualified=True,available_nodes=1,matter_ready=True),[])
    for key in ["ipv6_mdns_proven","commissioning_proven","thread_route_proven","fabric_ownership_proven","end_to_end_proven","recovery_proven"]:
        assert assess(dict(proven,**{key:False}))[0]["matter_ready"] is False,key
    assert assess(dict(proven,server_image_digest=None))[0]["matter_ready"] is False
    assert assess(dict(proven,observed_nodes=0))[0]["matter_ready"] is False
    print("unknown Apple TV -> TBR unqualified, zero available nodes, eight explicit blockers: pass")
    print("known hardware alone/unsupported variant/zero nodes cannot qualify Matter: pass")
