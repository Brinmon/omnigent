#!/usr/bin/env python
"""Generate the reference SQLite database that pins Omnigent's on-disk encodings.

The Go rewrite must read databases that the Python implementation already
wrote, so A5/A6 compare against real rows rather than a hand-written guess.
Every value here goes through the production encoders --

* ``Uuid16``      -- 32-char hex on the entity side, 16 raw bytes in the column
* ``enum_codecs`` -- string names on the entity side, stable SMALLINT codes
* ``CompressedText`` -- sentinel + codec byte + payload, zstd when it pays

-- which means a change to any of them fails this fixture instead of silently
producing databases Go cannot read.

The fixture deliberately covers the awkward cases:

* every enum value, so a missing/renumbered code shows up;
* a short string (raw codec) and a long JSON blob (zstd codec), so both
  framed variants are exercised;
* legacy unframed plaintext and legacy ``str`` rows, which SQLite's dynamic
  typing still returns from pre-migration databases;
* a parent/child conversation pair and a forked conversation, so joins and
  the ``parent_conversation_id`` encoding are covered.

Usage::

    python designs/go-rewrite/make-reference-db.py        # -> go/testdata/reference.db
    python designs/go-rewrite/make-reference-db.py --out /tmp/x.db
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sqlite3
import sys
import tempfile
import uuid
from pathlib import Path

_REPO_ROOT = Path(__file__).resolve().parents[2]
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))

_DEFAULT_OUT = _REPO_ROOT / "go" / "testdata" / "reference.db"

# Fixed ids so the Go tests can name rows without copying them out of the
# fixture. Derived from a hash rather than uuid4 so regeneration is stable.
def _stable_uuid(name: str) -> str:
    return hashlib.sha256(name.encode()).hexdigest()[:32]


SEED = {
    "main": _stable_uuid("conversation/main"),
    "child": _stable_uuid("conversation/child"),
    "fork": _stable_uuid("conversation/fork"),
    "agent": _stable_uuid("agent/hello"),
}
ITEM_IDS = {f"item/{i}": _stable_uuid(f"item/{i}") for i in range(1, 13)}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", default=str(_DEFAULT_OUT), help="output .db path")
    args = parser.parse_args(argv)

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    # Build in a temp dir first: a failed run must not leave a half-written
    # fixture that later tests would trust.
    staging = Path(tempfile.mkdtemp(prefix="refdb-")) / "reference.db"

    from sqlalchemy.orm import Session

    from omnigent.db import db_models as m
    from omnigent.db.compression import encode as compress_encode
    from omnigent.db.enum_codecs import (
        ACCOUNT_TOKEN_KIND,
        AGENT_KIND,
        COMMENT_STATUS,
        CONVERSATION_KIND,
        DEVICE_GRANT_STATUS,
        HOST_STATUS,
        ITEM_STATUS,
        ITEM_TYPE,
        POLICY_SCOPE,
        POLICY_TYPE,
        SCHEDULED_TASK_EXECUTION_TARGET,
        SCHEDULED_TASK_RUN_STATUS,
        SCHEDULED_TASK_SESSION_MODE,
        SCHEDULED_TASK_STATE,
        SESSION_LIVE_STATUS,
    )
    from omnigent.db.utils import get_or_create_engine

    uri = f"sqlite:///{staging}"
    engine = get_or_create_engine(uri)

    now = 1_760_000_000
    long_json = json.dumps(
        {"messages": [{"role": "user", "content": "x" * 400}], "note": "zstd frame"},
        separators=(",", ":"),
    )
    assert len(long_json.encode()) > 64, "must exceed the raw-codec threshold"

    with Session(engine) as session:
        # ── agents: one per kind, one with a description (CompressedText) ──
        for name, kind in (("agent/server", "server"), ("agent/user", "user")):
            session.add(
                m.SqlAgent(
                    workspace_id=0,
                    id=_stable_uuid(name),
                    created_at=now,
                    name=name.replace("/", "-"),
                    bundle_location="bundles/hello.tar.gz",
                    version=1,
                    kind=AGENT_KIND[kind],
                    description=long_json if kind == "server" else "short",
                    updated_at=now,
                    created_by=None if kind == "server" else "fixture-user",
                )
            )

        # ── conversations: main, a sub-agent child, and a fork ──
        session.add_all([
            m.SqlConversation(
                workspace_id=0,
                id=SEED["main"],
                created_at=now,
                updated_at=now,
                title="Main conversation",
                root_conversation_id=SEED["main"],
                next_position=100,
                agent_id=SEED["agent"],
                archived=False,
                session_overrides=None,
            ),
            m.SqlConversation(
                workspace_id=0,
                id=SEED["child"],
                created_at=now + 1,
                updated_at=now + 1,
                title="Sub-agents",           # exercises the kind code below
                parent_conversation_id=SEED["main"],
                root_conversation_id=SEED["main"],
                next_position=0,
                archived=False,
            ),
            m.SqlConversation(
                workspace_id=0,
                id=SEED["fork"],
                created_at=now + 2,
                updated_at=now + 2,
                title="Fork of main",
                parent_conversation_id=SEED["main"],
                root_conversation_id=SEED["main"],
                next_position=0,
                archived=True,
            ),
        ])

        # ── items: one row per ITEM_TYPE code, plus per ITEM_STATUS code ──
        position = 1
        item_keys = sorted(ITEM_TYPE)
        statuses = sorted(ITEM_STATUS)
        for index, item_type in enumerate(item_keys):
            status = statuses[index % len(statuses)]
            session.add(
                m.SqlConversationItem(
                    workspace_id=0,
                    conversation_id=SEED["main"],
                    id=_stable_uuid(f"item/{item_type}"),
                    response_id=f"resp-{index}",
                    created_at=now + index,
                    status=ITEM_STATUS[status],
                    position=position,
                    type=ITEM_TYPE[item_type],
                    data=long_json if index % 2 == 0 else "{}",
                    search_text=f"{item_type} {status}",
                    created_by="fixture-user",
                )
            )
            position += 1
        for offset, status in enumerate(statuses, start=100):
            item_type = item_keys[offset % len(item_keys)]
            session.add(
                m.SqlConversationItem(
                    workspace_id=0,
                    conversation_id=SEED["child"],
                    id=_stable_uuid(f"item/status/{status}"),
                    response_id=f"resp-s{offset}",
                    created_at=now + offset,
                    status=ITEM_STATUS[status],
                    position=offset,
                    type=ITEM_TYPE[item_type],
                    data="{}",
                    search_text=status,
                    created_by=None,
                )
            )

        session.commit()

    # Drop the pooled connections so the WAL is checkpointed into the main
    # file before it is copied: reading only ``reference.db`` would otherwise
    # miss rows still sitting in ``-wal``.
    engine.dispose()

    # ── Raw-SQL rows that the ORM cannot express ──────────────────────────
    # The three legacy/framed variants must be written as bytes directly:
    # the ORM would normalize them back into its own canonical form.
    conn = sqlite3.connect(staging)
    try:
        # A) framed + zstd. ``agents.description`` is a real CompressedText
        # column and the ORM wrote the long value through it, so this asserts
        # the production frame shape rather than re-creating it here.
        row = conn.execute(
            "SELECT description FROM agents WHERE kind = ?", (AGENT_KIND["server"],)
        ).fetchone()
        assert row and row[0][:2] == b"\x00\x01", f"expected a zstd frame, got {row[0][:4]!r}"

        # B) framed + raw: below the compression threshold, so framed but plain
        row = conn.execute(
            "SELECT description FROM agents WHERE kind = ?", (AGENT_KIND["user"],)
        ).fetchone()
        assert row and row[0][:2] == b"\x00\x00", f"expected a raw frame, got {row[0][:4]!r}"

        raw_short = b"\x00\x00" + "tiny payload".encode()
        assert raw_short == compress_encode("tiny payload")

        # C) legacy unframed plaintext stored as TEXT (pre-migration rows)
        legacy_text = '{"legacy": true, "reason": "written before compression"}'

        # D) legacy unframed plaintext stored as BLOB
        legacy_blob = b'{"legacy": "blob"}'

        conn.execute(
            "INSERT INTO conversation_items "
            "(workspace_id, conversation_id, id, response_id, created_at, status,"
            " position, type, data, search_text, created_by) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (
                0, bytes.fromhex(SEED["main"]), bytes.fromhex(_stable_uuid("item/legacy")),
                "resp-legacy", now + 500, ITEM_STATUS["completed"], 500,
                ITEM_TYPE["message"], legacy_text, "legacy", None,
            ),
        )
        conn.execute(
            "INSERT INTO conversation_items "
            "(workspace_id, conversation_id, id, response_id, created_at, status,"
            " position, type, data, search_text, created_by) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (
                0, bytes.fromhex(SEED["main"]), bytes.fromhex(_stable_uuid("item/legacy-blob")),
                "resp-legacy-blob", now + 501, ITEM_STATUS["completed"], 501,
                ITEM_TYPE["message"], legacy_blob, "legacy-blob", None,
            ),
        )
        # E) a raw-codec framed row, written as bytes like production would
        conn.execute(
            "INSERT INTO conversation_items "
            "(workspace_id, conversation_id, id, response_id, created_at, status,"
            " position, type, data, search_text, created_by) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?)",
            (
                0, bytes.fromhex(SEED["main"]), bytes.fromhex(_stable_uuid("item/rawframe")),
                "resp-rawframe", now + 502, ITEM_STATUS["completed"], 502,
                ITEM_TYPE["reasoning"], raw_short, "rawframe", None,
            ),
        )
        conn.commit()
    finally:
        conn.close()

    # Copy into place only after the whole fixture is built.
    out.write_bytes(staging.read_bytes())

    # ── Emit the enum tables the Go side must reproduce byte-for-byte ─────
    tables = {
        "conversation_kind": CONVERSATION_KIND,
        "item_type": ITEM_TYPE,
        "item_status": ITEM_STATUS,
        "comment_status": COMMENT_STATUS,
        "session_live_status": SESSION_LIVE_STATUS,
        "account_token_kind": ACCOUNT_TOKEN_KIND,
        "policy_type": POLICY_TYPE,
        "policy_scope": POLICY_SCOPE,
        "device_grant_status": DEVICE_GRANT_STATUS,
        "host_status": HOST_STATUS,
        "agent_kind": AGENT_KIND,
        "scheduled_task_state": SCHEDULED_TASK_STATE,
        "scheduled_task_execution_target": SCHEDULED_TASK_EXECUTION_TARGET,
        "scheduled_task_session_mode": SCHEDULED_TASK_SESSION_MODE,
        "scheduled_task_run_status": SCHEDULED_TASK_RUN_STATUS,
    }
    enum_out = out.parent / "enum_codes.json"
    enum_out.write_text(json.dumps(tables, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    # ── Emit the ids so Go tests do not have to hardcode hashes ──────────
    ids_out = out.parent / "reference_ids.json"
    ids_out.write_text(
        json.dumps({"conversations": SEED, "items": ITEM_IDS}, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    size = out.stat().st_size
    print(f"wrote {out} ({size} bytes)")
    print(f"wrote {enum_out} ({len(tables)} tables, {sum(len(t) for t in tables.values())} codes)")
    print(f"wrote {ids_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
