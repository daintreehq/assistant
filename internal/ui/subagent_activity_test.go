package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// subagent_activity_test.go locks how a delegated sub-agent run LOOKS across its
// whole lifecycle. It matters more than a typical activity row for two reasons: a
// run can occupy a minute of wall clock, so a frozen row reads as a hang and gets
// cancelled; and its result is the one place a PARTIAL finding can be mistaken for
// a settled one.

func subagentTurn(args string) (*TurnCell, string) {
	cell := &TurnCell{ID: "turn_s", State: TurnActive, Phase: domain.PhaseGenerating, PhaseStartedAt: domain.NowMS()}
	cell.Steps = []TurnStep{
		{Kind: StepTool, Activity: &Activity{
			ID: "sub", Name: "subagent.run", State: ActActive, Args: args,
		}},
	}
	return cell, "sub"
}

const findIssueArgs = `{"task":"Find the GitHub issue describing the terrain mesh flicker at chunk borders","deliverable":"the issue number, title and URL"}`

// The row must name the BRIEF. A delegation the user cannot read is one they
// cannot judge, and the tool name alone says nothing about what was handed off.
func TestSubagentRowNamesTheBrief(t *testing.T) {
	th := darkTheme()
	cell, id := subagentTurn(findIssueArgs)

	row := stripAnsi(renderActivityRow(th, *cell.findActivity(id), false, false, 0, domain.NowMS(), 100))

	if !strings.Contains(row, "Sub-agent") {
		t.Errorf("row must carry the Sub-agent verb: %q", row)
	}
	// briefGist strips the "Find the" lead-in, so the distinctive words lead.
	if !strings.Contains(row, "GitHub issue") {
		t.Errorf("row must show the brief, not the tool name: %q", row)
	}
	if strings.Contains(row, "subagent.run") {
		t.Errorf("row shows the raw tool name instead of the verb: %q", row)
	}
	if strings.Contains(row, "{") || strings.Contains(row, "deliverable") {
		t.Errorf("row leaked raw JSON args: %q", row)
	}
}

// While the run is live, the row must show the sub-agent's own progress. Without
// it the cockpit shows one unchanging line for what can be a minute, and a
// delegation that looks hung is one the user kills.
func TestSubagentRowShowsLiveProgress(t *testing.T) {
	m := harnessModel()
	cell, id := subagentTurn(findIssueArgs)
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID

	m.applyPumpEvent(pumpEvent{kind: pumpToolProgress, toolID: id, msg: "round 3/10 · fs.search"})

	th := darkTheme()
	row := stripAnsi(renderActivityRow(th, *cell.findActivity(id), false, false, 0, domain.NowMS(), 100))
	if !strings.Contains(row, "round 3/10") {
		t.Errorf("a live sub-agent row must show its round counter: %q", row)
	}
	if !strings.Contains(row, "fs.search") {
		t.Errorf("a live sub-agent row must show what it is doing: %q", row)
	}
}

// A completed run settles to the report's headline counters.
func TestSubagentRowSettlesToTheSummary(t *testing.T) {
	m := harnessModel()
	cell, id := subagentTurn(findIssueArgs)
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID

	res := domain.Ok("Reported back · 3 rounds, 4 tool calls · 11.5s", nil)
	m.applyPumpEvent(pumpEvent{kind: pumpToolResult, result: agent.ToolResultEvent{ID: id, Result: res, EndedAt: 2}})

	th := darkTheme()
	row := stripAnsi(renderActivityRow(th, *cell.findActivity(id), true, false, 0, domain.NowMS(), 110))
	if !strings.Contains(row, th.Glyphs.Done) {
		t.Errorf("a completed run must carry the done glyph: %q", row)
	}
	if !strings.Contains(row, "3 rounds") {
		t.Errorf("the settled row must carry the run's counters: %q", row)
	}
}

// THE one that matters: a partial finding must never read as a settled one. The
// orchestrator acts on this result, and a truncated row that dropped the warning
// is how a half-answer becomes a wrong one.
func TestSubagentPartialResultStaysVisiblyPartial(t *testing.T) {
	m := harnessModel()
	cell, id := subagentTurn(findIssueArgs)
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID

	res := domain.Ok("PARTIAL — stopped early · 11 rounds, 22 tool calls · 32.8s", nil)
	m.applyPumpEvent(pumpEvent{kind: pumpToolResult, result: agent.ToolResultEvent{ID: id, Result: res, EndedAt: 2}})

	th := darkTheme()
	// Narrow width is the real risk: the detail budget truncates, and "PARTIAL"
	// sits early in the summary precisely so it survives the cut.
	for _, width := range []int{60, 80, 120} {
		row := stripAnsi(renderActivityRow(th, *cell.findActivity(id), true, false, 0, domain.NowMS(), width))
		if !strings.Contains(row, "PARTIAL") {
			t.Errorf("width %d dropped the PARTIAL warning: %q", width, row)
		}
	}
}

// A fan-out is the shape the model is told to prefer, so the tree has to stay
// readable when three of these run at once — each row naming its own brief.
func TestSubagentFanOutRowsAreDistinguishable(t *testing.T) {
	th := darkTheme()
	briefs := []string{
		`{"task":"Find the GitHub issue about terrain mesh flicker"}`,
		`{"task":"Locate every Go file that registers a tool family"}`,
		`{"task":"Find what schemaUserVersion is set to"}`,
	}
	seen := map[string]bool{}
	for i, args := range briefs {
		a := Activity{ID: "s", Name: "subagent.run", State: ActActive, Args: args}
		row := stripAnsi(renderActivityRow(th, a, false, false, 0, domain.NowMS(), 100))
		if seen[row] {
			t.Fatalf("brief %d rendered identically to an earlier one: %q", i, row)
		}
		seen[row] = true
	}
}

// A visual smoke print of the whole lifecycle. Not an assertion — it exists so a
// reviewer can SEE the rows this feature produces without running the binary.
func TestSubagentRowsVisualSample(t *testing.T) {
	th := darkTheme()
	base := Activity{ID: "sub", Name: "subagent.run", Args: findIssueArgs}

	queued := base
	queued.State = ActQueued

	running := base
	running.State = ActActive
	running.ProgressMsg = "round 3/10 · fs.search, fs.read"

	done := base
	done.State = ActDone
	done.Detail = "Reported back · 3 rounds, 4 tool calls · 11.5s"

	partial := base
	partial.State = ActDone
	partial.Detail = "PARTIAL — stopped early · 11 rounds, 22 tool calls · 32.8s"

	failed := base
	failed.State = ActFailed
	failed.Outcome = "The sub-agent could not complete the task"

	t.Log("\n" + strings.Join([]string{
		stripAnsi(renderActivityRow(th, queued, false, false, 0, domain.NowMS(), 96)),
		stripAnsi(renderActivityRow(th, running, false, false, 0, domain.NowMS(), 96)),
		stripAnsi(renderActivityRow(th, done, true, false, 0, domain.NowMS(), 96)),
		stripAnsi(renderActivityRow(th, partial, true, false, 0, domain.NowMS(), 96)),
		stripAnsi(renderActivityRow(th, failed, true, false, 0, domain.NowMS(), 96)),
	}, "\n"))
}

func TestBriefGist(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"find the", "Find the GitHub issue about terrain flicker", "GitHub issue about terrain flicker"},
		{"find every", "Find every Go file that registers a tool family", "Go file that registers a tool family"},
		{"locate", "Locate the schemaUserVersion constant", "schemaUserVersion constant"},
		{"in this repo", "In this repository, count the tool families", "count the tool families"},
		{"which", "Which package may import bubbletea", "package may import bubbletea"},
		{"case insensitive", "find the terrain mesh code", "terrain mesh code"},
		// Already distinctive — left exactly as written.
		{"no lead-in", "internal/tools/dispatch.go ownership", "internal/tools/dispatch.go ownership"},
		// A trim that would leave almost nothing keeps the original, so a row can
		// never render as an empty or cryptic fragment.
		{"trim would gut it", "Find the bug", "Find the bug"},
		{"only a lead-in", "Find", "Find"},
		{"empty", "", ""},
		// Word-boundary regression: "find" must not match "Finding", nor "what"
		// "Whatever". These rendered as "ing the relevant issue" and "ever causes
		// the failure" — a corrupted label is worse than the boilerplate it removes.
		{"finding is not find", "Finding the relevant issue", "Finding the relevant issue"},
		{"whatever is not what", "Whatever causes the failure", "Whatever causes the failure"},
		{"listing is not list", "Listing every handler", "Listing every handler"},
		{"searching is not search", "Searching the terrain code", "Searching the terrain code"},
		// A real word boundary still strips.
		{"find with boundary", "Find the terrain mesh code", "terrain mesh code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := briefGist(tc.in); got != tc.want {
				t.Errorf("briefGist(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The rule this encodes is a safety one: the identity prefix is a nicety, the
// outcome is not, so a cramped row drops the prefix rather than the outcome.
func TestFanOutDetail_OutcomeWinsWhenCramped(t *testing.T) {
	identity, volatile := "GitHub issue about terrain flicker", "PARTIAL — stopped early · 11 rounds"

	roomy := fanOutDetail(identity, volatile, 90)
	if !strings.HasPrefix(roomy, "GitHub issue") || !strings.Contains(roomy, volatile) {
		t.Errorf("with room, both halves must show: %q", roomy)
	}

	cramped := fanOutDetail(identity, volatile, len([]rune(volatile))+6)
	if cramped != volatile {
		t.Errorf("when cramped the prefix must be dropped whole, got %q", cramped)
	}

	// And never a stub of a prefix: below the minimum it is all or nothing.
	edge := fanOutDetail(identity, volatile, len([]rune(volatile))+3+fanOutMinIdentityCells-1)
	if edge != volatile {
		t.Errorf("a sub-minimum prefix must be dropped, got %q", edge)
	}
}
