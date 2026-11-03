"""Find what keeps the landing page's "Start session" button disabled.

The acceptance run waits on it and times out, which says nothing about why. This
types a prompt, then reports the button's disabled state plus the agent selection
the page shows, so the condition is observed instead of guessed.
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
    env.update(PYTHONUTF8="1", NO_PROXY="127.0.0.1,localhost",
               OMNIGENT_PI_MODEL=env.get("OMNIGENT_PI_MODEL", "openai/deepseek-v4.1-flash"))
    for stale in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                  "http_proxy", "https_proxy", "all_proxy"):
        env.pop(stale, None)
    return env


def main() -> int:
    workdir = Path(tempfile.mkdtemp(prefix="probe-start-"))
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
            page.set_default_timeout(45_000)

            requests: list[str] = []
            page.on("request", lambda r: requests.append(f"{r.method} {r.url}") if "/v1/" in r.url else None)

            page.goto(base, wait_until="domcontentloaded")
            page.wait_for_selector("[data-testid='new-chat-landing-input']", timeout=30_000)
            time.sleep(2)

            def button_state(label: str) -> None:
                button = page.locator("[aria-label='Start session']")
                if button.count() == 0:
                    print(f"{label}: no Start session button")
                    return
                node = button.first
                print(f"{label}: disabled={node.is_disabled()} "
                      f"aria-busy={node.get_attribute('aria-busy')} "
                      f"class={(node.get_attribute('class') or '')[:70]}")

            button_state("initial")

            landing = page.locator("[data-testid='new-chat-landing-input']")
            landing.fill("Reply with exactly the word PONG.")
            time.sleep(1.5)
            button_state("after fill")

            landing.click()
            page.keyboard.type("x")
            time.sleep(1)
            page.keyboard.press("Backspace")
            time.sleep(1.5)
            button_state("after keyboard edit")

            print("\nlanding text:")
            print(page.inner_text("body")[:600])

            print("\nagent-related testids on the page:")
            for selector in page.locator("[data-testid]").all():
                try:
                    name = selector.get_attribute("data-testid") or ""
                except Exception:
                    continue
                if any(word in name for word in ("agent", "model", "harness", "start", "session")):
                    print(f"  {name!r} visible={selector.is_visible()} "
                          f"text={(selector.inner_text() or '')[:40]!r}")

            print("\nagent API responses:")
            for path in ("/v1/agents", "/v1/agents?scope=user&limit=50"):
                try:
                    body = httpx.get(base + path, timeout=20).json()
                    print(f"  {path}: {len(body.get('data') or [])} agents")
                except Exception as exc:
                    print(f"  {path}: {type(exc).__name__}")

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
