"""A8/A9: compare session creation between the Go and Python servers.

`POST /v1/sessions` is the one route openapi.json does not describe:
`requestBody.content` is empty and the 201 schema is `{}`. oapi-codegen therefore
hands Go an `interface{}`, so a wrong field name is not a compile error. This
script is the only guard, which is why it is written before the implementation.

Two layers, because they fail differently:

  A8  the HTTP response  -- what a client reading the API sees
  A9  the database rows  -- what a later read or a second process sees

A response can be right while the rows are wrong, and the rows are what the next
release has to keep reading.

Usage::

    python designs/go-rewrite/compare-create.py           # asserts, exit 0 on agree
    python designs/go-rewrite/compare-create.py -v        # show every field

Before R1 the Go server answers 501. The script reports that as NOT IMPLEMENTED
rather than as a shape difference, so it is usable as a progress meter.
"""

from __future__ import annotations

import argparse
import io
import json
import sqlite3
import sys
import tarfile
import gzip
from pathlib import Path

import httpx

from server_pair import (
    build_go,
    compare_json,
    free_port,
    stage_db,
    start_go,
    start_python,
    stop,
    wait_ready,
)

# The smallest agent bundle that the compat adapter accepts. The filename
# matters: a file called config.yaml takes the strict spec_version parser and
# fails with "missing required field: spec_version", while any other name takes
# the compat path that accepts this shorthand -- which is what the web client
# uploads.
AGENT_YAML = """\
name: probe_agent
description: Minimal agent for the create-session comparison.
executor:
  model: deepseek-v4.1-flash
  harness: pi
prompt: |
  You are a minimal probe agent. Answer briefly.
"""

# Fields that legitimately differ for two independent creates: identifiers and
# clocks. Everything else must match, including null-ness.
_VOLATILE = {"id", "agent_id", "created_at", "updated_at", "session_id",
             "agent_name", "root_conversation_id", "bundle_location",
             "last_item_id", "response_id"}


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


def snake_to_camel(name: str) -> str:
    head, *rest = name.split("_")
    return head + "".join(part[:1].upper() + part[1:] for part in rest)


def fetch_created_rows(db: Path, session_id: str) -> dict:
    """Read what a create wrote to the four tables, keyed by column name."""
    connection = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    connection.row_factory = sqlite3.Row
    try:
        raw = bytes.fromhex(session_id)
        result: dict = {}

        conversation = connection.execute(
            "SELECT * FROM conversations WHERE id = ?", (raw,)
        ).fetchone()
        result["conversations"] = _row_shape(conversation)

        if conversation is not None:
            agent = connection.execute(
                "SELECT * FROM agents WHERE id = ?", (conversation["agent_id"],)
            ).fetchone()
            result["agents"] = _row_shape(agent)

        grants = connection.execute(
            "SELECT * FROM session_permissions WHERE conversation_id = ?", (raw,)
        ).fetchall()
        result["session_permissions"] = [_row_shape(grant) for grant in grants]

        metadata = connection.execute(
            "SELECT * FROM omnigent_conversation_metadata WHERE id = ?", (raw,)
        ).fetchone()
        result["omnigent_conversation_metadata"] = _row_shape(metadata)
        return result
    finally:
        connection.close()


def _row_shape(row: sqlite3.Row | None) -> dict | None:
    """Reduce a row to column name -> a description of its value.

    Values differ between two creates (uuids, timestamps), so this records the
    value's *type and null-ness* instead. A backend writing NULL where the other
    writes an empty string is a real difference a client would see, and this
    keeps that visible without comparing unpredictable values.
    """
    if row is None:
        return None
    shape: dict = {}
    for key in row.keys():
        value = row[key]
        if key in _VOLATILE:
            shape[key] = "<volatile>"
        elif value is None:
            shape[key] = "NULL"
        elif isinstance(value, bytes):
            shape[key] = f"bytes[{len(value)}]"
        elif isinstance(value, str):
            shape[key] = "text" if value else "text(empty)"
        else:
            shape[key] = type(value).__name__
    return shape


def create_via_multipart(base: str, bundle: bytes, title: str) -> httpx.Response:
    return httpx.post(
        f"{base}/v1/sessions",
        files={"bundle": ("agent.tar.gz", bundle, "application/gzip")},
        data={"metadata": json.dumps({"title": title})},
        timeout=90.0,
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go-binary", default=None)
    parser.add_argument("-v", "--verbose", action="store_true",
                        help="show every compared field, not just differences")
    parser.add_argument("--keep-logs", action="store_true")
    args = parser.parse_args(argv)

    db = stage_db(prefix="create-compare-")
    workdir = db.parent
    go_binary = build_go(workdir, args.go_binary)

    py_port, go_port = free_port(), free_port()
    py_log, go_log = workdir / "python.log", workdir / "go.log"
    py = start_python(db, py_port, py_log)
    go_proc = start_go(go_binary, db, go_port, go_log, artifact_location=True)
    py_base, go_base = f"http://127.0.0.1:{py_port}", f"http://127.0.0.1:{go_port}"

    failures = 0
    skipped = 0
    try:
        for label, base in (("python", py_base), ("go", go_base)):
            try:
                wait_ready(base)
            except SystemExit as exc:
                print(f"{label} server did not start: {exc}")
                log = py_log if label == "python" else go_log
                print(log.read_text(errors="replace")[-3000:])
                return 1

        print(f"python: {py_base}\ngo:     {go_base}\ndatabase: {db}\n")

        bundle = build_bundle()

        # ── A8: does the multipart create answer the same way? ──────────────
        print("A8  multipart create -> HTTP response")
        py_create = create_via_multipart(py_base, bundle, "A8 create")
        go_create = create_via_multipart(go_base, bundle, "A8 create")

        if go_create.status_code == 501:
            print(f"{'':4}SKIP  Go answers 501; POST /v1/sessions is not implemented yet")
            skipped += 1
            failures += 1   # a skip here is still not-done, so it must not pass
        elif py_create.status_code != go_create.status_code:
            print(f"{'':4}FAIL  status {go_create.status_code} vs {py_create.status_code}")
            print(f"{'':4}      go body: {go_create.text[:300]}")
            failures += 1
        else:
            want = py_create.json()
            got = go_create.json()
            diffs = compare_json("/v1/sessions", want, got, _VOLATILE)
            if diffs:
                print(f"{'':4}FAIL  {len(diffs)} difference(s)")
                for diff in diffs[:10]:
                    print(f"{'':8}{diff}")
                failures += 1
            else:
                print(f"{'':4}OK    {len(want)} field(s) agree"
                      f" ({len(_VOLATILE & set(want))} volatile value(s) excluded)")

        # ── A9: do the rows agree? ──────────────────────────────────────────
        print("\nA9  multipart create -> database rows")
        py_id = _session_id(py_create)
        go_id = _session_id(go_create)
        if py_id is None:
            print(f"{'':4}FAIL  the Python create returned no session id: "
                  f"{py_create.text[:200]}")
            failures += 1
        elif go_id is None:
            print(f"{'':4}SKIP  the Go create returned no session id")
            skipped += 1
            failures += 1
        else:
            want_rows = fetch_created_rows(db, py_id)
            got_rows = fetch_created_rows(db, go_id)
            for table in sorted(want_rows):
                diffs = compare_json(f"A9.{table}", want_rows[table], got_rows[table])
                if diffs:
                    print(f"{'':4}FAIL  {table}: {len(diffs)} difference(s)")
                    for diff in diffs[:8]:
                        print(f"{'':8}{diff}")
                    failures += 1
                else:
                    columns = len(want_rows[table] or {})
                    print(f"{'':4}OK    {table}: {columns} column(s) agree")

        # ── A11: do both reject the same invalid bodies? ────────────────────
        print("\nA11 invalid bodies -> same status and error code")
        invalid_cases = [
            ("missing agent_id", {"title": "x"}),
            ("unknown agent_id", {"agent_id": "0" * 32}),
            ("malformed agent_id", {"agent_id": "not-a-uuid"}),
        ]
        for label, payload in invalid_cases:
            want = httpx.post(f"{py_base}/v1/sessions", json=payload, timeout=30.0)
            got = httpx.post(f"{go_base}/v1/sessions", json=payload, timeout=30.0)
            want_code = _error_code(want)
            got_code = _error_code(got)
            if got.status_code == 501:
                print(f"{'':4}SKIP  {label}: Go answers 501")
                skipped += 1
                failures += 1
                continue
            if want.status_code != got.status_code or want_code != got_code:
                print(f"{'':4}FAIL  {label}: {got.status_code}/{got_code}"
                      f" vs {want.status_code}/{want_code}")
                failures += 1
            else:
                print(f"{'':4}OK    {label}: {want.status_code}/{want_code}")

        # ── A8b: the JSON path binds to an existing agent ───────────────────
        print("\nA8b json create (bind to an existing agent) -> HTTP response")
        agent_id = _agent_id_of(db, py_id) if py_id else None
        if not agent_id:
            print(f"{'':4}SKIP  no agent to bind to")
            skipped += 1
        else:
            want = httpx.post(f"{py_base}/v1/sessions",
                              json={"agent_id": agent_id, "title": "A8b"}, timeout=60.0)
            got = httpx.post(f"{go_base}/v1/sessions",
                             json={"agent_id": agent_id, "title": "A8b"}, timeout=60.0)
            if got.status_code == 501:
                print(f"{'':4}SKIP  Go answers 501")
                skipped += 1
                failures += 1
            elif want.status_code != got.status_code:
                print(f"{'':4}FAIL  status {got.status_code} vs {want.status_code}")
                print(f"{'':4}      {got.text[:300]}")
                failures += 1
            else:
                diffs = compare_json("/v1/sessions", want.json(), got.json(), _VOLATILE)
                if diffs:
                    print(f"{'':4}FAIL  {len(diffs)} difference(s)")
                    for diff in diffs[:10]:
                        print(f"{'':8}{diff}")
                    failures += 1
                else:
                    print(f"{'':4}OK    {len(want.json())} field(s) agree")

        if args.verbose:
            print("\nA8/A8b payloads (Python):")
            print(json.dumps(py_create.json(), indent=2, sort_keys=True)[:2000])
    finally:
        stop([py, go_proc])
        if not args.keep_logs and failures:
            print(f"\nlogs: {py_log}\n      {go_log}")

    print()
    if failures:
        print(f"FAIL: {failures} check(s) differ ({skipped} not implemented yet)")
        return 1
    print("OK: session creation agrees between the Go and Python builds")
    return 0


def _session_id(response: httpx.Response) -> str | None:
    if response.status_code >= 400:
        return None
    try:
        body = response.json()
    except ValueError:
        return None
    return body.get("session_id") or body.get("id")


def _error_code(response: httpx.Response) -> str:
    try:
        body = response.json()
    except ValueError:
        return "<non-json>"
    error = body.get("error")
    if isinstance(error, dict):
        return str(error.get("code"))
    detail = body.get("detail")
    if isinstance(detail, list) and detail:
        loc = ".".join(str(part) for part in detail[0].get("loc") or [])
        return f"validation:{loc}"
    return "<none>"


def _agent_id_of(db: Path, session_id: str) -> str | None:
    connection = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    try:
        row = connection.execute(
            "SELECT agent_id FROM conversations WHERE id = ?", (bytes.fromhex(session_id),)
        ).fetchone()
        return row[0].hex() if row and row[0] else None
    finally:
        connection.close()


if __name__ == "__main__":
    sys.exit(main())
