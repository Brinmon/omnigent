"""Bisect which stored item type breaks the web client's render.

The fixture carries one row per item type. Something in it makes the client show
"Something went wrong", and it does so against the Python backend too -- so it is
a fixture problem, not a Go one. This narrows it to a type by writing a database
with only a message plus one candidate type and checking whether the session page
renders cleanly.
"""
import os
import sqlite3
import subprocess
import tempfile
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

# Name -> ITEM_TYPE code, from omnigent/db/enum_codecs.py.
TYPES = {
    "message": 1,
    "function_call": 2,
    "function_call_output": 3,
    "reasoning": 4,
    "error": 5,
    "compaction": 6,
    "native_tool": 7,
    "resource_event": 8,
    "routing_decision": 9,
    "slash_command": 10,
    "terminal_command": 11,
}


def renders_cleanly(allowed: list[str], port: int) -> tuple[bool, str]:
    """Build a database with only *allowed* item types and check the page."""
    workdir = Path(tempfile.mkdtemp(prefix="bisect-"))
    db = workdir / "d.db"
    db.write_bytes((REPO / "go/testdata/reference.db").read_bytes())

    connection = sqlite3.connect(db)
    main_id = connection.execute(
        "SELECT id FROM conversations WHERE parent_conversation_id IS NULL AND archived = 0"
    ).fetchone()[0]
    keep = ",".join(str(TYPES[name]) for name in allowed)
    connection.execute(
        f"DELETE FROM conversation_items WHERE conversation_id = ? AND type NOT IN ({keep})",
        (main_id,),
    )
    connection.commit()
    connection.close()

    env = dict(os.environ)
    env.update(
        PYTHONUTF8="1",
        NO_PROXY="127.0.0.1,localhost",
        OMNIGENT_PI_MODEL="openai/deepseek-v4.1-flash",
    )
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                  "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)

    binary = workdir / "omnigent-go.exe"
    subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/omnigent"],
        cwd=str(REPO / "go"), check=True, capture_output=True,
    )
    log = open(workdir / "s.log", "wb")
    process = subprocess.Popen(
        [str(binary), "serve", "--port", str(port), "--database-uri", f"sqlite:///{db}"],
        env=env, cwd=str(REPO), stdout=log, stderr=subprocess.STDOUT,
    )

    import httpx
    from playwright.sync_api import sync_playwright

    base = f"http://127.0.0.1:{port}"
    try:
        for _ in range(80):
            try:
                if httpx.get(f"{base}/health", timeout=2).status_code < 600:
                    break
            except Exception:
                pass
            time.sleep(0.3)
        session_id = httpx.get(f"{base}/v1/sessions", timeout=10).json()["data"][0]["id"]

        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(channel="msedge", headless=True)
            page = browser.new_page()
            # Collect the client's own error text, which names the failing field.
            errors: list[str] = []
            page.on("console", lambda msg: errors.append(msg.text) if msg.type == "error" else None)
            page.goto(f"{base}/c/{session_id}", wait_until="domcontentloaded")
            time.sleep(4)
            body = page.inner_text("body")
            browser.close()

        broken = "Something went wrong" in body
        detail = "" if not broken else " | ".join(errors[:2])[:200]
        return (not broken), detail
    finally:
        process.terminate()
        try:
            process.wait(timeout=6)
        except Exception:
            process.kill()
        log.close()


def main() -> int:
    ok, _ = renders_cleanly(["message"], 18110)
    print(f"{'message only':30} -> {'OK' if ok else 'BROKEN'}", flush=True)

    broken_types: list[str] = []
    for index, name in enumerate(TYPES):
        if name == "message":
            continue
        ok, detail = renders_cleanly(["message", name], 18120 + index)
        print(f"{'message + ' + name:30} -> {'OK' if ok else 'BROKEN'}  {detail}", flush=True)
        if not ok:
            broken_types.append(name)

    print()
    print("broken types:", broken_types or "none")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
