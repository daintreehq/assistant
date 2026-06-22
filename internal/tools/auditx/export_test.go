package auditx

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// fakeAuditStore returns rows newest-first, honoring the actor filter and limit
// (the slice the real store's QueryAudit would apply).
type fakeAuditStore struct {
	rows []domain.AuditRecord
}

func (s *fakeAuditStore) QueryAudit(f AuditFilters) ([]domain.AuditRecord, error) {
	var out []domain.AuditRecord
	for _, r := range s.rows {
		if f.Actor != nil && r.Actor != *f.Actor {
			continue
		}
		out = append(out, r)
	}
	// newest-first by ts.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	if f.Limit != nil && *f.Limit < len(out) {
		out = out[:*f.Limit]
	}
	return out, nil
}

func exportTool(store AuditStore) tools.Tool {
	return Tools(Deps{Store: store})[0]
}

func twoRows() *fakeAuditStore {
	return &fakeAuditStore{rows: []domain.AuditRecord{
		{ID: "aud_1", Ts: 1000, Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: "{}", Outcome: "ok", DurationMs: 1, Summary: "read"},
		{ID: "aud_2", Ts: 2000, Actor: domain.ActorWatcher, ToolName: "git.commit", ArgsJson: "{}", Outcome: "error", DurationMs: 2, Summary: "boom"},
	}}
}

func TestAuditExportToolReadRisk(t *testing.T) {
	tool := exportTool(twoRows())
	if tool.Name != "audit.export" || tool.Risk != domain.RiskRead {
		t.Fatalf("audit.export should be read-risk, got name=%q risk=%s", tool.Name, tool.Risk)
	}
}

// CSV export filters by actor and reports the count.
func TestAuditExportCSVFilteredWithCount(t *testing.T) {
	tool := exportTool(twoRows())
	res := tool.Handle(context.Background(), json.RawMessage(`{"format":"csv","actor":"main"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["format"] != "csv" || m["count"].(int) != 1 {
		t.Fatalf("format/count: %+v", m)
	}
	content := m["content"].(string)
	if header := strings.Split(content, "\r\n")[0]; header != strings.Join(AuditExportColumns, ",") {
		t.Fatalf("header: %q", header)
	}
	if !strings.Contains(content, "fs.read") || strings.Contains(content, "git.commit") {
		t.Fatalf("actor filter not applied: %q", content)
	}
}

// JSON export honors the limit and returns rows newest-first.
func TestAuditExportJSONHonorsLimit(t *testing.T) {
	tool := exportTool(twoRows())
	res := tool.Handle(context.Background(), json.RawMessage(`{"format":"json","limit":1}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["count"].(int) != 1 {
		t.Fatalf("count: %v", m["count"])
	}
	var parsed []domain.AuditRecord
	if err := json.Unmarshal([]byte(m["content"].(string)), &parsed); err != nil {
		t.Fatalf("content not JSON: %v", err)
	}
	if len(parsed) != 1 || parsed[0].Ts != 2000 {
		t.Fatalf("expected newest row only, got %+v", parsed)
	}
}
