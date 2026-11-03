#!/usr/bin/env python
"""Compare the Go and Python servers' responses on the same database.

This is the P1 acceptance step that matters: both builds read one SQLite file
written by Python, and every endpoint P1 implements must answer with the same
JSON. Comparing against a recorded golden would only prove the Go build matches
a snapshot; comparing live proves both builds agree now.

Usage::

    python designs/go-rewrite/compare-servers.py
    python designs/go-rewrite/compare-servers.py --go-binary /tmp/omnigent-go.exe

Exit 0 means every compared endpoint agreed.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path

import httpx

from server_pair import (
    GO_DIR,
    compare_json,
    free_port,
    build_go,
    stage_db,
    start_go,
    start_python,
    stop,
    wait_ready,
)

_REPO_ROOT = Path(__file__).resolve().parents[2]
_GO_DIR = GO_DIR

_PATHS = ["/v1/info", "/v1/me", "/v1/harnesses", "/v1/sessions"]

# /v1/agents is compared separately: the Python server seeds its packaged
# built-ins into the database at startup, so it lists agents this build does not
# have. Comparing the lists would report that whole gap as one diff on every run,
# which would hide a real regression in the rows both builds do have.

# Fields that legitimately differ between the two builds and are excluded from
# the comparison, with the reason. Keeping this list explicit is the point: a
# silent skip list would let a real regression hide.
_IGNORED: dict[str, dict[str, str]] = {
    "/v1/info": {
        "server_version": "both report the package version; asserted separately",
        # The Python build advertises harnesses it detects on the host, which is
        # environment state rather than contract. V1 supports pi only.
        "installable_harnesses": "V1 supports pi only",
        "session_event_types": "populated by the runner, which P1 does not have",
        "features": "feature flags track harness/runner presence",
    },
    "/v1/harnesses": {
        "data": "V1 advertises pi only; the Python build lists every harness it detects",
        "setup_steps": "keys follow the advertised harness list",
    },
}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go-binary", default=None,
                        help="Go binary (default: built into a temp dir)")
    parser.add_argument("--keep-logs", action="store_true",
                        help="leave server logs in place for inspection")
    args = parser.parse_args(argv)

    db = stage_db()
    workdir = db.parent

    go_binary = build_go(workdir, args.go_binary)

    py_port, go_port = free_port(), free_port()
    py_log, go_log = workdir / "python.log", workdir / "go.log"

    py = start_python(db, py_port, py_log)
    go_proc = start_go(go_binary, db, go_port, go_log)
    py_base, go_base = f"http://127.0.0.1:{py_port}", f"http://127.0.0.1:{go_port}"

    failures = 0
    try:
        try:
            wait_ready(py_base)
        except SystemExit as exc:
            print(f"Python server did not start: {exc}")
            print(py_log.read_text(errors="replace")[-3000:])
            return 1
        try:
            wait_ready(go_base, timeout=30)
        except SystemExit as exc:
            print(f"Go server did not start: {exc}")
            print(go_log.read_text(errors="replace")[-3000:])
            return 1

        print(f"python: {py_base}\ngo:     {go_base}\ndatabase: {db}\n")
        print(f"{'path':28} {'result':8} detail")
        print("-" * 78)

        for path in _PATHS:
            try:
                want_resp = httpx.get(py_base + path, timeout=30.0)
                got_resp = httpx.get(go_base + path, timeout=30.0)
            except Exception as exc:  # noqa: BLE001
                print(f"{path:28} ERROR    {type(exc).__name__}: {exc}")
                failures += 1
                continue

            if want_resp.status_code != got_resp.status_code:
                print(f"{path:28} FAIL     status {got_resp.status_code} vs "
                      f"{want_resp.status_code}")
                failures += 1
                continue

            try:
                want_json = want_resp.json()
                got_json = got_resp.json()
            except ValueError:
                if want_resp.text == got_resp.text:
                    print(f"{path:28} OK       (identical non-JSON body)")
                else:
                    print(f"{path:28} FAIL     non-JSON body differs")
                    failures += 1
                continue

            diffs = compare_json(path, want_json, got_json, _IGNORED.get(path))
            if diffs:
                print(f"{path:28} FAIL     {len(diffs)} difference(s)")
                for diff in diffs[:8]:
                    print(f"{'':38}{diff}")
                if len(diffs) > 8:
                    print(f"{'':38}... and {len(diffs) - 8} more")
                failures += 1
            else:
                ignored = len(_IGNORED.get(path, {}))
                note = f"({ignored} field(s) excluded by design)" if ignored else ""
                print(f"{path:28} OK       {note}")

        # The session detail read needs a real id, so it is compared on the
        # first row each server reports.
        try:
            sessions = httpx.get(go_base + "/v1/sessions", timeout=30.0).json()
            rows = sessions.get("data") or []
            if rows:
                sid = rows[0]["id"]
                want = httpx.get(f"{py_base}/v1/sessions/{sid}", timeout=30.0)
                got = httpx.get(f"{go_base}/v1/sessions/{sid}", timeout=30.0)
                if want.status_code == got.status_code and want.json() == got.json():
                    print(f"{'/v1/sessions/<id>':28} OK       detail read")
                else:
                    diffs = compare_json("/v1/sessions/{id}", want.json(), got.json(),
                                     _IGNORED.get("/v1/sessions/{id}"))
                    print(f"{'/v1/sessions/<id>':28} FAIL     {diffs[:4]}")
                    failures += 1
            else:
                print(f"{'/v1/sessions/<id>':28} SKIP     no rows in the fixture")
        except Exception as exc:  # noqa: BLE001
            print(f"{'/v1/sessions/<id>':28} ERROR    {type(exc).__name__}: {exc}")
            failures += 1

    finally:
        stop([py, go_proc])

    print()
    if failures:
        print(f"FAIL: {failures} endpoint(s) differ")
        if not args.keep_logs:
            print(f"logs: {py_log}\n      {go_log}")
        return 1
    print("OK: every P1 endpoint agrees between the Go and Python builds")
    return 0


if __name__ == "__main__":
    sys.exit(main())
