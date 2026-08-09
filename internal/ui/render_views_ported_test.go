package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
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

func TestActivityRow_InProgressVerbWhileLive(t *testing.T) {
	// The in-progress verb form ("Waiting") appears on exactly the NON-settled
	// states; every settled state uses the past-tense "Waited" — a live row must
	// not read as finished, and a finished one must not read as still running.
	th := darkTheme()
	cases := []struct {
		state ActivityState
		want  string
	}{
		{ActQueued, "Waiting"},
		{ActActive, "Waiting"},
		{ActWaiting, "Waiting"},
		{ActDone, "Waited"},
		{ActFailed, "Waited"},
		{ActCancelled, "Waited"},
		{ActAsyncPending, "Waited"},
	}
	for _, c := range cases {
		a := Activity{ID: "w", Name: "terminal.awaitAll", State: c.state, StartedAt: 0}
		if c.want == "Waited" {
			a.EndedAt = 5
		}
		row := stripAnsi(renderActivityRow(th, a, true, false, 0, 5, 72))
		if !strings.Contains(row, c.want) {
			t.Errorf("state %d: row %q, want verb %q", c.state, row, c.want)
		}
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

// The details below are representative artifact.read summaries — four successive
// pages of one 56394-char artifact plus its final partial page. They are COPIES of
// the producer's wording, NOT a contract with it: internal/tools/artifactx owns that
// format and pins it in its own test. What these tests own is the RENDERING property
// — that a detail whose varying part sits in the head stays distinguishable once the
// row truncates — so a producer rewording only has to be mirrored here, not caught.
const artifactPageDetail = "offset 10500: 3500/56394 chars, 42394 remaining — artifact_1091e529"

func TestActivityRow_ArtifactPagingProgressDistinctAtWidth80(t *testing.T) {
	// Issue #312: paging a large artifact produced a run of rows that all rendered
	// identically, because the old summary put the offset/remaining — the only varying
	// part — at the very end, well past the truncation budget. (At this geometry the
	// old details agreed through cell 56, so all four collapsed to one row.) A linear
	// walk therefore read as a stuck loop.
	//
	// Geometry: embedded 80 columns → chrome 77, minus prefix 5 + "Read artifact"+1 = 14
	// + duration 8 → 50 detail cells, of which 49 survive alongside the ellipsis. That
	// is comfortably enough: the offset ends by cell 26 even for a 19-digit value, so
	// distinct offsets cannot collide here for ANY artifact size. Far narrower terminals
	// (~40 columns leaves ~11 detail cells) can still collide, but no summary format
	// distinguishes arbitrary pages in ten cells.
	th := darkTheme()
	rowWidth := chromeWidth(80, gutterFor(true))

	cases := []struct{ detail, wantOffset, wantRemaining string }{
		{"offset 0: 3500/56394 chars, 52894 remaining — artifact_1091e529", "offset 0", "52894"},
		{"offset 3500: 3500/56394 chars, 49394 remaining — artifact_1091e529", "offset 3500", "49394"},
		{"offset 7000: 3500/56394 chars, 45894 remaining — artifact_1091e529", "offset 7000", "45894"},
		{artifactPageDetail, "offset 10500", "42394"},
	}

	seen := make(map[string]bool, len(cases))
	for i, c := range cases {
		// Every field but Detail is identical, so ONLY the summary can distinguish
		// these rows — exactly the situation the bug arose in.
		a := Activity{ID: "p", Name: "artifact.read", State: ActDone,
			Detail: c.detail, StartedAt: 0, EndedAt: 1}
		row := stripAnsi(renderActivityRow(th, a, true, false, 0, 1, rowWidth))
		if cellWidth(row) > rowWidth {
			t.Errorf("page %d: row width %d exceeds %d: %q", i, cellWidth(row), rowWidth, row)
		}
		// BOTH varying quantities must survive the truncation, not just one.
		for _, want := range []string{c.wantOffset, c.wantRemaining} {
			if !strings.Contains(row, want) {
				t.Errorf("page %d: %q missing from truncated row %q", i, want, row)
			}
		}
		seen[row] = true
	}
	if len(seen) != len(cases) {
		t.Errorf("successive paged reads must render as %d DISTINCT rows, got %d: %v",
			len(cases), len(seen), seen)
	}

	// The final page reads as an ending, not as another identical line.
	eof := Activity{ID: "p", Name: "artifact.read", State: ActDone,
		Detail:    "offset 56000: 394/56394 chars, end of artifact — artifact_1091e529",
		StartedAt: 0, EndedAt: 1}
	row := stripAnsi(renderActivityRow(th, eof, true, false, 0, 1, rowWidth))
	if !strings.Contains(row, "offset 56000") || !strings.Contains(row, "end of artifact") {
		t.Errorf("eof row must show its offset and the end marker: %q", row)
	}
}

func TestActivityRow_ArtifactSummaryFullIDRecoveredWhenExpanded(t *testing.T) {
	// The id is deliberately LAST, so it is what the ellipsis eats once the row is
	// tight — the deal this format makes to keep the paging numbers visible. Prove
	// the other half of that deal: expansion gets the callable id back.
	//
	// Embedded 90 columns is the geometry that exercises both halves at once: chrome
	// 87 → collapsed detail budget 60, which clips the 67-cell summary and takes the
	// id with it, while the expanded (^X) result line gets 87-12 = 75 and fits it whole.
	th := darkTheme()
	rowWidth := chromeWidth(90, gutterFor(true))
	a := Activity{ID: "p", Name: "artifact.read", State: ActDone,
		Detail: artifactPageDetail, StartedAt: 0, EndedAt: 1}

	collapsed := stripAnsi(renderActivityRow(th, a, true, false, 0, 1, rowWidth))
	if strings.Contains(collapsed, "artifact_1091e529") {
		t.Errorf("collapsed row should have clipped the trailing id: %q", collapsed)
	}
	if !strings.Contains(collapsed, "offset 10500") || !strings.Contains(collapsed, "42394") {
		t.Errorf("collapsed row must still carry the paging numbers: %q", collapsed)
	}

	out := stripAnsi(renderActivityRow(th, a, true, true, 0, 1, rowWidth))
	if !strings.Contains(out, "result: "+artifactPageDetail) {
		t.Errorf("expanded view must carry the complete summary: %q", out)
	}
	if !strings.Contains(out, "artifact_1091e529") {
		t.Errorf("expanded view must show the full callable artifact id: %q", out)
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
		// Mirror the dispatch/grant construction so cockpit fixtures gate typed-confirm
		// exactly as production does (the field is the single source of truth now).
		NeedsTypedConfirm: safety.NeedsTypedConfirm(risk),
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

func TestApproval_RendersRiskClassRow(t *testing.T) {
	// The approval sheet surfaces the safety taxonomy bucket as a labelled, column-
	// aligned dim row so the human sees WHICH risk class they're approving — not just
	// the consequence prose. (This inverts the earlier "never render raw risk" rule:
	// hiding the taxonomy at decision time was the bug, per issue #210.)
	for _, risk := range []domain.RiskClass{domain.RiskGit, domain.RiskSystem, domain.RiskTerminal} {
		out := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: confirmReq("some.tool", risk, "")}, 72))
		if !strings.Contains(out, "risk     "+string(risk)) {
			t.Errorf("risk %q: approval sheet must render the labelled risk row: %q", risk, out)
		}
	}
}

func TestApproval_TypedConfirmRenderPath(t *testing.T) {
	// With requireType set, the sheet renders the typed-confirm prompt (irreversible
	// warning + phrase) INSTEAD of the single-key action row, while still surfacing the
	// risk row. This is the highest-risk render branch and was previously unasserted.
	req := confirmReq("daintree.call", domain.RiskSystem, "")
	out := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: req, requireType: true}, 72))
	if !strings.Contains(out, "risk     system") {
		t.Errorf("typed-confirm sheet must still render the risk row: %q", out)
	}
	if !strings.Contains(out, "irreversible") || !strings.Contains(out, confirmPhrase) {
		t.Errorf("typed-confirm prompt missing the irreversible / type-phrase copy: %q", out)
	}
	// The single-key allow-list affordances must be absent in typed mode.
	if strings.Contains(out, "A allow") || strings.Contains(out, "F always") {
		t.Errorf("typed-confirm sheet must not show the single-key allow/always affordances: %q", out)
	}
}

func TestApproval_DaintreeCallTitleIsSpecific(t *testing.T) {
	// daintree.call (RiskSystem) is the raw MCP escape hatch; its title must NOT be the
	// generic system-level question that hides the riskiest forge writes.
	out := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: confirmReq("daintree.call", domain.RiskSystem, "")}, 72))
	if !strings.Contains(out, "Call a raw MCP tool?") {
		t.Errorf("daintree.call must get its specific title, not the generic system question: %q", out)
	}
	if strings.Contains(out, "Run a system-level action?") {
		t.Errorf("daintree.call still shows the generic system-level title: %q", out)
	}
	// A different RiskSystem tool keeps the generic phrasing (exact-name match only).
	other := stripAnsi(renderApproval(darkTheme(), &pendingConfirm{req: confirmReq("grant.create", domain.RiskSystem, "")}, 72))
	if !strings.Contains(other, "Run a system-level action?") {
		t.Errorf("a non-daintree.call RiskSystem tool should keep the generic title: %q", other)
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
		d.Agents = BuildAgentRows(d.Watchers, nil, nil)
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
		d.Agents = BuildAgentRows(d.Watchers, nil, nil)
	})
	out := stripAnsi(renderOperations(darkTheme(), d, PanelNone, 0, 72))
	if !strings.Contains(out, "obs") {
		t.Errorf("agent row must carry the 'obs' epistemic tag: %q", out)
	}
}

// TestOps_AgentBadgeTonedByUrgency proves the deck's agent badge is toned by urgency
// (a needs-input agent renders red, not flat cyan) so the deck agrees with the compact
// footer strip. Asserted on the STYLED output: the badge span must be the danger-toned
// render, and must NOT be the plain info-cyan render it used to be.
func TestOps_AgentBadgeTonedByUrgency(t *testing.T) {
	th := darkTheme()
	d := opsDash(func(d *Dashboard) {
		d.Watchers = []domain.WatcherRecord{watcherRec("wch_1", string(domain.ClassWaitingForInput), nil)}
		d.Agents = BuildAgentRows(d.Watchers, nil, nil)
	})
	out := renderOperations(th, d, PanelNone, 0, 72)
	if !strings.Contains(out, styleFor(th, "danger", "NEEDS INPUT")) {
		t.Errorf("needs-input agent badge must be danger-toned in the deck: %q", out)
	}
	if strings.Contains(out, th.Info().Render("NEEDS INPUT")) {
		t.Errorf("needs-input badge must not render flat info-cyan: %q", out)
	}

	// A normal working agent keeps the informational cyan — the "active" tone must not
	// fall through styleFor to plain body text (the regression Codex caught).
	working := opsDash(func(d *Dashboard) {
		d.Watchers = []domain.WatcherRecord{watcherRec("wch_2", string(domain.ClassStillWorking), nil)}
		d.Agents = BuildAgentRows(d.Watchers, nil, nil)
	})
	wout := renderOperations(th, working, PanelNone, 0, 72)
	if !strings.Contains(wout, th.Info().Render("WORKING")) {
		t.Errorf("working agent badge must render cyan (active tone), not plain body: %q", wout)
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
		d.Agents = BuildAgentRows(d.Watchers, nil, nil)
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
