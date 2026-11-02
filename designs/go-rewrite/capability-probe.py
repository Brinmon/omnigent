"""Probe what actually works end to end on the Go build.

Answers a concrete question -- can someone use this to chat, and to run a
sub-agent? -- by driving the real HTTP API rather than reading code.

Usage:
    python designs/go-rewrite/capability-probe.py
"""
import json
import os
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import httpx

REPO = Path(__file__).resolve().parents[2]

# Endpoints a normal chat session touches, in the order the client uses them.
ENDPOINTS = [
    ("GET", "/v1/info"),
    ("GET", "/v1/me"),
    ("GET", "/v1/harnesses"),
    ("GET", "/v1/sessions"),
    ("GET", "/v1/sessions/{sid}"),
    ("GET", "/v1/sessions/{sid}/items"),
    ("GET", "/v1/sessions/{sid}/stream"),
    ("POST", "/v1/sessions/{sid}/events"),
    ("POST", "/v1/sessions"),
    ("PATCH", "/v1/sessions/{sid}"),
    ("DELETE", "/v1/sessions/{sid}"),
    ("POST", "/v1/sessions/{sid}/fork"),
    ("GET", "/v1/sessions/{sid}/permissions"),
    ("POST", "/v1/sessions/{sid}/policies/evaluate"),
    ("GET", "/v1/hosts"),
    ("GET", "/v1/projects"),
]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def main() -> int:
    workdir = Path(tempfile.mkdtemp(prefix="probe-"))
    db = workdir / "p.db"
    db.write_bytes((REPO / "go/testdata/reference.db").read_bytes())

    binary = workdir / "omnigent-go.exe"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/omnigent"],
                   cwd=str(REPO / "go"), check=True, capture_output=True)

    port = free_port()
    env = dict(os.environ)
    env.update(PYTHONUTF8="1", NO_PROXY="127.0.0.1,localhost",
               OMNIGENT_PI_MODEL="openai/deepseek-v4.1-flash")
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                  "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)

    log = open(workdir / "s.log", "wb")
    process = subprocess.Popen(
        [str(binary), "serve", "--port", str(port), "--database-uri", f"sqlite:///{db}"],
        env=env, cwd=str(REPO), stdout=log, stderr=subprocess.STDOUT,
    )
    base = f"http://127.0.0.1:{port}"

    try:
        for _ in range(100):
            try:
                if httpx.get(f"{base}/health", timeout=2).status_code < 600:
                    break
            except Exception:
                pass
            time.sleep(0.3)

        sid = httpx.get(f"{base}/v1/sessions", timeout=10).json()["data"][0]["id"]

        print(f"{'method':7} {'endpoint':44} {'status':7} meaning")
        print("-" * 92)
        working = broken = 0
        for method, template in ENDPOINTS:
            path = template.replace("{sid}", sid)
            try:
                if method == "GET" and path.endswith("/stream"):
                    # A stream never completes; check the status line only.
                    with httpx.stream("GET", base + path, timeout=5.0) as response:
                        status = response.status_code
                elif method in ("POST", "PATCH"):
                    response = httpx.request(method, base + path, json={}, timeout=15.0)
                    status = response.status_code
                elif method == "DELETE":
                    response = httpx.request(method, base + path, timeout=15.0)
                    status = response.status_code
                else:
                    response = httpx.request(method, base + path, timeout=15.0)
                    status = response.status_code
            except Exception as exc:
                print(f"{method:7} {template:44} {'ERR':7} {type(exc).__name__}")
                broken += 1
                continue

            if status == 501:
                meaning = "NOT IMPLEMENTED"
                broken += 1
            elif status < 400:
                meaning = "works"
                working += 1
            elif status == 404:
                meaning = "route missing"
                broken += 1
            else:
                meaning = "responds (rejected this request shape)"
                working += 1
            print(f"{method:7} {template:44} {status:<7} {meaning}")

        print()
        print(f"working: {working}   not implemented / missing: {broken}")

        # The actual question: does a sub-agent tool exist?
        print()
        print("sub-agent support:")
        body = httpx.get(f"{base}/v1/harnesses", timeout=10).json()
        for entry in body.get("data") or []:
            caps = entry.get("capabilities") or {}
            print(f"  harness {entry.get('id')}: subagents={caps.get('subagents')}")
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except Exception:
            process.kill()
        log.close()

    return 0


if __name__ == "__main__":
    sys.exit(main())
