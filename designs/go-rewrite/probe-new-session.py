"""Watch what the web client does when asked to start a new session.

The acceptance script reported a timeout waiting for /c/**, which says the client
never treated the create as successful. This records the requests it actually
made and the console errors it logged, so the cause is observed rather than
inferred from reading the client source.
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

REPO = Path(__file__).resolve().parents[2]


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def clean_env() -> dict[str, str]:
    env = dict(os.environ)
    env.update(
        PYTHONUTF8="1",
        NO_PROXY="127.0.0.1,localhost",
        OMNIGENT_PI_MODEL=env.get("OMNIGENT_PI_MODEL", "openai/deepseek-v4.1-flash"),
    )
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                  "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)
    return env


def main() -> int:
    workdir = Path(tempfile.mkdtemp(prefix="probe-create-"))
    db = workdir / "p.db"
    db.write_bytes((REPO / "go/testdata/reference.db").read_bytes())
    binary = workdir / ("omnigent-go.exe" if os.name == "nt" else "omnigent-go")
    build = subprocess.run(["go", "build", "-o", str(binary), "./cmd/omnigent"],
                           cwd=str(REPO / "go"), capture_output=True, text=True,
                           encoding="utf-8", errors="replace")
    if build.returncode != 0:
        print(build.stderr[-2000:])
        return 1

    port = free_port()
    log = open(workdir / "server.log", "wb")
    process = subprocess.Popen(
        [str(binary), "serve", "--port", str(port),
         "--database-uri", f"sqlite:///{db}",
         "--artifact-location", str(workdir / "artifacts")],
        env=clean_env(), cwd=str(REPO), stdout=log, stderr=subprocess.STDOUT,
    )
    base = f"http://127.0.0.1:{port}"

    import httpx

    for _ in range(120):
        try:
            if httpx.get(f"{base}/health", timeout=2).status_code < 600:
                break
        except Exception:
            pass
        time.sleep(0.3)

    try:
        from playwright.sync_api import sync_playwright

        with sync_playwright() as pw:
            browser = pw.chromium.launch(channel="msedge", headless=True)
            page = browser.new_page()
            page.set_default_timeout(60_000)

            requests: list[dict] = []
            page.on("request", lambda r: requests.append({
                "method": r.method, "url": r.url,
                "post": (r.post_data or "")[:300],
                "content_type": r.headers.get("content-type", ""),
            }))
            console: list[str] = []
            page.on("console", lambda m: console.append(f"{m.type}: {m.text}"[:300]))
            failed: list[str] = []
            page.on("requestfailed", lambda r: failed.append(
                f"{r.method} {r.url} -> {r.failure}"))

            page.goto(base, wait_until="domcontentloaded")
            page.wait_for_selector("aside, nav, [role='complementary']", timeout=30_000)
            time.sleep(2)

            print("=== landing page text (first 700 chars) ===")
            print(page.inner_text("body")[:700])

            print("\n=== interactable elements ===")
            for selector in ("textarea", "button", "[contenteditable='true']",
                             "[data-testid]"):
                nodes = page.locator(selector)
                count = nodes.count()
                print(f"{selector}: {count}")
                for index in range(min(count, 8)):
                    node = nodes.nth(index)
                    try:
                        label = (node.get_attribute("data-testid")
                                 or node.get_attribute("aria-label")
                                 or (node.inner_text() or "")[:40])
                        print(f"   [{index}] {label!r} visible={node.is_visible()}")
                    except Exception as exc:
                        print(f"   [{index}] <{type(exc).__name__}>")

            print("\n=== typing a prompt into the landing box ===")
            box = page.locator("textarea:visible, [contenteditable='true']:visible").first
            if box.count() == 0:
                print("no landing input found")
            else:
                box.click()
                page.keyboard.type("Reply with exactly the word PONG.")
                page.keyboard.press("Enter")
                time.sleep(12)
                print("url now:", page.url)
                print("body text (first 500):", page.inner_text("body")[:500])

            print("\n=== API requests the client made ===")
            for entry in requests:
                if "/v1/" not in entry["url"] and not entry["url"].endswith("/health"):
                    continue
                path = entry["url"].split(str(port), 1)[-1]
                print(f"{entry['method']:6} {path[:90]:92} {entry['content_type'][:40]}")
                if entry["post"]:
                    print(f"       body: {entry['post']}")

            if failed:
                print("\n=== failed requests ===")
                for line in failed[:10]:
                    print(" ", line)
            if console:
                print("\n=== console (errors and warnings) ===")
                for line in console[:20]:
                    if line.startswith(("error", "warning")):
                        print(" ", line)
            browser.close()
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except Exception:
            process.kill()
        log.close()
        print(f"\nserver log: {workdir / 'server.log'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
