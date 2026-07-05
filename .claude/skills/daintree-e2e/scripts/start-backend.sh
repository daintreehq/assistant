#!/usr/bin/env bash
# Start the Daintree Assistant backend for a test run and serve on :8473.
# Run this via a BACKGROUND Bash (it stays running); kill it when done
# (pkill -f daintree_assistant_server).
#
#   start-backend.sh                 # LEARN=off (clean baseline for debugging behavior),
#                                    # thinking = the backend default (currently OFF)
#   LEARN=propose start-backend.sh   # skill-learning DRY-RUN: writes proposals, touches nothing
#   LEARN=apply   start-backend.sh   # skill-learning LIVE: learns + writes/updates real skill
#                                    #   files in place (inspect with skill-diff.sh afterward)
#   THINKING=on  start-backend.sh    # force MAIN_THINKING_ENABLED=true (A/B the plan-dump)
#   THINKING=off start-backend.sh    # force it false
#
# LEARN mode picks what the skill-learning harness does after a session:
#   off      → fixed catalog; use while ITERATING so skills can't mutate between runs.
#   propose  → run the learner, write proposals only (safe to inspect, nothing changed).
#   apply    → the self-improvement loop for real — the run's lessons are written into the
#              skill files (shows up in the backend's `git diff`). Use when the TEST IS the
#              skill creation/update ("work through this, create the skill").
set -euo pipefail
BACKEND_DIR="${DAINTREE_BACKEND_DIR:-/Users/gpriday/Projects/Daintree/assistant-backend}"
LEARN="${LEARN:-off}"
case "$LEARN" in off|propose|apply) ;; *) echo "LEARN must be off|propose|apply" >&2; exit 2 ;; esac
case "${THINKING:-}" in
  on|true|1)   export MAIN_THINKING_ENABLED=true ;;
  off|false|0) export MAIN_THINKING_ENABLED=false ;;
esac
cd "$BACKEND_DIR"
echo "starting backend  SKILL_LEARNING_MODE=$LEARN${MAIN_THINKING_ENABLED:+  MAIN_THINKING_ENABLED=$MAIN_THINKING_ENABLED}" >&2
exec ./dev "$LEARN"
