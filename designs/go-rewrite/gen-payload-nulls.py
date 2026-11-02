"""Generate the per-item-type null field table from a live Python server.

The stored payload uses the model's field names; the HTTP response uses
serialization aliases and additionally emits some optional fields as null. Which
ones is a property of each model's own JSON schema, so it is captured from a real
response rather than inferred from the generated Go struct: the struct marks
every optional field `omitempty`, which would add keys the server never sends.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

_REPO_ROOT = Path(__file__).resolve().parents[2]
_GO_DIR = _REPO_ROOT / "go"
_DB = _GO_DIR / "testdata" / "reference.db"

import httpx  # noqa: E402


def free_port() -> int:
    import socket

    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def capture() -> dict[str, dict]:
    tmp = Path(tempfile.mkdtemp(prefix="payload-nulls-"))
    db = tmp / "c.db"
    db.write_bytes(_DB.read_bytes())
    port = free_port()
    env = dict(os.environ)
    env.update(
        PYTHONUTF8="1", NO_PROXY="127.0.0.1,localhost",
        OMNIGENT_DISABLE_CATALOG_LOOKUP="1", OMNIGENT_DISABLE_TELEMETRY="1",
        OMNIGENT_AUTH_PROVIDER="header", OMNIGENT_LOCAL_SINGLE_USER="1",
    )
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                  "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)

    log = open(tmp / "server.log", "wb")
    proc = subprocess.Popen(
        [sys.executable, "-m", "omnigent", "server", "--host", "127.0.0.1",
         "--port", str(port), "--database-uri", f"sqlite:///{db}",
         "--artifact-location", str(tmp / "art")],
        env=env, cwd=str(tmp), stdout=log, stderr=subprocess.STDOUT,
    )
    base = f"http://127.0.0.1:{port}"
    try:
        for _ in range(180):
            try:
                if httpx.get(f"{base}/health", timeout=3).status_code < 600:
                    break
            except Exception:
                pass
            time.sleep(1)
        sessions = httpx.get(f"{base}/v1/sessions", timeout=20).json()["data"]
        sid = sessions[0]["id"]
        items = httpx.get(f"{base}/v1/sessions/{sid}", timeout=20).json()["items"]
        captured: dict[str, dict] = {}
        for item in items:
            captured.setdefault(item["type"], item["data"])
        return captured
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()


def main() -> int:
    captured = capture()
    if not captured:
        raise SystemExit("captured no items; the fixture may be empty")

    out = Path(__file__).with_name("payload_wire.json")
    out.write_text(json.dumps(captured, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"wrote {out} ({len(captured)} item types)")
    for item_type in sorted(captured):
        print(f"  {item_type:22} {len(captured[item_type])} fields")
    return 0


if __name__ == "__main__":
    sys.exit(main())
