package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/workflowgraph"
)

func TestOps_WorkflowsSectionRendersRows(t *testing.T) {
	d := opsDash(func(d *Dashboard) {
		d.WorkflowGraphs = []WorkflowGraphRow{
			{ID: "wfg_11223344", Goal: "Fix watcher tests", Status: "active",
				Progress: "3/5 done · current: Run tests", Next: "Run watcher test target"},
			{ID: "wfg_55667788", Goal: "Prepare PR for issue 286", Status: "blocked",
				Progress: "1/4 done", Blocked: true},
		}
	})
	out := stripAnsi(renderOperations(darkTheme(), d, PanelNone, 0, 72))
	if !strings.Contains(out, "WORKFLOWS") {
		t.Fatalf("WORKFLOWS section missing: %q", out)
	}
	if !strings.Contains(out, "Fix watcher tests") || !strings.Contains(out, "wfg_11223344") {
		t.Errorf("workflow goal/id missing: %q", out)
	}
	if !strings.Contains(out, "3/5 done") || !strings.Contains(out, "next: Run") {
		t.Errorf("progress/next missing: %q", out)
	}
	if !strings.Contains(out, "⚠") {
		t.Errorf("blocked workflow should carry the warning marker: %q", out)
	}
}

func TestOps_WorkflowsSectionVanishesWhenEmpty(t *testing.T) {
	out := stripAnsi(renderOperations(darkTheme(), opsDash(nil), PanelNone, 0, 72))
	if strings.Contains(out, "WORKFLOWS") {
		t.Errorf("feature off / no graphs must render no WORKFLOWS section: %q", out)
	}
}

// TestOps_WorkflowBlockedMarkerAsciiSafe proves the blocked-workflow marker degrades to
// the ASCII alert glyph instead of a raw unicode ⚠ when unicode is off.
func TestOps_WorkflowBlockedMarkerAsciiSafe(t *testing.T) {
	d := opsDash(func(d *Dashboard) {
		d.WorkflowGraphs = []WorkflowGraphRow{
			{ID: "wfg_1", Goal: "Prepare PR", Status: "blocked", Progress: "1/4 done", Blocked: true},
		}
	})
	out := stripAnsi(renderOperations(asciiTheme(t), d, PanelNone, 0, 72))
	if strings.Contains(out, "⚠") || strings.Contains(out, "●") {
		t.Errorf("workflow markers must not emit unicode glyphs in ascii mode: %q", out)
	}
	if !strings.Contains(out, "!") {
		t.Errorf("blocked workflow should use the ascii alert glyph: %q", out)
	}
}

func TestOps_WorkflowRowsTruncateAtNarrowWidth(t *testing.T) {
	long := strings.Repeat("supercalifragilistic goal text ", 10)
	d := opsDash(func(d *Dashboard) {
		d.WorkflowGraphs = []WorkflowGraphRow{{ID: "wfg_11223344", Goal: long, Status: "active", Progress: "0/9 done"}}
	})
	for _, width := range []int{72, 40} {
		out := renderOperations(darkTheme(), d, PanelNone, 0, width)
		for _, line := range strings.Split(stripAnsi(out), "\n") {
			if n := len([]rune(line)); n > width {
				t.Errorf("width %d: line overflows (%d runes): %q", width, n, line)
			}
		}
	}
}

func TestOps_WorkflowsCapWithMoreTrailer(t *testing.T) {
	d := opsDash(func(d *Dashboard) {
		for i := 0; i < 5; i++ {
			d.WorkflowGraphs = append(d.WorkflowGraphs, WorkflowGraphRow{
				ID: "wfg_0000000" + string(rune('0'+i)), Goal: "goal", Status: "active", Progress: "0/1 done",
			})
		}
	})
	out := stripAnsi(renderOperations(darkTheme(), d, PanelNone, 0, 72))
	if !strings.Contains(out, "+2 more") {
		t.Errorf("rows past the cap should collapse to a +N trailer: %q", out)
	}
}

func TestBuildWorkflowGraphRows(t *testing.T) {
	g := &workflowgraph.Graph{
		ID: "wfg_11223344", Goal: "fix tests", Status: workflowgraph.StatusActive, SchemaVersion: 1,
		Nodes: []workflowgraph.Node{
			{ID: "a", Title: "Orient", Kind: workflowgraph.KindOrient, Status: workflowgraph.NodeDone},
			{ID: "b", Title: "Run tests", Kind: workflowgraph.KindVerify, Status: workflowgraph.NodeRunning},
		},
		Blockers:   []workflowgraph.Blocker{{ID: "b1", Reason: "stuck"}},
		NextAction: &domain.RecommendedAction{Label: "Verify results", ToolName: "workflow.next"},
	}
	rows := buildWorkflowGraphRows([]*workflowgraph.Graph{g})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.Progress != "1/2 done · current: Run tests" {
		t.Errorf("unexpected progress: %q", r.Progress)
	}
	if !r.Blocked {
		t.Error("open blockers must mark the row blocked")
	}
	if r.Next != "Verify results" {
		t.Errorf("unexpected next: %q", r.Next)
	}
}
