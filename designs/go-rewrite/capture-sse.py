#!/usr/bin/env python
"""Capture a real SSE stream from the Python server as the P2 reference.

The stream is the contract P2 must reproduce: event names, the shape of each
frame, the ``sequence_number`` behaviour, and the headers that keep
intermediaries from buffering. Capturing it means tokenising and heartbeats can
be compared byte for byte instead of guessed at.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import httpx

_REPO_ROOT = Path(__file__).resolve().parents[2]
_DB = _REPO_ROOT / "go" / "testdata" / "reference.db"


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def main() -> int:
    tmp = Path(tempfile.mkdtemp(prefix="sse-capture-"))
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

        capture: dict = {"session_id": sid, "headers": {}, "frames": []}
        with httpx.stream("GET", f"{base}/v1/sessions/{sid}/stream", timeout=20.0) as resp:
            capture["status"] = resp.status_code
            for key, value in resp.headers.items():
                capture["headers"][key.lower()] = value
            started = time.monotonic()
            buffer = ""
            for chunk in resp.iter_text():
                buffer += chunk
                while "\n\n" in buffer:
                    raw, buffer = buffer.split("\n\n", 1)
                    if not raw.strip():
                        continue
                    frame = _parse_frame(raw)
                    capture["frames"].append(frame)
                # A stream with no traffic only sends heartbeats; a handful is
                # enough to compare cadence and shape.
                if time.monotonic() - started > 6 or len(capture["frames"]) >= 8:
                    break
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=15)
        except subprocess.TimeoutExpired:
            proc.kill()
        log.close()

    out = Path(__file__).with_name("sse_reference.json")
    out.write_text(json.dumps(capture, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"wrote {out}")
    print(f"status: {capture['status']}, frames: {len(capture['frames'])}")
    for header in ("content-type", "cache-control", "x-accel-buffering", "connection"):
        if header in capture["headers"]:
            print(f"  {header}: {capture['headers'][header]}")
    for frame in capture["frames"][:6]:
        print(f"  frame: event={frame.get('event')!r} keys={sorted(frame.get('data') or {})}")
    return 0


def _parse_frame(raw: str) -> dict:
    """Parse one SSE frame into its event name and decoded payload."""
    frame: dict = {"raw": raw}
    data_lines: list[str] = []
    for line in raw.splitlines():
        if line.startswith("event:"):
            frame["event"] = line[len("event:"):].strip()
        elif line.startswith("data:"):
            data_lines.append(line[len("data:"):].strip())
        elif line.startswith("id:"):
            frame["id"] = line[len("id:"):].strip()
    if data_lines:
        payload = "\n".join(data_lines)
        try:
            frame["data"] = json.loads(payload)
        except ValueError:
            frame["data_text"] = payload
    return frame


if __name__ == "__main__":
    sys.exit(main())
