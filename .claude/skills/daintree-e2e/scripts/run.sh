#!/usr/bin/env bash
# Build the newest Daintree assistant CLI and run ONE query end-to-end against the
# local backend + live Daintree MCP. Long-running (spawns agents): launch this via a
# BACKGROUND Bash, then watch the debug log with analyze-log.sh.
#
#   run.sh "the prompt text"        # prompt inline
#   run.sh -f /path/to/prompt.txt   # prompt from a file
#
# Requires: the backend already running (start-backend.sh) and a valid MCP token
# (env DAINTREE_MCP_URL + DAINTREE_MCP_TOKEN, exported from a live Daintree-launched
# assistant process — they are deliberately not recoverable from any log).
# Prints the new debug-log path on the final line.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PY="${PYTHON:-python3}"
ASSISTANT_DIR="${DAINTREE_ASSISTANT_DIR:-/Users/gpriday/Projects/Daintree/assistant}"
PROJECT_DIR="${DAINTREE_PROJECT_DIR:-/Users/gpriday/Projects/Daintree/daintree}"
BACKEND_URL="${DAINTREE_BACKEND_URL:-http://127.0.0.1:8473}"

if [ "${1:-}" = "-f" ]; then PROMPT="$(cat "${2:?-f needs a file}")"; else PROMPT="${1:?usage: run.sh \"prompt\" | run.sh -f file}"; fi

# 1. build the newest binary (test the change you just made)
( cd "$ASSISTANT_DIR" && make build >&2 )

# 2. fresh MCP creds — env ONLY; see mcp.py for why there is no log fallback
IFS=$'\t' read -r MCP_URL MCP_TOKEN < <("$PY" "$HERE/mcp.py" creds)
[ -n "${MCP_TOKEN:-}" ] || { echo "no MCP token: export DAINTREE_MCP_URL/DAINTREE_MCP_TOKEN from a live assistant process" >&2; exit 1; }

# 3. one-shot --json, with the FULL debug trace; cwd = the Daintree project (the CLI
#    takes ProjectPath from cwd). AUTO_APPROVE so the non-interactive run can spawn.
export DAINTREE_MCP_URL="$MCP_URL" DAINTREE_MCP_TOKEN="$MCP_TOKEN"
export DAINTREE_BACKEND_URL="$BACKEND_URL"
export DAINTREE_ASSISTANT_DEBUG_LOG=1 DAINTREE_ASSISTANT_TIER=system DAINTREE_ASSISTANT_AUTO_APPROVE=1
before="$(ls ~/.daintree/logs/*.log 2>/dev/null | sort || true)"
cd "$PROJECT_DIR"
"$ASSISTANT_DIR/bin/daintree-assistant" --json "$PROMPT" || true

# 4. report the debug log this run created
comm -13 <(printf '%s\n' "$before") <(ls ~/.daintree/logs/*.log 2>/dev/null | sort) | tail -1
