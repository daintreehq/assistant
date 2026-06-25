package agent

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// goalAnchorMaxRunes bounds the originating-ask text copied into the goal anchor.
// The ask can be a large pasted block; the footer only needs enough to re-anchor
// the model on what it set out to do, not the whole payload. Rune-bounded (not
// byte-bounded) so a multibyte ask is never split mid-character.
const goalAnchorMaxRunes = 500

// activeWorkflowRunsLimit caps how many non-terminal runs the footer renders (and is
// the LIMIT handed to the store read). The footer is a re-anchoring glance at open
// work, not a full ledger dump — the newest handful of runs is enough; the model can
// call workflow.list for the rest. Defined here (not session.go) so the row cap and
// the query bound are one number in one place.
const activeWorkflowRunsLimit = 10

// workflowRunIDPreviewMax bounds how many terminal/watcher ids a single run row
// lists inline before collapsing the tail to a "(+N)" count. A handful of ids is
// directly actionable (terminal.read, watcher.status); beyond that the row just
// gets long without adding a usable handle.
const workflowRunIDPreviewMax = 3

// workflowRunFieldMaxRunes bounds the free-text fragments (branch, next-action
// label) copied into a run row, rune-safe, so one verbose field can't blow the row
// width. Short enough to stay scannable, long enough to identify the run.
const workflowRunFieldMaxRunes = 40

// footerContext is the input every turn-footer section renders from. It carries the
// turn's originating goal plus the snapshot of open work pre-fetched for this round.
// Passing a struct (not a widening parameter list) is the broadening the seam comment
// in this file anticipated: a future section that needs another fact adds a field
// here, touching neither the footerSection type nor any existing section body.
//
// WorkflowRuns is the already-fetched, already-bounded slice of non-terminal runs
// (the Session reads it best-effort before composing). Keeping the I/O in the Session
// and handing sections a plain slice keeps every section a PURE FORMATTER — unit-
// testable from a record literal, no store or fake required.
type footerContext struct {
	Goal         string
	WorkflowRuns []domain.WorkflowRunRecord
}

// footerSection renders one section of the turn footer from the turn's footerContext.
// It returns ("", false) to omit the section entirely (e.g. an empty goal, or no open
// runs).
//
// This is the forward-compatibility seam: later waves (memory recall, …) register
// additional sections in footerSections WITHOUT re-touching the Router.Stream call in
// session.go. The input is a footerContext so a section that needs more than the goal
// just reads another field — broaden footerContext here, in one file, when a section
// actually needs it, rather than plumbing speculative context now.
type footerSection func(ctx footerContext) (string, bool)

// footerSections is the ordered registry of turn-footer sections. Composed in
// declaration order into a single trailing system message. Package-local and
// mutable so tests can swap it (save/restore via t.Cleanup); production registers
// statically here and never mutates it at runtime.
var footerSections = []footerSection{goalAnchorSection, activeWorkflowRunsSection}

// composeTurnFooter builds the UNCACHED tail of the model request: zero or one
// system-role message appended AFTER the history snapshot in the Router.Stream
// call. Because it sits at the tail (never in the leading prefix), it is never
// part of the Fireworks prefix cache and is rebuilt fresh every round — editing
// turn-varying facts here can never invalidate the cached prefix. The result is
// ephemeral: it is appended only onto the snapshot slice handed to Stream and is
// NEVER pushed into s.messages, so durable history and token estimates are
// unaffected.
//
// Sections are joined with a blank line into ONE message (simpler than one
// message per section); a future section that genuinely needs its own message can
// be handled when it arrives. Returns nil when no section emits anything, so the
// caller's append is a no-op and the request is byte-identical to the pre-footer
// behaviour.
func composeTurnFooter(ctx footerContext) []models.ChatMessage {
	ctx.Goal = strings.TrimSpace(ctx.Goal)

	var parts []string
	for _, section := range footerSections {
		body, ok := section(ctx)
		if !ok {
			continue
		}
		if body = strings.TrimSpace(body); body == "" {
			continue
		}
		parts = append(parts, body)
	}
	if len(parts) == 0 {
		return nil
	}
	return []models.ChatMessage{models.TextMessage("system", strings.Join(parts, "\n\n"))}
}

// goalAnchorSection emits the `# Current goal` anchor: the turn's originating ask
// (truncated) plus a terse output-discipline line. Seeding the goal at the tail on
// every round counteracts goal drift in long, many-round turns without rewriting
// any cached early control message. Omitted entirely when the goal is blank.
//
// The anchor stays pinned to the ORIGINATING ask for the whole turn: a mid-turn
// redirect (InjectPrompt → foldInInjections) lands as a fresh user message in
// history, which the model weighs over this trailing system reminder — so the
// anchor intentionally does NOT chase injections (it would otherwise thrash the
// footer and lose the turn's through-line). The known asymmetry is acceptable
// because a recent user message outranks trailing system boilerplate.
func goalAnchorSection(ctx footerContext) (string, bool) {
	if ctx.Goal == "" {
		return "", false
	}
	return "# Current goal\n" + sliceChars(ctx.Goal, goalAnchorMaxRunes) +
		"\n\nStay focused on this goal. Finish it before stopping, and report what you did, not what remains.", true
}

// activeWorkflowRunsSection emits the `# Active workflow runs` block: one line per
// non-terminal run from the ledger, capped at activeWorkflowRunsLimit. It re-surfaces
// the model's own open work and branches into the prompt every round — the durable
// trace that, after a compaction, the summary note would otherwise be the only record
// of. Read-only: it renders the ledger, it never mutates it (Daintree owns execution).
// Omitted entirely (false) when there are no open runs, so the common no-open-work
// case adds nothing to the request.
//
// Each row is a single, scannable line carrying the handles a follow-up action needs:
// the run id, the issue/branch it tracks, the recommended next action, and the
// terminal/watcher ids it owns (so the model can read or watch them directly).
func activeWorkflowRunsSection(ctx footerContext) (string, bool) {
	if len(ctx.WorkflowRuns) == 0 {
		return "", false
	}
	runs := ctx.WorkflowRuns
	if len(runs) > activeWorkflowRunsLimit {
		runs = runs[:activeWorkflowRunsLimit]
	}

	var b strings.Builder
	b.WriteString("# Active workflow runs")
	for i := range runs {
		b.WriteString("\n")
		b.WriteString(renderWorkflowRunRow(runs[i]))
	}
	return b.String(), true
}

// renderWorkflowRunRow formats one non-terminal run as a single footer line:
//
//   - [active] wfr_ab12cd34  #255 feature/issue-255  →  Build footer (workflow.update)  terms: term_a1b2  watchers: wch_e5f6
//
// Optional fields degrade quietly: a run with no issue/branch drops those fragments,
// a missing/malformed next-action or id list renders "none". Nothing here can panic
// on bad ledger data — every pointer is nil-checked and every JSON blob is parsed
// leniently — because a footer must never break the turn it rides on.
func renderWorkflowRunRow(r domain.WorkflowRunRecord) string {
	var b strings.Builder
	b.WriteString("- [")
	b.WriteString(string(r.Status))
	b.WriteString("] ")
	b.WriteString(r.ID)

	if r.IssueNumber != nil {
		b.WriteString("  #")
		b.WriteString(strconv.Itoa(*r.IssueNumber))
	}
	if branch := cleanWorkflowField(r.Branch); branch != "" {
		b.WriteString("  ")
		b.WriteString(branch)
	}

	b.WriteString("  →  ")
	b.WriteString(compactNextAction(r.NextActionJson))

	b.WriteString("  terms: ")
	b.WriteString(compactIDList(r.TerminalIdsJson))
	b.WriteString("  watchers: ")
	b.WriteString(compactIDList(r.WatcherIdsJson))
	return b.String()
}

// compactNextAction previews a serialized domain.RecommendedAction as
// "label (toolName)" for the run row. It partial-unmarshals only the two fields the
// glance needs (never the full struct, whose Args can be large), and degrades to
// "none" when the json is nil, malformed, or label-less — a footer preview must
// tolerate any ledger blob, not assume a well-formed one.
func compactNextAction(nextActionJson *string) string {
	if nextActionJson == nil || strings.TrimSpace(*nextActionJson) == "" {
		return "none"
	}
	var preview struct {
		Label    string `json:"label"`
		ToolName string `json:"toolName"`
	}
	if err := json.Unmarshal([]byte(*nextActionJson), &preview); err != nil {
		return "none"
	}
	label := cleanWorkflowFieldStr(preview.Label)
	if label == "" {
		return "none"
	}
	if tool := strings.TrimSpace(preview.ToolName); tool != "" {
		return label + " (" + tool + ")"
	}
	return label
}

// compactIDList previews a JSON string-array of ids (terminal or watcher ids) as up
// to workflowRunIDPreviewMax space-joined ids with a "(+N)" tail for the rest. It
// returns "none" for a nil, empty, or malformed blob — the row stays well-formed on
// any ledger data. Blank entries are skipped so a stray "" never renders an empty id.
func compactIDList(idsJson *string) string {
	if idsJson == nil || strings.TrimSpace(*idsJson) == "" {
		return "none"
	}
	var ids []string
	if err := json.Unmarshal([]byte(*idsJson), &ids); err != nil {
		return "none"
	}
	cleaned := ids[:0]
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			cleaned = append(cleaned, id)
		}
	}
	if len(cleaned) == 0 {
		return "none"
	}
	shown := cleaned
	extra := 0
	if len(shown) > workflowRunIDPreviewMax {
		extra = len(shown) - workflowRunIDPreviewMax
		shown = shown[:workflowRunIDPreviewMax]
	}
	out := strings.Join(shown, " ")
	if extra > 0 {
		out += " (+" + strconv.Itoa(extra) + ")"
	}
	return out
}

// cleanWorkflowField collapses a nullable free-text field (branch) to a single line
// and rune-caps it, returning "" for nil/blank so the caller can drop the fragment.
func cleanWorkflowField(s *string) string {
	if s == nil {
		return ""
	}
	return cleanWorkflowFieldStr(*s)
}

// cleanWorkflowFieldStr normalizes a free-text fragment for a one-line row: it
// collapses every run of whitespace (incl. embedded newlines) to a single space and
// rune-caps the result, so a multiline or oversized ledger value can't break the row
// layout or split a multibyte rune.
func cleanWorkflowFieldStr(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return sliceChars(s, workflowRunFieldMaxRunes)
}
