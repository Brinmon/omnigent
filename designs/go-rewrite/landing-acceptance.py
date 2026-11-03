"""F1-F9: the landing page's new-session path, end to end, in a browser.

This is the automated equivalent of the manual acceptance a person would do by
hand: open the page, type a prompt, press send, read the reply. It runs against a
server the caller already started, so it can be pointed at a real home directory.

Every step corresponds to something a user can see or do. Nothing asserts an
internal API shape -- other scripts do that -- because the question here is
whether a person can start a session.

Usage:
    python designs/go-rewrite/landing-acceptance.py
    python designs/go-rewrite/landing-acceptance.py --base-url http://127.0.0.1:18080
    python designs/go-rewrite/landing-acceptance.py --headed
"""

from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

# The word the agent is told to answer with. Chosen so the check is a substring
# test that cannot be satisfied by the prompt echoing back.
MARKER = "MARKERZULU"

PROMPT = f"Reply with exactly the word {MARKER}."


def api(base_url: str, path: str) -> object:
    with urllib.request.urlopen(base_url + path, timeout=30) as response:
        return json.load(response)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:18080")
    parser.add_argument("--channel", default="msedge")
    parser.add_argument("--headed", action="store_true")
    parser.add_argument("--timeout", type=int, default=120)
    args = parser.parse_args(argv)

    from playwright.sync_api import sync_playwright

    results: list[tuple[str, bool, str]] = []
    failures: list[str] = []
    failed_requests: list[str] = []

    with sync_playwright() as playwright:
        browser = playwright.chromium.launch(
            channel=args.channel, headless=not args.headed
        )
        page = browser.new_page()
        page.set_default_timeout(args.timeout * 1000)

        # Only 4xx/5xx are recorded; a redirect or a cancelled request is normal
        # during navigation and would make the console check meaningless.
        page.on(
            "response",
            lambda r: failed_requests.append(
                f"{r.status} {r.request.method} {r.url.split(args.base_url, 1)[-1].split('?')[0]}"
            )
            if r.status >= 400
            else None,
        )
        console_errors: list[str] = []
        page.on(
            "console",
            lambda m: console_errors.append(m.text[:160])
            if m.type == "error"
            else None,
        )

        def step(name: str, action) -> bool:
            try:
                detail = action()
            except Exception as exc:  # a failing step is a data point
                results.append((name, False, f"{type(exc).__name__}: {exc}"[:200]))
                failures.append(name)
                return False
            results.append((name, True, detail or ""))
            return True

        def f1_lands_without_host_warning() -> str:
            page.goto(args.base_url, wait_until="domcontentloaded")
            page.wait_for_selector(
                "[data-testid='new-chat-landing-input']", timeout=args.timeout * 1000
            )
            # The host chip and the picker settle asynchronously; asserting
            # immediately would race the first paint.
            page.wait_for_selector(
                "[data-testid='new-chat-landing-host-chip']", timeout=30_000
            )
            time.sleep(2)
            body = page.inner_text("body")
            if "No host selected" in body:
                raise AssertionError("the page reports no execution target")
            return "a host is selected"

        def f2_agent_is_available() -> str:
            picker = page.locator("[data-testid='new-chat-landing-agent-select']")
            if picker.count() == 0:
                raise AssertionError("no agent picker rendered")
            label = picker.first.inner_text().strip()
            if not label:
                raise AssertionError("the agent picker is empty")
            # The list is fed by GET /v1/agents; an empty response shows a
            # placeholder instead of a name.
            return f"agent: {label.splitlines()[0][:40]}"

        def f2b_workspace_picker_lists_directories() -> str:
            # The picker is how a user chooses where the agent runs. It failed
            # with "C:/Users/me doesn't exist on this host" while the route took
            # an absolute path and the generated single-segment {path} could not
            # match it -- so the listing was empty for every Windows user.
            page.locator("[data-testid='new-chat-landing-workspace-chip']").click()
            page.wait_for_selector(
                "[data-testid='new-chat-landing-workspace-open-folder']", timeout=20_000
            )
            page.locator("[data-testid='new-chat-landing-workspace-open-folder']").click()
            dialog = page.locator("[role='dialog']").first
            dialog.wait_for(timeout=20_000)
            time.sleep(3)

            text = dialog.inner_text()
            if "doesn't exist" in text:
                raise AssertionError(
                    "the picker reports the home directory as missing"
                )
            # A listing shows entries beside the Cancel/Confirm pair; a failed one
            # shows only the heading and the buttons.
            entries = [
                line for line in text.splitlines()
                if line.strip() and line.strip() not in
                {"Select working directory", "Choose a folder for the new session.",
                 "Cancel", "Confirm"}
            ]
            if len(entries) < 3:
                raise AssertionError(
                    f"the picker listed {len(entries)} entries: {entries[:5]}"
                )
            page.keyboard.press("Escape")
            time.sleep(1)
            return f"{len(entries)} entries listed"

        def f3_send_enables_after_typing() -> str:
            box = page.locator("[data-testid='new-chat-landing-input']")
            box.fill(PROMPT)
            send = page.locator("button[aria-label='Start session']").first
            # React needs a tick to re-render the button's disabled state.
            for _ in range(40):
                if send.count() and not send.is_disabled():
                    return "send is enabled"
                time.sleep(0.25)
            raise AssertionError(
                "the send button stayed disabled after typing a prompt"
            )

        created: dict[str, str] = {}

        def f4_send_starts_a_session() -> str:
            before = page.url
            page.locator("button[aria-label='Start session']").first.click()
            page.wait_for_url("**/c/**", timeout=args.timeout * 1000)

            # The client navigates to an optimistic `temp:` id first and replaces
            # it once the create returns, so the first URL after navigation is not
            # the session's real id. Waiting for the path to stop changing is what
            # makes this assertion about the session rather than about the
            # placeholder.
            # page.url lags the router during the create; the path settles to the
            # real id a moment after the optimistic one. Polling the URL is what
            # catches that, and waiting on a selector is not: the page renders
            # for the temp id too, so a selector would pass either way.
            deadline = time.monotonic() + args.timeout
            session_id = ""
            while time.monotonic() < deadline:
                session_id = page.url.rsplit("/c/", 1)[-1].split("?")[0]
                if session_id and not session_id.startswith("temp:"):
                    break
                page.wait_for_timeout(400)
            else:
                raise AssertionError(
                    f"the URL stayed on the optimistic id {session_id[:16]!r}"
                )
            if session_id == before:
                raise AssertionError("the URL did not change to a session")
            created["id"] = session_id
            return f"session {session_id[:8]}"

        def f5_prompt_is_rendered() -> str:
            # The bubble is drawn from the stored row, so this also proves the
            # create wrote something the transcript read can find.
            page.wait_for_selector(f"text={PROMPT[:24]}", timeout=args.timeout * 1000)
            return "the prompt is visible"

        def f6_reply_arrives() -> str:
            deadline = time.monotonic() + args.timeout
            while time.monotonic() < deadline:
                body = page.inner_text("body")
                # The prompt contains the marker, so it must be excluded or the
                # check would pass on the user's own message.
                if MARKER in body.replace(PROMPT, ""):
                    return "the assistant replied"
                time.sleep(1.5)
            raise AssertionError("no reply arrived")

        def f7_reload_keeps_history() -> str:
            page.reload(wait_until="domcontentloaded")
            page.wait_for_selector(f"text={PROMPT[:24]}", timeout=args.timeout * 1000)
            body = page.inner_text("body")
            if MARKER not in body.replace(PROMPT, ""):
                raise AssertionError("the reply did not survive a reload")
            return "history survived a reload"

        def f8_landing_console_is_clean() -> str:
            failed_requests.clear()
            console_errors.clear()
            page.goto(args.base_url, wait_until="domcontentloaded")
            page.wait_for_selector(
                "[data-testid='new-chat-landing-input']", timeout=30_000
            )
            time.sleep(4)
            if failed_requests:
                unique = sorted(set(failed_requests))[:5]
                raise AssertionError(f"{len(set(failed_requests))} failed: {unique}")
            return "no failed requests on the landing page"

        def f9_session_page_has_no_501() -> str:
            failed_requests.clear()
            page.goto(f"{args.base_url}/c/{created['id']}", wait_until="domcontentloaded")
            time.sleep(6)
            not_implemented = [
                f for f in set(failed_requests) if f.startswith("501")
            ]
            if not_implemented:
                raise AssertionError(f"still 501: {sorted(not_implemented)[:6]}")
            return "no 501 on the session page"

        if step("F1 host selected", f1_lands_without_host_warning):
            if step("F2 agent available", f2_agent_is_available):
                if step("F2b workspace picker", f2b_workspace_picker_lists_directories):
                    if step("F3 send enables", f3_send_enables_after_typing):
                        if step("F4 session starts", f4_send_starts_a_session):
                            if step("F5 prompt shown", f5_prompt_is_rendered):
                                if step("F6 reply arrives", f6_reply_arrives):
                                    step("F7 reload keeps history", f7_reload_keeps_history)
        step("F8 landing console clean", f8_landing_console_is_clean)
        if created.get("id"):
            step("F9 session page clean", f9_session_page_has_no_501)

        browser.close()

    print(f"\n{'step':30} {'result':7} detail")
    print("-" * 96)
    for name, ok, detail in results:
        print(f"{name:30} {'PASS' if ok else 'FAIL':7} {detail}")

    if failures:
        print(f"\nFAIL: {len(failures)} step(s) failed")
        return 1
    print("\nOK: the landing page starts a session, replies, and survives a reload")
    return 0


if __name__ == "__main__":
    sys.exit(main())
