#!/usr/bin/env bash
# Diagnostic snapshot of a Daintree assistant debug log — surfaces the failure
# signatures we know about. Usage: analyze-log.sh [log]   (default: the newest log)
set -uo pipefail
LOG="${1:-$(ls -t ~/.daintree/logs/*.log 2>/dev/null | head -1)}"
[ -f "$LOG" ] || { echo "no log: $LOG"; exit 1; }
red() { sed -E 's/token=[^ ]+/token=<REDACTED>/g'; }

echo "# $LOG"
echo
echo "## MCP (connected=false ⇒ token expired / Daintree down — the run can't spawn)"
grep -m1 "mcp.credentials" "$LOG" | grep -oE "connected=[a-z]+|transport=[a-z-]+" | tr '\n' ' '; echo
echo
echo "## skills loaded  (0 ⇒ NO runbook injected → model stalls or improvises; check /clear + backend selector)"
grep -ciE "SkillLoaded|Skill loaded" "$LOG"
echo
echo "## rounds  (a round-0 with a HUGE toolCallCount = plan-dump; thinking-on regression)"
grep "backend.respond.done" "$LOG" | while IFS= read -r line; do
  printf '   %s %s %s\n' \
    "$(printf '%s' "$line" | grep -oE 'round=[0-9]+')" \
    "$(printf '%s' "$line" | grep -oE 'toolCallCount=[0-9]+')" \
    "$(printf '%s' "$line" | grep -oE 'finishReason=[a-z_]+')"
done
echo "   biggest single batch: $(grep 'backend.respond.done' "$LOG" | grep -oE 'toolCallCount=[0-9]+' | cut -d= -f2 | sort -rn | head -1)"
echo
echo "## failed tool.calls by tool  (a tool dominating = a loop or a bad-id storm)"
grep "tool.call " "$LOG" | grep "ok=false" | grep -oE "tool=[a-zA-Z._]+" | sort | uniq -c | sort -rn | head
echo
echo "## circuit-breaker fires  (abort = the guard caught a runaway; warning = getting stuck)"
grep -E "tool.repeat" "$LOG" | grep -oE "tool.repeat[a-z.]*|count=[0-9]+|errorCode=[A-Z_]+" | paste - - - 2>/dev/null | head
echo
echo "## backend errors  (400/connect = CLI↔backend contract or backend down)"
grep "backend.respond.error" "$LOG" | red | cut -c1-160 | head -3
echo
echo "## terminal-tool failures  (bad/truncated ids — model requesting terminals that don't exist)"
echo "   failed terminal.* calls: $(grep -E 'tool.call .*tool=terminal' "$LOG" | grep -c 'ok=false')"
echo
echo "## turn ends"
grep "turn.end" "$LOG" | grep -oE "status=[a-z]+|rounds=[0-9]+|replyPreview=.{0,70}" | paste - - - 2>/dev/null | red
