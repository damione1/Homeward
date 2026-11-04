#!/usr/bin/env python3
"""Reproduce functional contract fixtures and validate the current API schema."""
import json,pathlib,subprocess
ROOT=pathlib.Path(__file__).resolve().parents[1]
index=json.loads((ROOT/"contracts/golden/index.json").read_text())
# protoc is needed for regeneration, not for checking committed bindings at runtime.
for name,record in index.items():
    data=subprocess.check_output(["protoc","-I","proto","--encode="+record["message"],record["schema"]],input=(ROOT/f"contracts/golden/{name}.textproto").read_bytes(),cwd=ROOT)
    assert data==(ROOT/f"contracts/golden/{name}.bin").read_bytes(),name
# Immutable retired AUTH fixtures are provenance, not a supported v1 codec.
import hashlib,tempfile
historical=json.loads((ROOT/"contracts/auth_historical_sha256.json").read_text())
assert len(historical)==96
historical_paths={str(p.relative_to(ROOT/"contracts")) for directory in ("auth_golden","authorization_golden") for p in (ROOT/"contracts"/directory).iterdir() if p.is_file()}
assert historical_paths==set(historical)
for path,digest in historical.items():
    assert hashlib.sha256((ROOT/"contracts"/path).read_bytes()).hexdigest()==digest,path
# Resolve current public AUTH annotations through the pinned Buf module graph.
auth_cases=[]
with tempfile.TemporaryDirectory(prefix="homeward-auth-descriptor-") as work:
    descriptor=pathlib.Path(work)/"authentication.pb"
    subprocess.run(["buf","build","--path","proto/home/api/v2/authentication.proto","-o",str(descriptor)],cwd=ROOT,check=True)
    current_dir=ROOT/"contracts/auth_resource_golden"
    for row in (current_dir/"index.tsv").read_text().splitlines():
        if not row or row.startswith("#"): continue
        name,message,admitted=row.split("\t")
        assert admitted in ("true","false") and name not in auth_cases,name
        auth_cases.append(name)
        text=current_dir/f"{name}.textproto"
        if text.exists():
            data=subprocess.check_output(["protoc","--descriptor_set_in="+str(descriptor),"--encode=home.api.v2."+message],input=text.read_bytes(),cwd=ROOT)
        else:
            data=bytes.fromhex((current_dir/f"{name}.hex").read_text())
        assert data==(current_dir/f"{name}.bin").read_bytes(),name
assert len(auth_cases)==32
for line in (ROOT/"contracts/solar_golden/index.tsv").read_text().splitlines():
    name,message,admitted=line.split("\t")
    assert admitted in ("true","false")
    data=subprocess.check_output(["protoc","-I","proto","--encode=home.solar.v1."+message,"proto/home/solar/v1/solar.proto"],input=(ROOT/f"contracts/solar_golden/{name}.textproto").read_bytes(),cwd=ROOT)
    assert data==(ROOT/f"contracts/solar_golden/{name}.bin").read_bytes(),name
print(f"{len(index)} functional goldens and {len(auth_cases)} auth fixtures reproduced")
subprocess.run(["python3", "scripts/check-api.py"], cwd=ROOT, check=True)
