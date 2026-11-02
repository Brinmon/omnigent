#!/usr/bin/env bash
# Prove the OMNIGENT_COMPAT_SERVER_PYTHON knob swaps the server subprocess.
# First green run establishes the "judge" the Go rewrite will be graded by.
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

export PYTHONUTF8=1
export NO_PROXY=127.0.0.1,localhost
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy
unset ANTHROPIC_API_KEY OPENAI_API_KEY DATABRICKS_TOKEN LLM_API_KEY

wrapper=${SERVER_WRAPPER_BIN:-}
if [[ -z "$wrapper" ]]; then
  echo "server-wrapper: building" >&2
  ( cd designs/go-rewrite/harness && go build -o "$repo_root/.tmp-server-wrapper" . )
  wrapper="$repo_root/.tmp-server-wrapper"
fi

# Point the wrapper at the real (baseline) python CLI for this smoke check.
# Baseline mode: run the target as an interpreter over "-m omnigent".
export GO_SERVER_BIN="${GO_SERVER_BIN:-$repo_root/.venv/Scripts/python.exe}"
export GO_SERVER_MODULE_ENTRY="${GO_SERVER_MODULE_ENTRY:-1}"

OMNIGENT_COMPAT_SERVER_PYTHON="$wrapper" \
OMNIGENT_COMPAT_SERVER_VERSION="${OMNIGENT_COMPAT_SERVER_VERSION:-0.21.0}" \
  .venv/Scripts/python.exe -m pytest tests/e2e_ui/chat/test_smoke.py \
    -x -q --ui-skip-build --browser-channel "${BROWSER_CHANNEL:-msedge}" "$@"
