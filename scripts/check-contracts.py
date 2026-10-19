#!/usr/bin/env python3
"""Golden reproduction and a real negative Buf breaking check in an external temporary tree."""
import json,pathlib,subprocess,tempfile,shutil
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
subprocess.run(["buf","breaking","--against",".compat/baseline"],cwd=ROOT,check=True)
with tempfile.TemporaryDirectory(prefix="nha004-breaking-") as directory:
    temp=pathlib.Path(directory);shutil.copytree(ROOT/"proto",temp/"proto");shutil.copy(ROOT/"buf.yaml",temp/"buf.yaml")
    file=temp/"proto/home/actions/v1/actions.proto";source=file.read_text();needle="  home.registry.v1.EntityRef target = 4;"
    assert needle in source;file.write_text(source.replace(needle,""))
    result=subprocess.run(["buf","breaking",str(temp),"--against",str(ROOT/".compat/baseline")],capture_output=True,text=True)
    assert result.returncode!=0 and 'target' in result.stdout+result.stderr and '4' in result.stdout+result.stderr,result.stdout+result.stderr
    print("negative compatibility probe rejected unreserved deletion of CommandRequest.target #4")
print(f"{len(index)} historical goldens and {len(auth_cases)} auth fixtures reproduced; Buf baseline compatible")
