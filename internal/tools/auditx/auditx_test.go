package auditx

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func TestCSVFieldQuoting(t *testing.T) {
	cases := map[string]string{
		"plain":        "plain",
		"has,comma":    `"has,comma"`,
		"has\"quote":   `"has""quote"`,
		"has\nnewline": "\"has\nnewline\"",
		"has\rcr":      "\"has\rcr\"",
		"":             "",
	}
	for in, want := range cases {
		if got := CSVField(in); got != want {
			t.Errorf("CSVField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuditToCSVUsesCRLFAndNullColumns(t *testing.T) {
	gs := domain.AutomationGrantSource("local")
	rows := []domain.AuditRecord{
		{ID: "a1", Ts: 5, Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: `{"path":"x"}`,
			Outcome: "ok", DurationMs: 12, Summary: "Read x", ResultJson: nil, GrantSource: &gs},
	}
	out := AuditToCSV(rows)
	if !strings.HasPrefix(out, strings.Join(AuditExportColumns, ",")+"\r\n") {
		t.Fatalf("missing CRLF header row: %q", out)
	}
	// resultJson is nil → renders empty; the row must have 11 comma-separated fields.
	dataLine := strings.Split(out, "\r\n")[1]
	if fields := strings.Count(dataLine, ",") + 1; fields != len(AuditExportColumns) {
		t.Fatalf("row has %d fields, want %d: %q", fields, len(AuditExportColumns), dataLine)
	}
	if !strings.Contains(dataLine, "local") {
		t.Errorf("grantSource missing from row: %q", dataLine)
	}
}

func TestSerializeAuditJSONEmptyIsArray(t *testing.T) {
	if got := SerializeAudit(nil, "json"); got != "[]" {
		t.Errorf("empty JSON export = %q, want []", got)
	}
}

func TestParseAuditExportArgs(t *testing.T) {
	// Valid: format + a couple of filters.
	format, f, errMsg := ParseAuditExportArgs([]string{"csv", "actor=main", "limit=50"})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if format != "csv" {
		t.Errorf("format = %q, want csv", format)
	}
	if f.Actor == nil || *f.Actor != domain.ToolActor("main") {
		t.Errorf("actor not parsed: %+v", f.Actor)
	}
	if f.Limit == nil || *f.Limit != 50 {
		t.Errorf("limit not parsed: %+v", f.Limit)
	}

	// Bad format.
	if _, _, e := ParseAuditExportArgs([]string{"xml"}); e == "" {
		t.Error("expected error for bad format")
	}
	// Empty value (the from= → epoch-0 trap).
	if _, _, e := ParseAuditExportArgs([]string{"json", "from="}); e == "" {
		t.Error("expected error for empty value")
	}
	// limit out of range.
	if _, _, e := ParseAuditExportArgs([]string{"json", "limit=99999"}); e == "" {
		t.Error("expected error for out-of-range limit")
	}
	// Unknown filter key.
	if _, _, e := ParseAuditExportArgs([]string{"json", "bogus=1"}); e == "" {
		t.Error("expected error for unknown key")
	}
}
