package commands

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/storage"
)

// FormatRunList renders the /explain run list (commandRegistry §6.7).
func FormatRunList(runs []domain.RunSummaryRecord) string {
	if len(runs) == 0 {
		return "(no runs recorded yet — run a turn first, then /explain)"
	}
	lines := make([]string, 0, len(runs))
	for _, r := range runs {
		noun := "events"
		if r.EventCount == 1 {
			noun = "event"
		}
		lines = append(lines, fmt.Sprintf("%s %s  %d %s",
			padRight(r.RunID, 16), localTime(r.FirstTs), r.EventCount, noun))
	}
	return strings.Join(lines, "\n")
}

// FormatRunTimeline renders a single run's replay (commandRegistry §6.7). It
// parses each event payload defensively and joins audit outcomes for tool results.
func FormatRunTimeline(events []domain.RunEventRecord, auditRows []domain.AuditRecord) string {
	auditByID := make(map[string]domain.AuditRecord, len(auditRows))
	for _, a := range auditRows {
		auditByID[a.ID] = a
	}
	var lines []string
	for _, ev := range events {
		payload := parseEventPayload(ev.Payload)

		// Truncated wrapper {truncated,bytes,preview}.
		if trunc, ok := payload["truncated"].(bool); ok && trunc {
			bytes := numToInt(payload["bytes"])
			lines = append(lines, fmt.Sprintf("… [truncated %s — %d bytes]", ev.Type, bytes))
			if prev, ok := payload["preview"].(string); ok && prev != "" {
				lines = append(lines, indent(prev, 4))
			}
			continue
		}

		switch ev.Type {
		case "assistant:start":
			lines = append(lines, "▸ assistant")
		case "assistant:content":
			lines = append(lines, indent(str(payload["content"]), 4))
		case "assistant:end":
			if r := str(payload["reasoning"]); r != "" {
				lines = append(lines, "  reasoning:")
				lines = append(lines, indent(r, 6))
			}
			lines = append(lines, indent(str(payload["content"]), 4))
		case "assistant:cancelled":
			lines = append(lines, "■ cancelled")
			if c := str(payload["content"]); c != "" {
				lines = append(lines, indent(c, 4))
			}
		case "tool:call":
			lines = append(lines, "→ tool "+str(payload["name"])+" "+previewArgs(payload["args"]))
		case "tool:result":
			ok, _ := payload["ok"].(bool)
			mark := "✗"
			if ok {
				mark = "✓"
			}
			detail := "ok"
			if !ok {
				detail = "error"
			}
			if aid := str(payload["auditId"]); aid != "" {
				if a, found := auditByID[aid]; found {
					detail = fmt.Sprintf("%s, %dms", a.Outcome, a.DurationMs)
				}
			}
			lines = append(lines, fmt.Sprintf("%s tool %s (%s)", mark, str(payload["name"]), detail))
			if s := str(payload["summary"]); s != "" {
				lines = append(lines, indent(s, 4))
			}
		case "error":
			lines = append(lines, "⚠ error: "+str(payload["message"]))
		case "info":
			lines = append(lines, "· "+str(payload["message"]))
		default:
			lines = append(lines, "· "+ev.Type)
		}
	}
	return strings.Join(lines, "\n")
}

// --- audit export (commandRegistry §6.4 audit) ---

// AuditExportResult is the parsed `/audit export` request.
type AuditExportResult struct {
	Format  string // "json" | "csv"
	Filters storage.AuditFilters
	Error   string // non-empty on a parse error
}

// ParseAuditExportArgs parses `export <format> [k=v ...]`. Defaults format json.
// Supported filters: tool=<name>, outcome=<outcome>, actor=<actor>, n=<limit>.
func ParseAuditExportArgs(args []string) AuditExportResult {
	res := AuditExportResult{Format: "json"}
	for i, a := range args {
		if i == 0 && (a == "json" || a == "csv") {
			res.Format = a
			continue
		}
		k, v, found := strings.Cut(a, "=")
		if !found {
			res.Error = "Unknown audit export argument '" + a + "'. Use export <json|csv> [tool=… outcome=… actor=… n=…]."
			return res
		}
		switch k {
		case "tool":
			vv := v
			res.Filters.ToolName = &vv
		case "outcome":
			vv := v
			res.Filters.Outcome = &vv
		case "actor":
			act := domain.ToolActor(v)
			res.Filters.Actor = &act
		case "n":
			if n := atoiSafe(v); n > 0 {
				res.Filters.Limit = &n
			}
		default:
			res.Error = "Unknown audit export filter '" + k + "'."
			return res
		}
	}
	return res
}

// SerializeAudit renders audit rows as json or csv.
func SerializeAudit(rows []domain.AuditRecord, format string) string {
	if format == "csv" {
		var sb strings.Builder
		w := csv.NewWriter(&sb)
		_ = w.Write([]string{"id", "ts", "actor", "toolName", "outcome", "durationMs", "summary"})
		for _, r := range rows {
			_ = w.Write([]string{r.ID, fmt.Sprintf("%d", r.Ts), string(r.Actor), r.ToolName,
				r.Outcome, fmt.Sprintf("%d", r.DurationMs), r.Summary})
		}
		w.Flush()
		return sb.String()
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return "[]"
	}
	return string(b)
}

// --- small helpers ---

func parseEventPayload(p *string) map[string]any {
	if p == nil || *p == "" {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal([]byte(*p), &m) != nil {
		return map[string]any{}
	}
	return m
}

// previewArgs JSON-stringifies args, drops "{}", caps at 120 chars (slice 117 + …).
func previewArgs(args any) string {
	if args == nil {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	s := string(b)
	if s == "{}" {
		return ""
	}
	rs := []rune(s)
	if len(rs) > 120 {
		return string(rs[:117]) + "…"
	}
	return s
}

// indent prefixes every line of s with `pad` spaces.
func indent(s string, pad int) string {
	prefix := strings.Repeat(" ", pad)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func numToInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func localTime(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
