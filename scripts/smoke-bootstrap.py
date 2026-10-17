#!/usr/bin/env python3
"""Real child process/SIGTERM and invalid-YAML proof; uses temporary directories outside repo."""
import pathlib, subprocess, tempfile, socket, signal, selectors, re
with tempfile.TemporaryDirectory(prefix="nha001-") as directory:
    root=pathlib.Path(directory); binary=root/"homed"
    subprocess.run(["go","build","-o",str(binary),"./cmd/homed"],check=True)
    cfg=root/"config.yaml";cfg.write_text(f'listen: "127.0.0.1:0"\n')
    proc=subprocess.Popen([str(binary),"-config",str(cfg)],stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True)
    try:
        selector=selectors.DefaultSelector()
        selector.register(proc.stdout,selectors.EVENT_READ)
        try:
            assert selector.select(timeout=5), "startup log timeout"
            line=proc.stdout.readline()
            port=int(re.search(r"bootstrap ready 127\.0\.0\.1:(\d+)",line)[1])
        finally: selector.close()
        proc.send_signal(signal.SIGTERM)
        output=proc.communicate(timeout=5)[0]
        assert proc.returncode==0 and "shutdown complete" in output
    finally:
        if proc.poll() is None:proc.kill();proc.wait()
    cfg.write_text(f'listen: "127.0.0.1:{port}"\nunknown: true\n')
    result=subprocess.run([str(binary),"-config",str(cfg)],capture_output=True,text=True,timeout=5)
    assert result.returncode!=0 and "field unknown" in result.stderr
    with socket.socket() as s:s.bind(("127.0.0.1",port))
print("process SIGTERM + invalid YAML/no port: pass")
