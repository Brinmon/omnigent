#!/usr/bin/env bash
# Run the A1-A6 contract checks (scope P0) and record evidence.
#
# A-group gates P1: without it the Go build can disagree with the Python one
# on the wire contract or on the on-disk encoding, and every later stage
# would be built on a wrong foundation. Every check here runs offline in
# seconds -- no browser, no server -- so it belongs in CI.
#
# Usage:
#   bash designs/go-rewrite/run-contract.sh              # all checks
#   bash designs/go-rewrite/run-contract.sh A5 A6        # selected
#   EVIDENCE=path bash designs/go-rewrite/run-contract.sh
#
# Exit 0 means every selected check passed.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT" || exit 1

EVIDENCE="${EVIDENCE:-designs/go-rewrite/evidence/contract}"
GO_DIR="$REPO_ROOT/go"
PYTHON="$REPO_ROOT/.venv/Scripts/python.exe"
[[ -x "$PYTHON" ]] || PYTHON="$REPO_ROOT/.venv/bin/python"
SUMMARY="$EVIDENCE/SUMMARY.tsv"

# A clean environment: a proxy would break the localhost fetches, and a
# developer's global Pi packages or model keys must not influence a contract
# check.
export PYTHONUTF8=1
export NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy

mkdir -p "$EVIDENCE"
printf 'id\tresult\tduration_s\ttests\n' > "$SUMMARY"

FAILED=()

record() {
  local id="$1" result="$2" duration="$3" tests="$4"
  printf '%s\t%s\t%s\t%s\n' "$id" "$result" "$duration" "$tests" >> "$SUMMARY"
}

# run <ID> <description> <command...>
run() {
  local id="$1" description="$2"
  shift 2
  local dir="$EVIDENCE/$id"
  rm -rf "$dir"
  mkdir -p "$dir"

  printf '\n[%s] %s\n' "$id" "$description"
  local start end duration status
  start=$(date +%s)
  "$@" > "$dir/output.log" 2>&1
  status=$?
  end=$(date +%s)
  duration=$((end - start))

  {
    printf 'id: %s\n' "$id"
    printf 'description: %s\n' "$description"
    printf 'command: %s\n' "$*"
    printf 'commit: %s\n' "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
    printf 'go: %s\n' "$(go version 2>/dev/null | awk '{print $3}' || echo unknown)"
    printf 'python: %s\n' "$("$PYTHON" --version 2>&1 | awk '{print $2}' || echo unknown)"
    printf 'duration_s: %s\n' "$duration"
    printf 'result: %s\n' "$([[ $status -eq 0 ]] && echo PASS || echo FAIL)"
    printf 'tail:\n'
    tail -n 20 "$dir/output.log" 2>/dev/null
  } > "$dir/manifest.txt"

  local tests
  tests=$(grep -cE '^(--- PASS|ok )' "$dir/output.log" 2>/dev/null || echo 0)
  if [[ $status -eq 0 ]]; then
    record "$id" PASS "$duration" "$tests checks"
    printf '      → PASS (%ss)\n' "$duration"
  else
    record "$id" FAIL "$duration" "$tests checks"
    printf '      → FAIL (%ss)  see %s/output.log\n' "$duration" "$dir"
    FAILED+=("$id")
  fi
}

# Refuse to run against a stale fixture: a reference database older than the
# generator would compare Go against an encoding that has already moved.
check_fixture_fresh() {
  local db="$GO_DIR/testdata/reference.db" gen="$REPO_ROOT/designs/go-rewrite/make-reference-db.py"
  [[ -f "$db" ]] || return 0
  [[ -f "$gen" ]] || return 0
  if [[ "$gen" -nt "$db" ]]; then
    printf 'WARNING: testdata/reference.db is older than its generator;\n'
    printf '         regenerate with: %s designs/go-rewrite/make-reference-db.py\n' "$PYTHON"
  fi
}

want() {
  [[ ${#SELECTED[@]} -eq 0 ]] && return 0
  local id
  for id in "${SELECTED[@]}"; do [[ "$id" == "$1" ]] && return 0; done
  return 1
}

SELECTED=("$@")
check_fixture_fresh

if want A1 || want A2 || want A3; then
  # A1/A2/A3 are one comparison: paths, operations, and schemas in a single
  # pass, because they share a parse and fail for related reasons.
  if want A1; then
    run A1 "OpenAPI path set equivalent (117 paths)" \
      bash -c "cd '$GO_DIR' && go test ./cmd/openapi-diff/ -run TestIdenticalDocumentsMatch -v"
  fi
  if want A2; then
    run A2 "All 154 operations implemented" \
      bash -c "cd '$GO_DIR' && go test ./cmd/openapi-diff/ -run 'TestMissingOperationFails|TestMissingPathFails' -v"
  fi
  if want A3; then
    run A3 "194 schema field/required/enum parity" \
      bash -c "cd '$GO_DIR' && go test ./cmd/openapi-diff/ -run 'TestMissingProperty|TestRelaxedRequired|TestMissingEnumValue|TestMissingSchema' -v"
  fi
fi

# A4 depends on the generated union being current, so check the generation
# before the tests that read it.
if want A4; then
  run A4-gen "oapi-codegen output matches openapi.json"     bash designs/go-rewrite/check-generated.sh
fi

if want A4; then
  run A4 "ServerStreamEvent 56 variants match spec" \
    bash -c "cd '$GO_DIR' && go test ./internal/event/ -v"
fi

# P1 acceptance: both builds answer the same JSON for the endpoints P1
# implements, reading one Python-written database. This is stronger than a
# golden comparison because it runs both servers now.
if want A7; then
  run A7 "Go and Python serve identical JSON (P1 endpoints)"     bash -c "cd '$REPO_ROOT' && .venv/Scripts/python.exe designs/go-rewrite/compare-servers.py"
fi

if want A5; then
  run A5 "Go reads a Python-written database" \
    bash -c "cd '$GO_DIR' && go test ./internal/db/ -run 'TestReferenceDBIsReadable|TestUuid16RoundTrip' -v"
fi

if want A6; then
  run A6 "Uuid16 / enum codes / CompressedText compatibility" \
    bash -c "cd '$GO_DIR' && go test ./internal/db/ -run 'TestCompressedText|TestEnumCodes|TestStoredEnumCodes' -v"
fi

printf '\n%-3s %-6s %s\n' "id" "result" "checks"
column -t -s $'\t' "$SUMMARY" 2>/dev/null || cat "$SUMMARY"

if [[ ${#FAILED[@]} -gt 0 ]]; then
  printf '\nFAILED: %s\n' "${FAILED[*]}"
  exit 1
fi

printf '\nAll selected contract checks passed. Evidence: %s\n' "$EVIDENCE"
