"""A mock backend for the MonkeyCode frontend.

The point is to see the UI, not to run MonkeyCode. Every endpoint answers with the
shallowest shape that keeps a page rendering, and every request is logged so the
next missing endpoint is visible instead of guessed.

Two response formats exist and mixing them up breaks a page silently:

  * most routes are wrapped in {code, message, data} -- the client checks
    code === 0 before acting, so a bare body reads as a failure;
  * the captcha routes are NOT wrapped -- the gocap library reads
    challenge/expires/token at the top level.

Run:
    python designs/go-rewrite/monkeycode-mock.py
Then point the frontend's vite dev server at it (TARGET=http://127.0.0.1:18999).
"""

from __future__ import annotations

import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

SEEN: dict[str, int] = {}

USER = {
    "id": "u_mock_1",
    "username": "demo",
    "name": "Demo User",
    "email": "demo@example.com",
    "avatar": "",
    "role": "user",
    "is_admin": True,
    "status": "active",
    "created_at": "2025-01-01T00:00:00Z",
}

TEAM = {
    "id": "t_mock_1",
    "name": "Demo Team",
    "description": "A mock team",
    "role": "owner",
    "created_at": "2025-01-01T00:00:00Z",
}

TASKS = [
    {
        "id": f"task_mock_{index}",
        "title": title,
        "status": status,
        "cli_name": "claude",
        "created_at": "2025-06-01T10:00:00Z",
        "updated_at": "2025-06-01T10:05:00Z",
        "repo_url": "https://github.com/example/repo",
        "branch": "main",
        "user_id": "u_mock_1",
    }
    for index, (title, status) in enumerate(
        [
            ("Refactor the auth middleware", "running"),
            ("Fix pagination on the task list", "completed"),
            ("Add unit tests for the parser", "failed"),
            ("Upgrade to React 19", "pending"),
        ],
        start=1,
    )
]

PROJECTS = [
    {
        "id": f"proj_mock_{index}",
        "name": name,
        "description": "",
        "repo_url": "https://github.com/example/repo",
        "created_at": "2025-05-01T00:00:00Z",
    }
    for index, name in enumerate(["monkeycode", "internal-tools"], start=1)
]

MODELS = [
    {
        "id": "m_mock_1",
        "name": "claude-sonnet-4",
        "provider": "Anthropic",
        "interface_type": "anthropic",
        "base_url": "https://api.anthropic.com",
        "model": "claude-sonnet-4-20250514",
        "is_default": True,
    },
    {
        "id": "m_mock_2",
        "name": "gpt-5",
        "provider": "OpenAI",
        "interface_type": "openai_chat",
        "base_url": "https://api.openai.com/v1",
        "model": "gpt-5",
        "is_default": False,
    },
]

HOSTS = [
    {
        "id": "host_mock_1",
        "name": "mock-host",
        "hostname": "mock-host",
        "status": "online",
        "os": "linux",
        "arch": "amd64",
        "cores": 8,
        "memory": 17179869184,
        "disk": 107374182400,
    }
]


def paginated(data: list) -> dict:
    return {"items": data, "total": len(data), "page": 1, "page_size": 50}


def resolve(method: str, path: str) -> tuple[object, bool]:
    """Return (payload, wrap_in_envelope)."""

    # ── captcha: read at the top level by the gocap library ─────────────────
    if path.endswith("/public/captcha/challenge"):
        return {
            "challenge": {"c": 1, "d": 1, "s": 1},
            "expires": int(time.time() * 1000) + 600_000,
            "token": "mock-captcha-token",
        }, False
    if path.endswith("/public/captcha/redeem"):
        return {
            "success": True,
            "token": "mock-captcha-verified",
            "expires": int(time.time() * 1000) + 600_000,
        }, False

    # ── identity ────────────────────────────────────────────────────────────
    if path.endswith("/users/password-login") or path.endswith("/teams/users/login"):
        return {
            "user": USER,
            "teams": [{"team": TEAM, "role": "owner", "user_id": USER["id"]}],
        }, True
    if path.endswith("/users/login"):
        return {"token": "mock-token", "user": USER}, True
    # /users/status drives the auth guard: the client treats a code===0 answer as
    # "logged in" and reads the user from it. Returning a paginated envelope here
    # left the login page thinking nobody was signed in, so it never navigated.
    if path.endswith("/users/status"):
        return {
            "user": USER,
            "teams": [{"team": TEAM, "role": "owner", "user_id": USER["id"]}],
        }, True
    if path.endswith("/users/current") or path.endswith("/users/profile"):
        return USER, True
    if path.endswith("/users/logout"):
        return {"ok": True}, True
    if path.endswith("/users/oidc/default-team"):
        return {"team": TEAM, "enabled": False}, True
    if path.endswith("/license/status"):
        return {"licensed": True, "edition": "offline", "machine_code": "MOCK"}, True
    if path.endswith("/license/machine-code"):
        return {"machine_code": "MOCK-MACHINE-CODE"}, True

    # ── team ────────────────────────────────────────────────────────────────
    if "/teams/dashboard" in path:
        return {
            "task_total": 4,
            "task_running": 1,
            "task_completed": 2,
            "task_failed": 1,
            "token_total": 123456,
            "cost_total": 1.23,
        }, True
    if path.endswith("/teams/current"):
        return TEAM, True
    if path.rstrip("/").endswith("/teams"):
        return paginated([TEAM]), True

    # ── task ────────────────────────────────────────────────────────────────
    if "/tasks" in path:
        if method == "POST":
            return TASKS[0], True
        if path.rstrip("/").split("/")[-1].startswith("task_"):
            return TASKS[0], True
        return paginated(TASKS), True

    # ── project / model / host / resources ──────────────────────────────────
    if "/projects" in path:
        return paginated(PROJECTS), True
    if "/models" in path:
        return paginated(MODELS), True
    if "/hosts" in path:
        return paginated(HOSTS), True
    # These are bare arrays, not paginated envelopes: the client calls .map on the
    # result, so returning {items: []} throws "skills.map is not a function" and
    # the console page renders blank with no visible error.
    if any(part in path for part in ("/skills", "/plugins", "/agent-resources")):
        return [], True
    if any(part in path for part in ("/images", "/apikeys", "/api-keys",
                                     "/git-identities", "/members")):
        return [], True

    # ── anything else: an empty page, which most list views accept ──────────
    return paginated([]), True


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        return

    def _send(self, payload, status=200, envelope=True):
        document = {"code": 0, "message": "ok", "data": payload} if envelope else payload
        body = json.dumps(document, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Access-Control-Allow-Headers", "*")
        self.send_header("Access-Control-Allow-Methods", "*")
        self.end_headers()
        self.wfile.write(body)

    def do_OPTIONS(self):
        self._send({})

    def do_GET(self):
        self.route("GET")

    def do_POST(self):
        self.route("POST")

    def do_PUT(self):
        self.route("PUT")

    def do_DELETE(self):
        self.route("DELETE")

    def do_PATCH(self):
        self.route("PATCH")

    def route(self, method: str) -> None:
        path = urlparse(self.path).path
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)

        key = f"{method} {path}"
        SEEN[key] = SEEN.get(key, 0) + 1

        payload, envelope = resolve(method, path)
        self._send(payload, envelope=envelope)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=18999)
    parser.add_argument("--report", default=None,
                        help="write the request tally here on exit")
    args = parser.parse_args()

    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"mock backend on http://127.0.0.1:{args.port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        if args.report:
            Path(args.report).write_text(
                json.dumps(SEEN, indent=2, sort_keys=True), encoding="utf-8"
            )
        print("\nrequests seen:")
        for key in sorted(SEEN):
            print(f"  {SEEN[key]:3}  {key}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
