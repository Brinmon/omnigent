"""Shared plumbing for the Go-vs-Python acceptance scripts.

Both compare scripts start the same pair of servers against the same staged
database, and both need the same field-by-field JSON comparison. Sharing it here
keeps them from drifting apart, which would make their results incomparable.

Nothing here asserts anything: it only starts processes, stages fixtures, and
diffs values. The assertions stay in the scripts that own them.
"""

from __future__ import annotations

import os
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import httpx

REPO_ROOT = Path(__file__).resolve().parents[2]
GO_DIR = REPO_ROOT / "go"
REFERENCE_DB = GO_DIR / "testdata" / "reference.db"


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def clean_env() -> dict[str, str]:
    """An environment both servers start from, with inherited state removed."""
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


def stage_db(prefix: str = "server-compare-") -> Path:
    """Copy the reference database so neither server can alter the fixture."""
    if not REFERENCE_DB.is_file():
        raise SystemExit(
            f"missing {REFERENCE_DB}; run `python designs/go-rewrite/make-reference-db.py`"
        )
    staged = Path(tempfile.mkdtemp(prefix=prefix)) / "compare.db"
    staged.write_bytes(REFERENCE_DB.read_bytes())
    for suffix in ("-shm", "-wal"):
        Path(str(staged) + suffix).unlink(missing_ok=True)
    return staged


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


def start_python(db: Path, port: int, log: Path) -> subprocess.Popen[bytes]:
    python = REPO_ROOT / ".venv/Scripts/python.exe"
    if not python.is_file():
        python = REPO_ROOT / ".venv/bin/python"
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


def start_go(binary: Path, db: Path, port: int, log: Path,
             artifact_location: bool = False) -> subprocess.Popen[bytes]:
    """Start the Go server.

    ``artifact_location`` is opt-in because the flag appears with the bundle
    store in R1; passing it sooner makes every earlier comparison fail with an
    unknown-flag error rather than a real difference.
    """
    handle = open(log, "wb")
    argv = [str(binary), "serve", "--host", "127.0.0.1", "--port", str(port),
            "--database-uri", f"sqlite:///{db}"]
    if artifact_location:
        argv += ["--artifact-location", str(db.parent / "artifacts")]
    return subprocess.Popen(
        argv, env=clean_env(), cwd=str(db.parent),
        stdout=handle, stderr=subprocess.STDOUT,
    )


def build_go(workdir: Path, binary: str | None = None) -> Path:
    if binary:
        return Path(binary)
    target = workdir / ("omnigent-go.exe" if os.name == "nt" else "omnigent-go")
    build = subprocess.run(
        ["go", "build", "-o", str(target), "./cmd/omnigent"],
        cwd=str(GO_DIR), capture_output=True, text=True,
        encoding="utf-8", errors="replace",
    )
    if build.returncode != 0:
        print(build.stderr[-3000:])
        raise SystemExit("go build failed")
    return target


def stop(processes: list[subprocess.Popen[bytes]]) -> None:
    for process in processes:
        process.terminate()
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            process.kill()


def compare_json(path: str, want: object, got: object,
                 ignored: dict[str, str] | None = None) -> list[str]:
    """Return the differing field names, honouring a documented skip list.

    Compares shape as strictly as values: a key present on one side only is a
    difference, because a client reading that key would behave differently
    against each backend.
    """
    ignored = ignored or {}
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
            for sub in compare_json(path, want[key], got[key], ignored):
                diffs.append(f"{key}.{sub}" if not sub.startswith(f"{key}.") else sub)
        return diffs

    if isinstance(want, list) and isinstance(got, list):
        if len(want) != len(got):
            diffs.append(f"list length {len(got)} vs {want}")
            return diffs
        for index, (wantItem, gotItem) in enumerate(zip(want, got)):
            for diff in compare_json(path, wantItem, gotItem, ignored):
                diffs.append(f"[{index}].{diff}")
        return diffs

    if want != got:
        diffs.append(f"value {got!r} vs {want!r}")
    return diffs


def run(argv: list[str] | None) -> int:
    """Entry-point shim so the scripts stay importable under test."""
    raise NotImplementedError


if __name__ == "__main__":
    sys.exit("import this module; run one of the compare-*.py scripts")
