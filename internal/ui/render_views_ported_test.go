package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
)

// render_views_ported_test.go exercises the rendered-view assertions for the
// ActivityTree, ApprovalSheet, OperationsView, and Transcript on the Go render
// helpers (which return styled strings; we strip ANSI and assert on the visible
// text + branch/width invariants).

// --- ActivityTree ---

func TestActivityRow_FailureSummaryAlongsideTarget(t *testing.T) {
	th := darkTheme()
	a := Activity{ID: "f", Name: "fs.read", State: ActFailed, Detail: "missing.ts",
		Outcome: "ENOENT: no such file", StartedAt: 0, EndedAt: 5}
	row := stripAnsi(renderActivityRow(th, a, true, false, 0, 5, 72))
	if !strings.Contains(row, "Read") {
		t.Errorf("verb missing: %q", row)
	}
	if !strings.Contains(row, "missing.ts") {
		t.Errorf("target must still be shown on a failed row: %q", row)
	}
	if !strings.Contains(row, "ENOENT") {
		t.Errorf("failure summary must be shown alongside the target: %q", row)
	}
}

func TestActivityRow_StaticGlyphOnNonLiveActiveRow(t *testing.T) {
	// A scrollback (non-live) render of an active row must show the STATIC active glyph
	// (spinnerFrame 0 of the spinner is the static base), never an animated mid-frame
	// that would freeze in scrollback. We assert the row is renderable and stable.
	th := darkTheme()
	a := Activity{ID: "a", Name: "fs.read", State: ActActive, StartedAt: 0}
	row := stripAnsi(renderActivityRow(th, a, true, false, 0, 0, 72))
	if !strings.Contains(row, "Read") {
		t.Errorf("active row missing verb: %q", row)
	}
}

func TestActivityRow_SquareLastBranchNotArc(t *testing.T) {
	th := darkTheme()
	// The last-branch glyph is the square └─, never the rounded arc ╰.
	if th.Glyphs.BranchLast != "└─" {
		t.Fatalf("BranchLast = %q, want square └─", th.Glyphs.BranchLast)
	}
	// Distinct, non-compactable tools so the multi-row branch tree renders (a finished
	// homogeneous read batch now compacts to one summary row — see compaction_test.go);
	// this test is about the ├─/└─ grammar, not compaction.
	acts := []Activity{
		{ID: "a", Name: "agentTask.spawnForEdits", State: ActDone, StartedAt: 0, EndedAt: 5},
		{ID: "b", Name: "watcher.terminal.create", State: ActDone, StartedAt: 0, EndedAt: 5},
	}
	group := stripAnsi(renderToolGroup(th, acts, false, 0, 5, 72))
	if !strings.Contains(group, "├─") || !strings.Contains(group, "└─") {
		t.Errorf("branch grammar missing both ├─ and └─: %q", group)
	}
	if strings.Contains(group, "╰") {
		t.Errorf("the arc ╰ must never reach the screen: %q", group)
	}
}

func TestActivityRow_LongDetailTruncatesAwayFromDuration(t *testing.T) {
	th := darkTheme()
	a := Activity{ID: "b", Name: "context.snapshot", State: ActDone,
		Detail: "Daintree MCP connected via streamable-http (12 tools).", StartedAt: 0, EndedAt: 1}
	row := stripAnsi(renderActivityRow(th, a, true, false, 0, 1, 40))
	if cellWidth(row) > 40 {
		t.Errorf("row width %d exceeds 40: %q", cellWidth(row), row)
	}
	if !strings.Contains(row, "…") {
		t.Errorf("long detail must be truncated with an ellipsis: %q", row)
	}
}

// --- ApprovalSheet ---

func confirmReq(tool string, risk domain.RiskClass, consequence string) tools.ConfirmRequest {
	return tools.ConfirmRequest{
		ToolName:    tool,
		Risk:        risk,
		Summary:     "the branch is ready for review",
		Consequence: consequence,
		Args:        []byte(`{"branch":"fix/x","remote":"origin"}`),
	}
}

func TestApproval_LeadsWithConsequenceToolNameSecondary(t *testing.T) {
	th := darkTheme()
	req := confirmReq("git.push", domain.RiskGit, "Pushes your branch to the remote, visible to collaborators.")
	out := stripAnsi(renderApproval(th, &pendingConfirm{req: req}, 72))
	if !strings.Contains(out, "Push branch to origin?") {
		t.Errorf("title missing: %q", out)
	}
	if !strings.Contains(out, "affects") {
		t.Errorf("consequence lead label 'affects' missing: %q", out)
	}
	if !strings.Contains(out, "Pushes your branch to the remote") {
		t.Errorf("consequence prose missing: %q", out)
	}
	if !strings.Contains(out, "git.push") {
		t.Errorf("tool name must stay visible (dim secondary): %q", out)
	}
	if !strings.Contains(out, "approve") || !strings.Contains(out, "decline") {
		t.Errorf("approve/decline actions missing: %q", out)
	}
}

func TestApproval_NeverRendersRawRiskAsField(t *testing.T) {
	out := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: confirmReq("git.push", domain.RiskGit, "")}, 72))
	// The old "risk  git" labelled row is gone — consequence language replaces it.
	if strings.Contains(out, "risk     git") || strings.Contains(out, "risk  git") {
		t.Errorf("raw risk-class field must not render: %q", out)
	}
}

func TestApproval_PerRiskConsequenceFallback(t *testing.T) {
	// A blank/whitespace consequence falls back to the per-risk prose, never an empty
	// line. Every one of the 8 risk classes yields a non-empty consequence.
	risks := []domain.RiskClass{
		domain.RiskRead, domain.RiskLocal, domain.RiskUI, domain.RiskTerminal,
		domain.RiskProject, domain.RiskGit, domain.RiskExternal, domain.RiskSystem,
	}
	for _, r := range risks {
		out := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: confirmReq("some.tool", r, "   ")}, 72))
		var affects string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "affects") {
				affects = strings.TrimSpace(strings.Replace(l, "affects", "", 1))
				break
			}
		}
		if affects == "" {
			t.Errorf("risk %q: affects row carries no prose: %q", r, out)
		}
		if affects == string(r) {
			t.Errorf("risk %q: affects row is the bare risk word, not prose", r)
		}
	}
	// System gives a system-level gloss.
	sys := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: confirmReq("daintree.call", domain.RiskSystem, "")}, 72))
	if !strings.Contains(sys, "system-level action") {
		t.Errorf("system fallback missing: %q", sys)
	}
}

func TestApproval_HidesReasonArgsUntilInspect(t *testing.T) {
	th := darkTheme()
	req := confirmReq("git.push", domain.RiskExternal, "")
	collapsed := stripAnsi(renderApproval(th, &pendingConfirm{req: req}, 72))
	if strings.Contains(collapsed, "ready for review") || strings.Contains(collapsed, "fix/x") {
		t.Errorf("reason/args must be hidden until inspect: %q", collapsed)
	}
	expanded := stripAnsi(renderApproval(th, &pendingConfirm{req: req, showArgs: true}, 72))
	if !strings.Contains(expanded, "ready for review") {
		t.Errorf("reason must reveal under inspect: %q", expanded)
	}
	if !strings.Contains(expanded, "fix/x") {
		t.Errorf("args must reveal under inspect: %q", expanded)
	}
}

func TestApproval_TerminalInputTitledDistinctly(t *testing.T) {
	out := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: confirmReq("terminal.sendInput", domain.RiskTerminal, "")}, 72))
	if !strings.Contains(out, "Send input to terminal?") {
		t.Errorf("terminal-input title missing: %q", out)
	}
}

// --- OperationsView ---

func opsDash(over func(*Dashboard)) Dashboard {
	d := Dashboard{}
	if over != nil {
		over(&d)
	}
	return d
}

func TestOps_OrdersSectionsAndMergesAgents(t *testing.T) {
	d := opsDash(func(d *Dashboard) {
		d.Watchers = []domain.WatcherRecord{watcherRec("wch_1", string(domain.ClassStillWorking), nil)}
		d.Agents = BuildAgentRows(d.Watchers)
		d.Agents[0].ID = "term_8"
	})
	out := stripAnsi(renderOperations(darkTheme(), d, PanelNone, 0, 72))
	if !strings.Contains(out, "NOW") || !strings.Contains(out, "AGENTS") {
		t.Errorf("section headers missing: %q", out)
	}
	if !strings.Contains(out, "term_8") {
		t.Errorf("supervised terminal id missing: %q", out)
	}
	// NOW comes before AGENTS (priority order).
	if strings.Index(out, "NOW") > strings.Index(out, "AGENTS") {
		t.Error("NOW must come before AGENTS")
	}
}

func TestOps_AgentRowEpistemicProvenance(t *testing.T) {
	obs := domain.EpistemicObserved
	d := opsDash(func(d *Dashboard) {
		d.Watchers = []domain.WatcherRecord{watcherRec("wch_1", string(domain.ClassTerminalExited), &obs)}
		d.Agents = BuildAgentRows(d.Watchers)
	})
	out := stripAnsi(renderOperations(darkTheme(), d, PanelNone, 0, 72))
	if !strings.Contains(out, "obs") {
		t.Errorf("agent row must carry the 'obs' epistemic tag: %q", out)
	}
}

func TestOps_AttentionEventEpistemicProvenance(t *testing.T) {
	d := opsDash(func(d *Dashboard) {
		d.Inbox = []domain.QueueEvent{{
			Title: "Tests failed in term_8", Summary: "3 failures",
			Severity: domain.SeverityUrgent, Count: 1, EpistemicKind: domain.EpistemicInferred,
		}}
	})
	out := stripAnsi(renderOperations(darkTheme(), d, PanelNone, 0, 72))
	if !strings.Contains(out, "NEEDS ATTENTION") {
		t.Errorf("attention section missing: %q", out)
	}
	if !strings.Contains(out, "Tests failed in term_8") || !strings.Contains(out, "3 failures") {
		t.Errorf("attention title/summary missing: %q", out)
	}
	if !strings.Contains(out, "inf") {
		t.Errorf("attention event must carry the 'inf' tag: %q", out)
	}
}

func TestOps_HidesEmptySectionsStandingBy(t *testing.T) {
	out := stripAnsi(renderOperations(darkTheme(), opsDash(nil), PanelNone, 0, 72))
	if strings.Contains(out, "RECENT") || strings.Contains(out, "SCHEDULED") {
		t.Errorf("empty audit/timer sections must vanish: %q", out)
	}
	if !strings.Contains(out, "Standing by") {
		t.Errorf("idle NOW section should read 'Standing by': %q", out)
	}
}

func TestOps_FocusedPanelRendersOnlyThatSection(t *testing.T) {
	full := opsDash(func(d *Dashboard) {
		d.Watchers = []domain.WatcherRecord{watcherRec("wch_1", string(domain.ClassStillWorking), nil)}
		d.Agents = BuildAgentRows(d.Watchers)
		d.Agents[0].ID = "term_8"
		d.Inbox = []domain.QueueEvent{{Title: "Tests failed in term_8", Severity: domain.SeverityUrgent, Count: 1}}
		d.Timers = []domain.TimerRecord{{Title: "nudge", FireAt: 1}}
		d.Audit = []domain.AuditRecord{{ToolName: "git.push", Outcome: "ok", DurationMs: 5}}
	})
	cases := []struct {
		panel  PanelKey
		label  string
		marker string
	}{
		{PanelWatchers, "AGENTS", "term_8"},
		{PanelInbox, "NEEDS ATTENTION", "Tests failed in term_8"},
		{PanelTimers, "SCHEDULED", "nudge"},
		{PanelAudit, "RECENT", "git.push"}, // raw tool name (r.toolName shown verbatim)
	}
	allLabels := []string{"NEEDS ATTENTION", "AGENTS", "SCHEDULED", "RECENT"}
	for _, c := range cases {
		out := stripAnsi(renderOperations(darkTheme(), full, c.panel, 0, 72))
		if !strings.Contains(out, c.label) || !strings.Contains(out, c.marker) {
			t.Errorf("panel %q: label %q / marker %q missing: %q", c.panel, c.label, c.marker, out)
		}
		for _, other := range allLabels {
			if other != c.label && strings.Contains(out, other) {
				t.Errorf("panel %q leaked other section %q: %q", c.panel, other, out)
			}
		}
	}
}

func TestOps_FocusedEmptyPanelHonestPlaceholder(t *testing.T) {
	out := stripAnsi(renderOperations(darkTheme(), opsDash(nil), PanelTimers, 0, 72))
	if strings.Contains(out, "SCHEDULED") {
		t.Errorf("empty focused timers section header should not render: %q", out)
	}
	if !strings.Contains(out, "Nothing here yet.") {
		t.Errorf("focused-empty panel must show an honest placeholder: %q", out)
	}
}

// --- Transcript ---

func transcriptTurn() *TurnCell {
	return &TurnCell{
		ID:       "t1",
		UserText: "Fix the watcher tests.",
		State:    TurnActive,
		Steps: []TurnStep{
			{Kind: StepProse, Text: "I'll delegate and supervise."},
			{Kind: StepTool, Activity: &Activity{ID: "c1", Name: "fs.search", State: ActDone,
				Detail: "8 matches", Args: `{"query":"watcher"}`, StartedAt: 1000, EndedAt: 1180}},
			{Kind: StepTool, Activity: &Activity{ID: "c3", Name: "watcher.terminal.create", State: ActActive,
				Detail: "tests running", StartedAt: 0}},
		},
	}
}

func TestTranscript_RunMarkersBranchVerbs(t *testing.T) {
	th := darkTheme()
	out := stripAnsi(renderTurn(th, markdown.New(th), transcriptTurn(), 72, 70, false, 0, 200))
	if !strings.Contains(out, "YOU") || !strings.Contains(out, "▏") {
		t.Errorf("human turn must carry YOU + the ▏ accent bar: %q", out)
	}
	if !strings.Contains(out, "DAINTREE") {
		t.Errorf("DAINTREE marker missing: %q", out)
	}
	if strings.Contains(out, "assistant") {
		t.Errorf("no raw role label: %q", out)
	}
	// Human verbs, never raw fn() / JSON.
	if !strings.Contains(out, "Searched") || !strings.Contains(out, "Watching") {
		t.Errorf("human verbs missing: %q", out)
	}
	if strings.Contains(out, "watcher.terminal.create(") || strings.Contains(out, `"query"`) {
		t.Errorf("raw fn/JSON must not appear in the collapsed view: %q", out)
	}
	if !strings.Contains(out, "├") && !strings.Contains(out, "└") {
		t.Errorf("branch grammar missing: %q", out)
	}
	if !strings.Contains(out, "180ms") {
		t.Errorf("settled duration missing: %q", out)
	}
}

func TestTranscript_ExpandedRevealsArgs(t *testing.T) {
	th := darkTheme()
	out := stripAnsi(renderTurn(th, markdown.New(th), transcriptTurn(), 72, 70, true, 0, 200))
	if !strings.Contains(out, "query") {
		t.Errorf("expanded mode must reveal raw args: %q", out)
	}
}
