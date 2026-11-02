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
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import httpx

_REPO_ROOT = Path(__file__).resolve().parents[2]
_GO_DIR = _REPO_ROOT / "go"
_REFERENCE_DB = _GO_DIR / "testdata" / "reference.db"

# Endpoints P1 implements. Each is compared field by field.
_PATHS = ["/v1/info", "/v1/me", "/v1/harnesses", "/v1/sessions"]

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


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_ready(base: str, timeout: float = 120.0) -> None:
    deadline = time.monotonic() + timeout
    last = "not polled"
    while time.monotonic() < deadline:
        try:
            resp = httpx.get(f"{base}/health", timeout=3.0)
            # Any HTTP answer means the process is listening; a 4xx/5xx on
            # /health is a routing problem the caller will surface, not a
            # startup failure.
            if resp.status_code < 600:
                return
            last = f"HTTP {resp.status_code}"
        except Exception as exc:  # noqa: BLE001 - any failure means "not yet"
            last = f"{type(exc).__name__}: {exc}"
        time.sleep(0.5)
    raise SystemExit(f"server at {base} never became ready ({last})")


def clean_env() -> dict[str, str]:
    env = dict(os.environ)
    env.update(
        PYTHONUTF8="1",
        NO_PROXY="127.0.0.1,localhost",
        OMNIGENT_DISABLE_CATALOG_LOOKUP="1",
        OMNIGENT_DISABLE_TELEMETRY="1",
        OMNIGENT_AUTH_PROVIDER="header",
        OMNIGENT_LOCAL_SINGLE_USER="1",
    )
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                  "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)
    # Keep the developer's global Pi packages out of both servers.
    env["PI_CODING_AGENT_DIR"] = str(Path(tempfile.gettempdir()) / "interop-pi-agent")
    return env


def stage_db() -> Path:
    """Copy the reference database so neither server can alter the fixture."""
    if not _REFERENCE_DB.is_file():
        raise SystemExit(
            f"missing {_REFERENCE_DB}; run `python designs/go-rewrite/make-reference-db.py`"
        )
    tmp = Path(tempfile.mkdtemp(prefix="server-compare-")) / "compare.db"
    tmp.write_bytes(_REFERENCE_DB.read_bytes())
    for suffix in ("-shm", "-wal"):
        stale = Path(str(tmp) + suffix)
        stale.unlink(missing_ok=True)
    return tmp


def start_python(db: Path, port: int, log: Path) -> subprocess.Popen[bytes]:
    python = _REPO_ROOT / ".venv/Scripts/python.exe"
    if not python.is_file():
        python = _REPO_ROOT / ".venv/bin/python"
    if not python.is_file():
        raise SystemExit("no .venv python found")
    handle = open(log, "wb")
    return subprocess.Popen(
        [
            str(python), "-m", "omnigent", "server",
            "--host", "127.0.0.1", "--port", str(port),
            "--database-uri", f"sqlite:///{db}",
            "--artifact-location", str(db.parent / "artifacts"),
        ],
        env=clean_env(), cwd=str(db.parent), stdout=handle, stderr=subprocess.STDOUT,
    )


def start_go(binary: Path, db: Path, port: int, log: Path) -> subprocess.Popen[bytes]:
    handle = open(log, "wb")
    return subprocess.Popen(
        [str(binary), "serve", "--host", "127.0.0.1", "--port", str(port),
         "--database-uri", f"sqlite:///{db}"],
        env=clean_env(), cwd=str(db.parent), stdout=handle, stderr=subprocess.STDOUT,
    )


def compare_json(path: str, want: object, got: object) -> list[str]:
    """Return the differing field names, honouring the documented skip list."""
    ignored = _IGNORED.get(path, {})
    diffs: list[str] = []

    if isinstance(want, dict) and isinstance(got, dict):
        keys = sorted(set(want) | set(got))
        for key in keys:
            if key in ignored:
                continue
            if key not in want:
                diffs.append(f"{key}: only in Go")
                continue
            if key not in got:
                diffs.append(f"{key}: only in Python")
                continue
            for sub in compare_json(path, want[key], got[key]):
                diffs.append(f"{key}.{sub}" if not sub.startswith(f"{key}.") else sub)
        return diffs

    if isinstance(want, list) and isinstance(got, list):
        if len(want) != len(got):
            diffs.append(f"list length {len(got)} vs {len(want)}")
            return diffs
        for index, (wantItem, gotItem) in enumerate(zip(want, got)):
            for diff in compare_json(path, wantItem, gotItem):
                diffs.append(f"[{index}].{diff}")
        return diffs

    if want != got:
        diffs.append(f"value {got!r} vs {want!r}")
    return diffs


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go-binary", default=None,
                        help="Go binary (default: built into a temp dir)")
    parser.add_argument("--keep-logs", action="store_true",
                        help="leave server logs in place for inspection")
    args = parser.parse_args(argv)

    db = stage_db()
    workdir = db.parent

    go_binary: Path
    if args.go_binary:
        go_binary = Path(args.go_binary)
    else:
        go_binary = workdir / ("omnigent-go.exe" if os.name == "nt" else "omnigent-go")
        build = subprocess.run(
            ["go", "build", "-o", str(go_binary), "./cmd/omnigent"],
            cwd=str(_GO_DIR), capture_output=True, text=True,
            encoding="utf-8", errors="replace",
        )
        if build.returncode != 0:
            print(build.stderr[-3000:])
            raise SystemExit("go build failed")

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

            diffs = compare_json(path, want_json, got_json)
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
                    diffs = compare_json("/v1/sessions/{id}", want.json(), got.json())
                    print(f"{'/v1/sessions/<id>':28} FAIL     {diffs[:4]}")
                    failures += 1
            else:
                print(f"{'/v1/sessions/<id>':28} SKIP     no rows in the fixture")
        except Exception as exc:  # noqa: BLE001
            print(f"{'/v1/sessions/<id>':28} ERROR    {type(exc).__name__}: {exc}")
            failures += 1

    finally:
        for proc in (py, go_proc):
            proc.terminate()
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                proc.kill()

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
