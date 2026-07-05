#!/usr/bin/env bash
# Inspect what the backend's skill-learning harness proposed / created / updated. Run
# after a LEARN=propose|apply session to review the self-improvement output: the
# uncommitted changes to skill + prompt files, and the audit trail. Usage: skill-diff.sh
set -uo pipefail
BACKEND_DIR="${DAINTREE_BACKEND_DIR:-/Users/gpriday/Projects/Daintree/assistant-backend}"
cd "$BACKEND_DIR"
SK="src/daintree_assistant_server/skills"
PR="src/daintree_assistant_server/prompts"

echo "# uncommitted skill/prompt changes (what apply-mode learning wrote — review, then keep or revert)"
git status -s -- "$SK" "$PR" | sed 's/^/   /'
echo
echo "# diff of skill files"
git --no-pager diff -- "$SK/files"
echo
echo "# skill-learning audit trail (proposals + gate decisions + confidence)"
AUDIT="${SKILL_LEARNING_AUDIT_DIR:-.daintree/skill-learning}"
if [ -d "$AUDIT" ]; then
  ls -lt "$AUDIT" 2>/dev/null | head -10
  f="$(ls -t "$AUDIT"/* 2>/dev/null | head -1 || true)"
  [ -n "$f" ] && { echo "--- newest audit entry: $f ---"; head -80 "$f"; }
else
  echo "no audit dir at $BACKEND_DIR/$AUDIT (learning was off, or nothing has run yet)"
fi
