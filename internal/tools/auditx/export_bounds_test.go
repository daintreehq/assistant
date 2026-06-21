package auditx

import (
	"encoding/json"
	"testing"
)

// Finding 4: audit.export limit must be int().min(1).max(5000) — a negative or
// oversized row cap is rejected at decode rather than forwarded unbounded to the
// store query.
func TestAuditExportRejectsOutOfBoundsLimit(t *testing.T) {
	tool := exportTool(twoRows())
	for _, bad := range []string{
		`{"format":"json","limit":-1}`,
		`{"format":"json","limit":0}`,
		`{"format":"json","limit":5001}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("out-of-bounds limit should be rejected: %s", bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"format":"json","limit":50}`)); err != nil {
		t.Errorf("valid limit should decode: %v", err)
	}
}
