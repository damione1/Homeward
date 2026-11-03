#!/usr/bin/env python3
"""Reproduce functional contract fixtures and validate the current API schema."""
import json,pathlib,subprocess
ROOT=pathlib.Path(__file__).resolve().parents[1]
index=json.loads((ROOT/"contracts/golden/index.json").read_text())
# protoc is needed for regeneration, not for checking committed bindings at runtime.
for name,record in index.items():
    data=subprocess.check_output(["protoc","-I","proto","--encode="+record["message"],record["schema"]],input=(ROOT/f"contracts/golden/{name}.textproto").read_bytes(),cwd=ROOT)
    assert data==(ROOT/f"contracts/golden/{name}.bin").read_bytes(),name
auth_dir=ROOT/"contracts/auth_golden"
auth_cases=[]
for line in (auth_dir/"index.tsv").read_text().splitlines():
    if not line or line.startswith("#"): continue
    name,message,admitted=line.split("\t")
    assert admitted in ("true","false") and name not in auth_cases,name
    auth_cases.append(name)
    text=auth_dir/f"{name}.textproto"
    if text.exists():
        data=subprocess.check_output(["protoc","-I","proto","--encode=home.auth.v1."+message,"home/auth/v1/auth.proto"],input=text.read_bytes(),cwd=ROOT)
    else:
        data=bytes.fromhex((auth_dir/f"{name}.hex").read_text())
    assert data==(auth_dir/f"{name}.bin").read_bytes(),name
for line in (ROOT/"contracts/solar_golden/index.tsv").read_text().splitlines():
    name,message,admitted=line.split("\t")
    assert admitted in ("true","false")
    data=subprocess.check_output(["protoc","-I","proto","--encode=home.solar.v1."+message,"proto/home/solar/v1/solar.proto"],input=(ROOT/f"contracts/solar_golden/{name}.textproto").read_bytes(),cwd=ROOT)
    assert data==(ROOT/f"contracts/solar_golden/{name}.bin").read_bytes(),name
print(f"{len(index)} functional goldens and {len(auth_cases)} auth fixtures reproduced")
subprocess.run(["python3", "scripts/check-api.py"], cwd=ROOT, check=True)
