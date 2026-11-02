#!/usr/bin/env python
"""Verify the Go build writes CompressedText frames Python can read.

The Go test ``TestWritesAreReadableByPython`` emits a probe file under
``OMNIGENT_INTEROP_OUT``; this script decodes it with the *production* Python
decoder, so a framing change that only the Go tests would miss is caught here.

Run both halves:

    cd go && OMNIGENT_INTEROP_OUT=../.interop-probe.bin go test ./internal/db/ -run Interop
    python designs/go-rewrite/verify-go-interop.py ../.interop-probe.bin

Or just run this script with no arguments: it invokes the Go test itself and
then checks the result, which is what CI should do.
"""

from __future__ import annotations

import os
import struct
import subprocess
import sys
import tempfile
from pathlib import Path

_REPO_ROOT = Path(__file__).resolve().parents[2]
_GO_DIR = _REPO_ROOT / "go"


def read_records(path: Path) -> list[bytes]:
    """Read the length-prefixed records the Go test writes."""
    data = path.read_bytes()
    records: list[bytes] = []
    offset = 0
    while offset < len(data):
        if offset + 4 > len(data):
            raise SystemExit(f"truncated length prefix at offset {offset}")
        (length,) = struct.unpack(">I", data[offset : offset + 4])
        offset += 4
        if offset + length > len(data):
            raise SystemExit(f"truncated payload at offset {offset}, want {length} bytes")
        records.append(data[offset : offset + length])
        offset += length
    return records


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    probe = Path(argv[0]) if argv else None

    if probe is None:
        # No path given: drive the Go half ourselves so one command does it all.
        fd, name = tempfile.mkstemp(prefix="go-interop-", suffix=".bin")
        os.close(fd)
        probe = Path(name)
        env = dict(os.environ, OMNIGENT_INTEROP_OUT=str(probe))
        result = subprocess.run(
            ["go", "test", "./internal/db/", "-run", "TestWritesAreReadableByPython",
             "-count=1", "-v"],
            cwd=str(_GO_DIR),
            env=env,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
        )
        if result.returncode != 0:
            print(result.stdout[-3000:])
            print(result.stderr[-3000:])
            raise SystemExit("Go half failed; see output above")
        if not probe.is_file() or probe.stat().st_size == 0:
            raise SystemExit(f"Go test did not write a probe at {probe}")

    from omnigent.db.compression import decode as py_decode

    records = read_records(probe)
    if len(records) != 4:
        raise SystemExit(f"expected 4 records (2 pairs), got {len(records)}")

    failures = 0
    for index in range(0, len(records), 2):
        plain = records[index].decode("utf-8")
        frame = records[index + 1]

        framed, note = _classify(frame)
        decoded = py_decode(frame)

        pair = index // 2
        if decoded != plain:
            print(f"FAIL pair {pair}: Python decoded {decoded[:60]!r}, want {plain[:60]!r}")
            failures += 1
            continue
        print(f"OK   pair {pair} ({framed}{note}): {len(frame)} frame bytes -> "
              f"{len(plain)} chars")

    if probe.name.startswith("go-interop-"):
        probe.unlink(missing_ok=True)
    if failures:
        print(f"\n{failures} pair(s) failed: the Go encoder is not Python-compatible")
        return 1
    print("\nAll frames decode in Python: the Go encoder is compatible")
    return 0


def _classify(frame: bytes) -> tuple[str, str]:
    """Name the frame shape so a mismatched expectation is visible."""
    if not frame or frame[0] != 0x00:
        return "unframed", " (legacy passthrough)"
    if len(frame) < 2:
        return "truncated", ""
    return {0x00: "raw", 0x01: "zstd"}.get(frame[1], f"codec=0x{frame[1]:02x}"), ""


if __name__ == "__main__":
    sys.exit(main())
