#!/usr/bin/env python3
"""Check actual transitive Go imports: domain cannot reach concrete drivers/client/transport."""
import json, subprocess
raw=subprocess.check_output(["go","list","-json","./internal/domain"] ,text=True)
pkg=json.loads(raw)
for dep in pkg.get("Deps",[]):
    assert not any(part in dep for part in ["/drivers/","/client/","/gen/","/internal/server","/internal/config"]),dep
print("domain dependency boundary: pass")
