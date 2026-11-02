# Omnigent in Go

The Go rewrite. The Python implementation under `../omnigent/` stays runnable
throughout: it is the behavioural baseline the rewrite is measured against, and
it is the rollback path.

## Why a separate module at the repo root

- `../omnigent/` is packaged by `pyproject.toml` (`include = ["omnigent*"]`).
  Go sources inside it would be walked by the Python tooling, and removing the
  Python side at cutover would mean picking Go files out of a package tree.
- `go.mod` at the repo root would also claim `../designs/go-rewrite/harness`,
  which is a separate module and must stay independently buildable -- it is
  test infrastructure, not product code.

## Layout

```
cmd/omnigent/        unified entry point (server / runner subcommands)
cmd/openapi-diff/    contract comparison, see ../designs/go-rewrite/P0-CONTRACT.md
internal/api/        generated OpenAPI types and server interface
internal/db/         on-disk encodings shared with Python (Uuid16, enum codes, CompressedText)
internal/event/      the 56 SSE event types
internal/harness/    harness Backend interface plus the pi implementation
internal/policy/     CEL policy evaluation
internal/runner/     per-session runner (P3)
internal/store/      SQLite access
internal/stream/     session event fan-out (ordering, backpressure, SSE wire format)
testdata/            Python-written reference database (the compatibility oracle)
```

## Checking the contract

```bash
bash ../designs/go-rewrite/run-contract.sh     # A1-A6, seconds, offline
go test ./...
```

A1–A6 gate all later work: they prove the Go build reads a database the Python
build wrote and serves the same HTTP surface the reused web client expects.
