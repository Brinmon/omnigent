"""Capture the real POST /v1/sessions contract from the Python server.

openapi.json does not describe this route's body or response (`requestBody.content`
is empty and the 201 schema is `{}`), so there is no compile-time guard to inherit.
The only way to know the contract is to drive it and record what happens.

Captures both shapes the route dispatches on:
  * application/json  -- bind to an already-registered agent by agent_id
  * multipart/form-data -- upload an agent bundle and create it in one transaction

Usage:
    python designs/go-rewrite/capture-create-session.py
"""

from __future__ import annotations

import gzip
import io
import json
import os
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
from pathlib import Path

import httpx

REPO = Path(__file__).resolve().parents[2]

# The smallest agent that can answer a turn. Written as a bundle tar.gz because
# that is the shape the client uploads.
AGENT_YAML = """\
name: probe_agent
description: Minimal agent for the create-session probe.
executor:
  harness: pi
prompt: |
  You are a minimal probe agent. Answer briefly.
"""


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def build_bundle() -> bytes:
    """Render AGENT_YAML as a gzipped tar, the shape the client uploads."""
    buffer = io.BytesIO()
    with gzip.GzipFile(fileobj=buffer, mode="wb", mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w") as tar:
            data = AGENT_YAML.encode("utf-8")
            info = tarfile.TarInfo(name="probe_agent.yaml")
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
    return buffer.getvalue()


def main() -> int:
    workdir = Path(tempfile.mkdtemp(prefix="create-probe-"))
    db = workdir / "c.db"
    db.write_bytes((REPO / "go/testdata/reference.db").read_bytes())

    port = free_port()
    env = dict(os.environ)
    env.update(
        PYTHONUTF8="1",
        NO_PROXY="127.0.0.1,localhost",
        OMNIGENT_DISABLE_CATALOG_LOOKUP="1",
        OMNIGENT_DISABLE_TELEMETRY="1",
        OMNIGENT_AUTH_PROVIDER="header",
        OMNIGENT_LOCAL_SINGLE_USER="1",
        OMNIGENT_PI_MODEL="openai/deepseek-v4.1-flash",
    )
    for stale in (
        "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
        "http_proxy", "https_proxy", "all_proxy",
    ):
        env.pop(stale, None)

    log = open(workdir / "s.log", "wb")
    process = subprocess.Popen(
        [str(REPO / ".venv/Scripts/python.exe"), "-m", "omnigent", "server",
         "--host", "127.0.0.1", "--port", str(port),
         "--database-uri", f"sqlite:///{db}",
         "--artifact-location", str(workdir / "artifacts")],
        env=env, cwd=str(workdir), stdout=log, stderr=subprocess.STDOUT,
    )
    base = f"http://127.0.0.1:{port}"
    capture: dict = {}

    try:
        for _ in range(160):
            try:
                if httpx.get(f"{base}/health", timeout=3.0).status_code < 600:
                    break
            except Exception:
                pass
            time.sleep(0.5)

        sessions = httpx.get(f"{base}/v1/sessions", timeout=20.0).json()
        existing = sessions["data"][0]
        capture["existing_session_keys"] = sorted(existing)

        # ── 1. multipart: upload a bundle and create in one transaction ──────
        bundle = build_bundle()
        metadata = {"title": "Probe from bundle"}
        multipart = httpx.post(
            f"{base}/v1/sessions",
            files={"bundle": ("agent.tar.gz", bundle, "application/gzip")},
            data={"metadata": json.dumps(metadata)},
            timeout=60.0,
        )
        capture["multipart"] = {
            "status": multipart.status_code,
            "body": _safe_json(multipart),
        }

        created_id = None
        if multipart.status_code < 400:
            body = _safe_json(multipart) or {}
            created_id = body.get("session_id") or body.get("id")

        # ── 2. json: bind to the agent the bundle just created ──────────────
        if created_id:
            detail = httpx.get(f"{base}/v1/sessions/{created_id}", timeout=20.0).json()
            capture["created_session_keys"] = sorted(detail)
            agent_id = detail.get("agent_id")
            capture["created_agent_id"] = agent_id

            json_create = httpx.post(
                f"{base}/v1/sessions",
                json={"agent_id": agent_id, "title": "Probe from json"},
                timeout=60.0,
            )
            capture["json"] = {
                "status": json_create.status_code,
                "body": _safe_json(json_create),
            }

        # ── 3. what a minimal JSON body needs, by omission ───────────────────
        probes = [
            ("empty", {}),
            ("title only", {"title": "x"}),
            ("bad agent_id", {"agent_id": "00000000000000000000000000000000"}),
            ("agent_id as uuid", {"agent_id": "not-a-uuid"}),
        ]
        capture["json_validation"] = {}
        for label, payload in probes:
            response = httpx.post(f"{base}/v1/sessions", json=payload, timeout=30.0)
            capture["json_validation"][label] = {
                "status": response.status_code,
                "body": _safe_json(response) or response.text[:200],
            }

        # ── 4. what the created session looks like in the database ──────────
        if created_id:
            capture["created_row"] = _row_snapshot(db, created_id)

    finally:
        process.terminate()
        try:
            process.wait(timeout=15)
        except Exception:
            process.kill()
        log.close()

    out = Path(__file__).with_name("create_session_reference.json")
    out.write_text(json.dumps(capture, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    print(f"wrote {out}")
    for key in ("multipart", "json"):
        entry = capture.get(key)
        if entry:
            print(f"\n{key}: status {entry['status']}")
            body = entry.get("body") or {}
            if isinstance(body, dict):
                for name in sorted(body)[:14]:
                    value = str(body[name])[:60]
                    print(f"  {name:26} {value}")
    for label, entry in (capture.get("json_validation") or {}).items():
        print(f"\njson {label:18} -> {entry['status']}: {str(entry['body'])[:120]}")
    return 0


def _safe_json(response: httpx.Response) -> object:
    try:
        return response.json()
    except ValueError:
        return None


def _row_snapshot(db: Path, session_id: str) -> dict:
    """Read what the create wrote, so the Go implementation knows what to insert."""
    import sqlite3

    connection = sqlite3.connect(db)
    try:
        snapshot: dict = {}
        raw = bytes.fromhex(session_id)
        row = connection.execute(
            "SELECT id, created_at, updated_at, title, parent_conversation_id,"
            " root_conversation_id, next_position, workspace_id, archived, agent_id,"
            " session_overrides FROM conversations WHERE id = ?", (raw,)
        ).fetchone()
        snapshot["conversation"] = [str(value) for value in row] if row else None

        agent = connection.execute(
            "SELECT id, created_at, name, bundle_location, version, kind, created_by"
            " FROM agents WHERE id = (SELECT agent_id FROM conversations WHERE id = ?)",
            (raw,)
        ).fetchone()
        snapshot["agent"] = [str(value) for value in agent] if agent else None

        grants = connection.execute(
            "SELECT user_id, level FROM session_permissions WHERE conversation_id = ?",
            (raw,)
        ).fetchall()
        snapshot["permissions"] = [list(map(str, grant)) for grant in grants]

        metadata = connection.execute(
            "SELECT kind, runner_id, live_status FROM omnigent_conversation_metadata"
            " WHERE id = ?", (raw,)
        ).fetchone()
        snapshot["metadata"] = list(map(str, metadata)) if metadata else None
        return snapshot
    finally:
        connection.close()


if __name__ == "__main__":
    sys.exit(main())
