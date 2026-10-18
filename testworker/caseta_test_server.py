"""Standalone local mTLS fixture for the real Go Host/PostgreSQL composition."""

import argparse
import asyncio
import hashlib
import json
import os
import signal
import sys
from pathlib import Path

from cryptography.hazmat.primitives import serialization

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from testworker.caseta_fake_bridge import FakeBridge
from testworker.caseta_worker_test import issue, settings_file


async def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--deny-away", action="store_true")
    parser.add_argument("--reconcile-seconds", type=int, default=1)
    args = parser.parse_args()
    root = args.root
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    ca = issue(root, "fake-ca")
    client = issue(root, "fake-client", ca)
    server = issue(root, "fake-server", ca)
    pin = hashlib.sha256(server[1].public_bytes(serialization.Encoding.DER)).digest()
    fake = FakeBridge(server[2], server[3], ca[2], args.deny_away)
    port = await fake.start()
    settings = settings_file(root, pin, port, args.reconcile_seconds)
    control_path = root / "control.sock"

    async def control(reader, writer):
        try:
            async for line in reader:
                result = await fake.control(json.loads(line))
                writer.write(json.dumps(result, separators=(",", ":")).encode() + b"\n")
                await writer.drain()
        finally:
            writer.close()
            await writer.wait_closed()

    server_control = await asyncio.start_unix_server(control, path=str(control_path))
    os.chmod(control_path, 0o600)
    ready_path = root / "ready.json"
    ready_path.write_text(json.dumps({"port": port, "settings": str(settings),
        "ca": str(ca[2]), "client_cert": str(client[2]),
        "client_key": str(client[3]), "control": str(control_path)}))
    os.chmod(ready_path, 0o600)
    stop = asyncio.Event()
    for sig in (signal.SIGTERM, signal.SIGINT):
        asyncio.get_running_loop().add_signal_handler(sig, stop.set)
    await stop.wait()
    server_control.close()
    await server_control.wait_closed()
    await fake.close()


if __name__ == "__main__":
    asyncio.run(main())
