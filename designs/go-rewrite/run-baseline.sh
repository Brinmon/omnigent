#!/usr/bin/env bash
# Run the Playwright acceptance baseline batch by batch and write evidence.
#
# Each acceptance ID runs as its own pytest invocation so one hang or one
# failing module cannot take down the whole run. Results land in
# $EVIDENCE/<ID>/ with a manifest and the raw pytest log.
#
#   bash designs/go-rewrite/run-baseline.sh            # every group
#   bash designs/go-rewrite/run-baseline.sh B E        # only B and E IDs
#   EVIDENCE=/tmp/ev bash designs/go-rewrite/run-baseline.sh C
set -uo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

EVIDENCE=${EVIDENCE:-$repo_root/designs/go-rewrite/evidence/baseline}
CHANNEL=${BROWSER_CHANNEL:-msedge}
PER_TEST_TIMEOUT=${PER_TEST_TIMEOUT:-1200}

export PYTHONUTF8=1
export NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
unset ANTHROPIC_API_KEY OPENAI_API_KEY ANTHROPIC_AUTH_TOKEN \
      DATABRICKS_TOKEN LLM_API_KEY CLAUDE_CODE_OAUTH_TOKEN

# Point Pi at a clean per-run agent dir. The developer's global Pi install
# carries user packages (pi-cworker, pi-subagents, ...) that Pi loads as
# extensions; a broken one aborts `pi` at startup and makes e2e runs flaky for
# reasons unrelated to what the test measures.
export PI_CODING_AGENT_DIR=${PI_CODING_AGENT_DIR:-$EVIDENCE/pi-agent-dir}
mkdir -p "$PI_CODING_AGENT_DIR"

PY=$repo_root/.venv/Scripts/python.exe
[[ -x $PY ]] || PY=$repo_root/.venv/bin/python

mkdir -p "$EVIDENCE"
SUMMARY="$EVIDENCE/SUMMARY.tsv"
printf "id\tresult\tduration_s\ttests\n" > "$SUMMARY"

want=("$@")
selected() {
  ((${#want[@]} == 0)) && return 0
  local g
  for g in "${want[@]}"; do [[ $g == "$1" ]] && return 0; done
  return 1
}

# run <ui|noui> <ID> <description> <pytest targets...>
run() {
  local mode=$1 id=$2 desc=$3
  shift 3
  selected "$id" || return 0

  local out="$EVIDENCE/$id"
  rm -rf "$out"
  mkdir -p "$out"
  local log="$out/pytest.log"
  local -a extra=()
  if [[ $mode == ui ]]; then
    extra=(--ui-skip-build --browser-channel "$CHANNEL"
           --video=on --screenshot=on --output="$out/playwright")
  fi

  echo "──────────────────────────────────────────────────────────────"
  echo "[$id] $desc"
  echo "      $*"

  local started ended dur rc
  started=$(date +%s)
  timeout "$PER_TEST_TIMEOUT" "$PY" -m pytest "$@" -q -p no:randomly \
    "${extra[@]}" > "$log" 2>&1
  rc=$?
  ended=$(date +%s)
  dur=$((ended - started))

  local tests_line result
  tests_line=$(grep -oE '[0-9]+ (passed|failed|error|skipped)[^=]*' "$log" | tail -1)
  tests_line=${tests_line%% in *}

  if grep -qE 'unrecognized arguments|ERROR: usage:' "$log"; then
    result=HARNESS_ERROR
  elif [[ $rc -eq 0 ]]; then
    result=PASS
  elif [[ $rc -eq 124 ]]; then
    result=TIMEOUT
  else
    result=FAIL
  fi

  echo "      → $result (${dur}s) ${tests_line:-}"

  printf "%s\t%s\t%s\t%s\n" "$id" "$result" "$dur" "${tests_line:-}" >> "$SUMMARY"
  {
    echo "id: $id"
    echo "description: $desc"
    echo "result: $result"
    echo "exit_code: $rc"
    echo "duration_s: $dur"
    echo "mode: $mode"
    echo "browser_channel: ${CHANNEL}"
    echo "commit: $(git rev-parse --short HEAD)"
    echo "python: $("$PY" -V 2>&1)"
    echo "targets: $*"
    echo "tail: $(tail -3 "$log" | tr '\n' ' ' | sed 's/[[:space:]]\+/ /g')"
  } > "$out/manifest.txt"
}

# ── B · startup chain ─────────────────────────────────────────────────
run ui   B1 "SPA boot + /v1/info /v1/me /v1/harnesses"   tests/e2e_ui/chat/test_smoke.py
run ui   B2 "session list renders in sidebar"            tests/e2e_ui/sessions/test_sidebar_search.py tests/e2e_ui/sessions/test_sidebar_rename.py
run ui   B3 "history hydration after reload"             tests/e2e_ui/chat/test_initial_history_loading.py tests/e2e_ui/chat/test_reload_continue.py
run ui   B4 "send message -> SSE -> assistant bubble"    tests/e2e_ui/chat/test_smoke.py
run ui   B5 "WS /v1/sessions/updates increments"         tests/e2e_ui/sessions/test_session_updates_stream.py

# ── E · session lifecycle ─────────────────────────────────────────────
run ui   E1 "rename / pin / archive / delete"            tests/e2e_ui/sessions/test_sidebar_rename.py tests/e2e_ui/sessions/test_sidebar_pin_unpin.py tests/e2e_ui/sessions/test_sidebar_delete.py tests/e2e_ui/sessions/test_archived_session_header_menu.py
run ui   E2 "search title + content + archived"          tests/e2e_ui/sessions/test_session_search.py tests/e2e_ui/sessions/test_sidebar_search.py
run ui   E3 "reload / reconnect dedup"                   tests/e2e_ui/chat/test_reload_continue.py
run ui   E4 "presence co-viewers"                        tests/e2e_ui/collaboration/test_collab_realtime.py
run ui   E5 "unread survives reload"                     tests/e2e_ui/sessions/test_sidebar_mark_unread.py

# ── C · Pi agent (own environment: no SPA flags) ──────────────────────
run noui C1 "real pi --mode rpc one turn"                tests/e2e/omnigent/test_per_harness_pi.py
run noui C2 "tool call round trip"                       tests/e2e/omnigent/test_pi_long_tool_turn.py
run noui C4 "interrupt then continue"                    tests/e2e/test_pi_interrupt_reap_budget_race_e2e.py
run noui C5 "thinking / reasoning stream"                tests/e2e/omnigent/test_pi_thinking_compatibility_e2e.py
run noui C6 "provider retry recovery"                    tests/e2e/omnigent/test_pi_retry_recovery_e2e.py
run noui C7 "compaction + get_messages"                  tests/e2e/test_pi_compact_restart_resume.py
run noui C8 "policy ASK round trip"                      tests/e2e/test_pi_extension_ui_round_trip.py

# ── D · policy / resources ────────────────────────────────────────────
run ui   D1 "PHASE_REQUEST / PHASE_TOOL_CALL gate"       tests/e2e_ui/approvals/test_approval_card.py
run ui   D3 "terminal create/read/write/close"           tests/e2e_ui/shells/test_new_shell.py
run ui   D5 "workspace file search"                      tests/e2e_ui/files/test_file_search.py
run ui   D6 "fork keeps history, stays isolated"         tests/e2e_ui/fork_session/test_fork_from_middle.py

echo
echo "══════════════════════════════════════════════════════════════"
column -t -s $'\t' "$SUMMARY" 2>/dev/null || cat "$SUMMARY"
