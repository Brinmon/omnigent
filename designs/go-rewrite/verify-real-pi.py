#!/usr/bin/env python
"""Drive the real Pi harness against a real OpenAI-compatible endpoint.

Smoke-tests the whole Windows path — spec loading, runner spawn, Pi RPC,
provider credentials, streaming — without the mock LLM server, so a failure
here is a genuine product defect rather than a test-scaffold artifact.

Credentials are read from the Pi agent dir's ``models.json`` (the same file
Pi itself uses), so nothing secret lives in this repo or on the command line.

Usage::

    python designs/go-rewrite/verify-real-pi.py
    python designs/go-rewrite/verify-real-pi.py --model deepseek-v4.1-flash
    python designs/go-rewrite/verify-real-pi.py --provider openai --expect PONG

Exit code 0 means the assistant produced the expected text.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

_REPO_ROOT = Path(__file__).resolve().parents[2]
_AGENT_YAML = """\
name: real-pi-smoke
description: Real Pi harness against a real provider endpoint.
executor:
  harness: pi
prompt: |
  You are a minimal smoke-test agent. Answer exactly as instructed.
"""

# Windows cannot start a process with a stripped environment: Winsock
# initialisation needs SYSTEMROOT, and Path.home() needs USERPROFILE. Keep the
# same allowlist the e2e suite uses rather than inheriting everything.
_KEEP_ENV = {
    "PATH", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
    "TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA",
    "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)",
    "PROGRAMW6432", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
    "USERNAME", "LANG", "LC_ALL",
}


def _load_provider(agent_dir: Path, provider: str) -> tuple[str, str]:
    """Return ``(base_url, api_key)`` for *provider* from Pi's models.json."""
    data = json.loads((agent_dir / "models.json").read_text(encoding="utf-8"))
    providers = data.get("providers", data)
    entry = providers.get(provider)
    if not isinstance(entry, dict):
        known = ", ".join(sorted(providers)) or "none"
        raise SystemExit(f"provider {provider!r} not found in models.json (have: {known})")
    base_url = entry.get("baseUrl")
    api_key = entry.get("apiKey")
    if not base_url or not api_key:
        raise SystemExit(f"provider {provider!r} lacks baseUrl or apiKey")
    return str(base_url), str(api_key)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", default="openai", help="models.json provider name")
    parser.add_argument("--model", default="deepseek-v4.1-flash", help="model id")
    parser.add_argument("--expect", default="PONG", help="substring the reply must contain")
    parser.add_argument("--timeout", type=float, default=300.0, help="seconds")
    parser.add_argument(
        "--agent-dir",
        default=None,
        help="Pi agent dir (default: %%USERPROFILE%%/.pi/agent)",
    )
    args = parser.parse_args(argv)

    if args.agent_dir:
        agent_dir = Path(args.agent_dir)
    else:
        agent_dir = Path(os.environ["USERPROFILE"] if os.name == "nt" else Path.home()) / ".pi" / "agent"
    if not (agent_dir / "models.json").is_file():
        raise SystemExit(f"no models.json under {agent_dir}")

    base_url, api_key = _load_provider(agent_dir, args.provider)
    print(f"provider: {args.provider} | endpoint: {base_url} | model: {args.model}")

    work = Path(tempfile.mkdtemp(prefix="real-pi-smoke-"))
    (work / "agent.yaml").write_text(_AGENT_YAML, encoding="utf-8")
    for sub in ("cfg", "data", "pi-agent"):
        (work / sub).mkdir(parents=True, exist_ok=True)

    env = {k: v for k, v in os.environ.items() if k in _KEEP_ENV}
    env.update(
        OMNIGENT_CONFIG_HOME=str(work / "cfg"),
        OMNIGENT_DATA_DIR=str(work / "data"),
        OMNIGENT_AUTH_PROVIDER="header",
        OMNIGENT_LOCAL_SINGLE_USER="1",
        OMNIGENT_DISABLE_CATALOG_LOOKUP="1",
        OMNIGENT_DISABLE_TELEMETRY="1",
        # Keep the developer's global Pi packages out of the run: a broken
        # extension aborts Pi at startup for reasons unrelated to this check.
        PI_CODING_AGENT_DIR=str(work / "pi-agent"),
        OPENAI_BASE_URL=base_url,
        OPENAI_API_KEY=api_key,
        PYTHONUTF8="1",
        NO_PROXY="127.0.0.1,localhost",
    )
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)

    python = _REPO_ROOT / ".venv/Scripts/python.exe"
    if not python.is_file():
        python = _REPO_ROOT / ".venv/bin/python"
    if not python.is_file():
        python = Path(sys.executable)

    cmd = [
        str(python), "-m", "omnigent", "run", str(work / "agent.yaml"),
        "--model", args.model,
        "-p", f"Reply with exactly the word {args.expect} and nothing else.",
        "--no-session",
    ]
    print(f"cwd: {work}")
    result = subprocess.run(
        cmd,
        env=env,
        cwd=str(work),
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=args.timeout,
    )
    print(f"exit: {result.returncode}")
    print("--- stdout ---")
    print((result.stdout or "").strip()[:2000])
    if result.returncode != 0:
        print("--- stderr ---")
        print((result.stderr or "").strip()[-3000:])
        return 1
    if args.expect not in (result.stdout or ""):
        print(f"FAIL: reply did not contain {args.expect!r}")
        return 1
    print(f"OK: got {args.expect!r}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
