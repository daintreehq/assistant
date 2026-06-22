// Package auditx is the audit-export tool family (audit.export, risk "read"). It
// filters the durable audit log and serializes the result as JSON or CSV. It only
// queries audit_log via the AuditStore seam and returns the serialized content in
// the tool result; it never touches the filesystem, so it respects the
// no-file-edit invariant. The caller (the model, or a CLI user via `/audit
// export`) decides what to do with the returned string.
package auditx

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// AuditFilters is the optional AND-combined filter set for QueryAudit. ms bounds
// are inclusive. Locally defined (consumer-interface idiom) so this package does
// not import the storage subsystem; the wiring layer adapts storage.AuditFilters
// to this shape. Mirrors storage.AuditFilters field-for-field.
type AuditFilters struct {
	Actor    *domain.ToolActor
	ToolName *string
	Outcome  *string
	TsFrom   *int64
	TsTo     *int64
	Limit    *int
}

// AuditStore is the slice of storage this family reads: a filtered, newest-first
// query over audit_log. Mirrors storage.Store.QueryAudit.
type AuditStore interface {
	QueryAudit(f AuditFilters) ([]domain.AuditRecord, error)
}

// Deps wires the family to the audit store.
type Deps struct {
	Store AuditStore
}

// AuditExportColumns is the column order for CSV/JSON export — mirrors the
// audit_log table declaration. Exported for the CLI `/audit export` surface.
var AuditExportColumns = []string{
	"id", "ts", "actor", "toolName", "argsJson", "outcome",
	"durationMs", "summary", "resultJson", "grantSource", "grantId",
}

// csvSpecial matches a field that must be quoted per RFC 4180.
var csvSpecial = regexp.MustCompile(`[",\r\n]`)

// CSVField escapes a single CSV field per RFC 4180: wrap in double quotes when
// the value contains a comma, double quote, CR or LF, and double any embedded
// quote. A nil/empty value (nullable columns like resultJson) renders as empty.
func CSVField(value string) string {
	if csvSpecial.MatchString(value) {
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	return value
}

// auditColumn extracts a column's string value from an AuditRecord, matching the
// TS `row[col]` lookup. null/undefined columns render as "".
func auditColumn(r domain.AuditRecord, col string) string {
	switch col {
	case "id":
		return r.ID
	case "ts":
		return strconv.FormatInt(r.Ts, 10)
	case "actor":
		return string(r.Actor)
	case "toolName":
		return r.ToolName
	case "argsJson":
		return r.ArgsJson
	case "outcome":
		return r.Outcome
	case "durationMs":
		return strconv.FormatInt(r.DurationMs, 10)
	case "summary":
		return r.Summary
	case "resultJson":
		return derefStr(r.ResultJson)
	case "grantSource":
		if r.GrantSource != nil {
			return string(*r.GrantSource)
		}
		return ""
	case "grantId":
		return derefStr(r.GrantID)
	}
	return ""
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// AuditToCSV serializes audit rows to RFC 4180 CSV with a header row and CRLF
// line endings (load-bearing — the export wire format).
func AuditToCSV(rows []domain.AuditRecord) string {
	lines := []string{strings.Join(AuditExportColumns, ",")}
	for _, row := range rows {
		fields := make([]string, len(AuditExportColumns))
		for i, col := range AuditExportColumns {
			fields[i] = CSVField(auditColumn(row, col))
		}
		lines = append(lines, strings.Join(fields, ","))
	}
	return strings.Join(lines, "\r\n")
}

// SerializeAudit serializes rows in the requested format. JSON uses 2-space
// indentation (JSON.stringify(rows, null, 2)).
func SerializeAudit(rows []domain.AuditRecord, format string) string {
	if format == "csv" {
		return AuditToCSV(rows)
	}
	// JSON.stringify(rows, null, 2) of an empty array is "[]"; encoding/json of a
	// nil slice is "null" — normalize so the JSON export is always an array.
	if rows == nil {
		rows = []domain.AuditRecord{}
	}
	out, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return "[]"
	}
	return string(out)
}

// ParseAuditExportArgs parses the CLI form `export <json|csv> [key=value …]`
// shared by both /audit surfaces. tokens is everything after the `export`
// keyword. Returns (format, filters, "") on success or ("", {}, errMsg) to show.
func ParseAuditExportArgs(tokens []string) (string, AuditFilters, string) {
	usage := "Usage: /audit export <json|csv> [actor=… tool=… outcome=… from=<ms> to=<ms> limit=<n>]"
	if len(tokens) == 0 {
		return "", AuditFilters{}, usage
	}
	format := tokens[0]
	if format != "json" && format != "csv" {
		return "", AuditFilters{}, usage
	}
	var f AuditFilters
	for _, tok := range tokens[1:] {
		eq := strings.Index(tok, "=")
		if eq <= 0 {
			return "", AuditFilters{}, fmt.Sprintf("Bad filter '%s'. Use key=value (e.g. actor=main).", tok)
		}
		key := tok[:eq]
		value := tok[eq+1:]
		// Reject empty values: `from=` would otherwise coerce to 0 (the Unix epoch).
		if value == "" {
			return "", AuditFilters{}, fmt.Sprintf("Empty value for '%s'. Use key=value (e.g. actor=main).", key)
		}
		switch key {
		case "actor":
			a := domain.ToolActor(value)
			f.Actor = &a
		case "tool", "toolName":
			v := value
			f.ToolName = &v
		case "outcome":
			v := value
			f.Outcome = &v
		case "from", "tsFrom":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", AuditFilters{}, fmt.Sprintf("from must be an integer (Unix ms), got '%s'.", value)
			}
			f.TsFrom = &n
		case "to", "tsTo":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", AuditFilters{}, fmt.Sprintf("to must be an integer (Unix ms), got '%s'.", value)
			}
			f.TsTo = &n
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 5000 {
				return "", AuditFilters{}, fmt.Sprintf("limit must be an integer 1–5000, got '%s'.", value)
			}
			f.Limit = &n
		default:
			return "", AuditFilters{}, fmt.Sprintf("Unknown filter '%s'. Use actor, tool, outcome, from, to, limit.", key)
		}
	}
	return format, f, ""
}

type exportArgs struct {
	Format   string  `json:"format"`
	Actor    *string `json:"actor,omitempty"`
	ToolName *string `json:"toolName,omitempty"`
	Outcome  *string `json:"outcome,omitempty"`
	TsFrom   *int64  `json:"tsFrom,omitempty"`
	TsTo     *int64  `json:"tsTo,omitempty"`
	Limit    *int    `json:"limit,omitempty"`
}

// Validate enforces `limit: int().min(1).max(5000)` so a negative/oversized row
// cap is rejected rather than forwarded unbounded to the store query. Matches the
// /audit export CLI 1–5000 bound.
func (a *exportArgs) Validate() error {
	if a.Limit != nil && (*a.Limit < 1 || *a.Limit > 5000) {
		return fmt.Errorf("limit must be between 1 and 5000")
	}
	return nil
}

var exportSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "format": { "type": "string", "enum": ["json", "csv"], "description": "Output format for the exported rows." },
    "actor": { "type": "string", "description": "Filter by actor (main, watcher, timer, workflow, system)." },
    "toolName": { "type": "string", "description": "Filter by tool name (exact match, e.g. fs.read)." },
    "outcome": { "type": "string", "description": "Filter by outcome (ok, error, denied, dedup, grant_ok)." },
    "tsFrom": { "type": "number", "description": "Inclusive start of the time range, Unix epoch milliseconds." },
    "tsTo": { "type": "number", "description": "Inclusive end of the time range, Unix epoch milliseconds." },
    "limit": { "type": "number", "description": "Maximum rows to return (newest first). Default 200, max 5000." }
  },
  "required": ["format"]
}`)

// Tools returns the audit family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{{
		Name: "audit.export",
		Description: "Export the audit log as JSON or CSV with optional filters (actor, toolName, outcome, and a tsFrom/tsTo time " +
			"range in Unix ms). Filters are AND-combined; rows are returned newest-first and bounded by `limit`. Read-only — " +
			"returns the serialized content as a string for the caller to save or inspect.",
		Risk:   domain.RiskRead,
		Schema: exportSchema,
		Decode: tools.StrictDecoder(func() any { return &exportArgs{} }),
		Handle: func(_ context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			return handle(deps, raw)
		},
	}}
}

func handle(deps Deps, raw json.RawMessage) tools.ToolResult {
	var a exportArgs
	_ = json.Unmarshal(raw, &a)
	// Default limit 200 (the Zod .default) — applied here since strict decode
	// keeps it nil when omitted.
	limit := 200
	if a.Limit != nil {
		limit = *a.Limit
	}
	var actor *domain.ToolActor
	if a.Actor != nil {
		av := domain.ToolActor(*a.Actor)
		actor = &av
	}
	filters := AuditFilters{
		Actor:    actor,
		ToolName: a.ToolName,
		Outcome:  a.Outcome,
		TsFrom:   a.TsFrom,
		TsTo:     a.TsTo,
		Limit:    &limit,
	}
	rows, err := deps.Store.QueryAudit(filters)
	if err != nil {
		return tools.Fail(domain.CodeInternal, "audit.export: "+err.Error())
	}
	content := SerializeAudit(rows, a.Format)
	plural := "s"
	if len(rows) == 1 {
		plural = ""
	}
	return tools.Ok(
		fmt.Sprintf("Exported %d audit row%s as %s.", len(rows), plural, strings.ToUpper(a.Format)),
		map[string]any{"format": a.Format, "count": len(rows), "content": content})
}
