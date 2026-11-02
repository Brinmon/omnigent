"""Probe the real ``pi --mode rpc`` protocol.

P3 must speak this protocol exactly: the JSONL commands in, the event stream
out, and the turn boundary. Rather than infer it from the Python source (which
has to cope with several Pi versions), this drives one turn and records what
actually crosses the pipe.

Usage:
    python designs/go-rewrite/probe-pi-rpc.py
    python designs/go-rewrite/probe-pi-rpc.py --model deepseek-v4.1-flash
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import threading
import time
from pathlib import Path

_REPO_ROOT = Path(__file__).resolve().parents[2]


def pi_command() -> list[str]:
    """Return the argv that runs Pi's CLI.

    On Windows ``pi`` resolves to an npm ``.cmd`` shim, and a ``.cmd`` cannot be
    driven over a pipe: writing to its stdin fails with "Invalid argument"
    because cmd.exe does not forward the handle. The managed install also exposes
    the real entry point, so prefer it and keep the shim only as a fallback for a
    global ``pi`` that is already a native binary.
    """
    import json as _json
    import shutil

    agent_dir = Path(os.environ.get("USERPROFILE", "")) / ".pi" / "agent"
    version_file = agent_dir / "install" / "current-version"
    if version_file.is_file():
        version = version_file.read_text(encoding="utf-8").strip()
        package_dir = (
            agent_dir / "install" / "releases" / version
            / "node_modules" / "@earendil-works" / "pi-coding-agent"
        )
        package_json = package_dir / "package.json"
        if package_json.is_file():
            metadata = _json.loads(package_json.read_text(encoding="utf-8"))
            bin_field = metadata.get("bin")
            rel = bin_field if isinstance(bin_field, str) else bin_field.get("pi")
            if rel:
                return ["node", str(package_dir / rel)]

    found = shutil.which("pi")
    if found:
        return [found]
    raise SystemExit("no pi entry point found")


def load_provider() -> tuple[str, str]:
    models = Path(os.environ["USERPROFILE"]) / ".pi" / "agent" / "models.json"
    data = json.loads(models.read_text(encoding="utf-8"))
    provider = data["providers"]["openai"]
    return provider["baseUrl"], provider["apiKey"]


# The environment Pi needs. A stripped env breaks it (no SYSTEMROOT means Winsock
# init fails); inheriting everything lets a developer's global Pi packages load,
# which is a different failure. This is the whitelist the Python executor uses.
_KEEP = {
    "PATH", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
    "TEMP", "TMP", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA",
    "LOCALAPPDATA", "PROGRAMDATA", "PROGRAMFILES", "PROGRAMFILES(X86)",
    "PROGRAMW6432", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
    "USERNAME",
}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", default="deepseek-v4.1-flash")
    parser.add_argument("--prompt", default="Reply with exactly the word PONG.")
    parser.add_argument("--timeout", type=float, default=180.0)
    args = parser.parse_args()

    base_url, api_key = load_provider()
    pi = pi_command()
    workdir = Path(__file__).parent / ".pi-rpc-probe"
    workdir.mkdir(exist_ok=True)
    agent_dir = workdir / "pi-agent"
    agent_dir.mkdir(exist_ok=True)
    # Pi resolves model ids against PI_CODING_AGENT_DIR/models.json, so an
    # isolated dir without that file makes every model unknown even though the
    # developer's install defines it. Copy the provider registry in; the
    # isolation is about excluding global *packages*, not the model list.
    source_models = Path(os.environ["USERPROFILE"]) / ".pi" / "agent" / "models.json"
    if source_models.is_file():
        (agent_dir / "models.json").write_bytes(source_models.read_bytes())

    env = {k: v for k, v in os.environ.items() if k in _KEEP}
    env.update(
        PYTHONUTF8="1",
        NO_PROXY="127.0.0.1,localhost",
        PI_CODING_AGENT_DIR=str(agent_dir),
        OPENAI_BASE_URL=base_url,
        OPENAI_API_KEY=api_key,
    )
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                  "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)

    model_ref = args.model if "/" in args.model else f"openai/{args.model}"
    cmd = [*pi, "--mode", "rpc", "--no-session", "--model", model_ref]
    print(f"spawn: {' '.join(cmd)}")
    proc = subprocess.Popen(
        cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        cwd=str(workdir), env=env, text=True, encoding="utf-8", errors="replace",
    )

    transcript: list[dict] = []

    def read_stdout() -> None:
        assert proc.stdout is not None
        for line in proc.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                event = json.loads(line)
            except ValueError:
                transcript.append({"unparsed": line})
                continue
            transcript.append(event)

    def read_stderr() -> None:
        assert proc.stderr is not None
        for line in proc.stderr:
            line = line.strip()
            if line:
                transcript.append({"stderr": line})

    stdout_thread = threading.Thread(target=read_stdout, daemon=True)
    stderr_thread = threading.Thread(target=read_stderr, daemon=True)
    stdout_thread.start()
    stderr_thread.start()

    # Give Pi a moment to install its RPC loop, then send one prompt.
    time.sleep(2.0)
    command = {"type": "prompt", "message": args.prompt}
    print(f"stdin: {json.dumps(command)}")
    assert proc.stdin is not None
    proc.stdin.write(json.dumps(command) + "\n")
    proc.stdin.flush()

    deadline = time.monotonic() + args.timeout
    settled = False
    while time.monotonic() < deadline:
        kinds = [event.get("type") for event in transcript]
        if "agent_settled" in kinds:
            settled = True
            break
        if "agent_end" in kinds and time.monotonic() > deadline - args.timeout + 30:
            # agent_end is not the turn boundary; keep waiting briefly for
            # agent_settled to confirm.
            pass
        time.sleep(0.2)

    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
    stdout_thread.join(timeout=2)
    stderr_thread.join(timeout=2)

    out = Path(__file__).with_name("pi_rpc_reference.json")
    out.write_text(json.dumps(transcript, indent=2) + "\n", encoding="utf-8")

    print(f"\nsettled: {settled}  events: {len(transcript)}  wrote {out.name}")
    seen: dict[str, int] = {}
    for event in transcript:
        name = event.get("type") or next(iter(event), "?")
        seen[name] = seen.get(name, 0) + 1
    for name in sorted(seen):
        print(f"  {name:32} x{seen[name]}")
    print("\nfirst 3 events:")
    for event in transcript[:3]:
        print("  " + json.dumps(event)[:200])
    return 0 if settled else 1


if __name__ == "__main__":
    sys.exit(main())
