package timer

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// listRows runs timer.list and returns its rows as the model sees them (JSON).
func listRows(t *testing.T, st *memStore) []map[string]any {
	t.Helper()
	res := find(Tools(Deps{Store: st}), "timer.list").Handle(context.Background(), nil, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("timer.list failed: %+v", res.Error)
	}
	raw, err := json.Marshal(res.Result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out struct {
		Timers []map[string]any `json:"timers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out.Timers
}

// A check-in loop scheduled with a linkage and bounds lists them back: the ledger row it
// drives, its deadline, its cap and how many runs are left. Without these a model
// reading timer.list cannot tell which loop belongs to which workflow run, or how far
// through it is.
func TestListExposesLinkageAndBounds(t *testing.T) {
	st := &memStore{}
	until := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	args := json.RawMessage(`{"title":"queue check","delayMs":600000,
		"repeat":{"everyMs":600000,"maxRuns":30,"until":"` + until + `"},
		"payload":{"type":"message","message":"check the queue"},
		"target":{"workflowRunId":"wfr_queue","worktreeId":"wt-1","terminalId":"term-9"}}`)
	if res := find(Tools(Deps{Store: st}), "timer.schedule").Handle(context.Background(), args, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("schedule: %+v", res.Error)
	}
	st.inserted[0].RunCount = 4 // four ticks already fired

	rows := listRows(t, st)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %v", rows)
	}
	r := rows[0]
	if r["repeatUntil"] != until || r["maxRuns"] != float64(30) || r["runsLeft"] != float64(26) ||
		r["repeatEveryMs"] != float64(600000) || r["payloadType"] != "message" {
		t.Fatalf("row bounds wrong: %v", r)
	}
	tgt, ok := r["target"].(map[string]any)
	if !ok || tgt["workflowRunId"] != "wfr_queue" || tgt["worktreeId"] != "wt-1" || tgt["terminalId"] != "term-9" {
		t.Fatalf("row target wrong: %v", r["target"])
	}
	if _, has := tgt["projectId"]; has {
		t.Fatalf("unset target fields must be omitted, got %v", tgt)
	}
}

// Rows without a linkage or bounds carry none of the new keys, and an unreadable stored
// target is omitted rather than failing the whole listing.
func TestListOmitsAbsentLinkageAndSurvivesABadTarget(t *testing.T) {
	bad := `{not json`
	st := &memStore{inserted: []domain.TimerRecord{
		{ID: "tmr_plain", Title: "plain", FireAt: domain.NowMS() + 60_000, Status: "scheduled", PayloadType: "enqueue"},
		{ID: "tmr_bad", Title: "bad", FireAt: domain.NowMS() + 60_000, Status: "scheduled", PayloadType: "enqueue", TargetJson: &bad},
	}}
	for _, r := range listRows(t, st) {
		for _, k := range []string{"target", "repeatUntil", "maxRuns", "runsLeft"} {
			if _, has := r[k]; has {
				t.Fatalf("%s: unexpected %q in %v", r["id"], k, r)
			}
		}
	}
}

// Read risk, always: listing is what a supervision loop does on every tick.
func TestListStaysReadRisk(t *testing.T) {
	if got := find(Tools(Deps{}), "timer.list").Risk; got != domain.RiskRead {
		t.Fatalf("timer.list risk = %q, want read", got)
	}
}
