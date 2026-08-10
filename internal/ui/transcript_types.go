package ui

import "github.com/daintreehq/assistant/internal/domain"

// transcript_types.go holds the run-oriented transcript model
// plus the ordered-step turn model. The transcript folds a
// flat event stream into turns; a sealed cell becomes an immutable ScrollbackBlock.

// NoteLevel tones a standalone operational note.
type NoteLevel int

const (
	NoteInfo NoteLevel = iota
	NoteWarn
	NoteError
	NoteSuccess // green ● — a settled-good operational event
)

// TurnState is the lifecycle of a turn cell. A turn seals (commits to scrollback)
// the moment it leaves TurnActive.
type TurnState int

const (
	TurnActive TurnState = iota
	TurnComplete
	TurnFailed
	TurnCancelled
)

// TurnStepKind tags an ordered step within a turn. The ordered model preserves the
// true chronological narrative (prose → tool batch → more prose) instead of a flat
// "all prose then all tools" layout.
type TurnStepKind int

const (
	StepStatus TurnStepKind = iota
	StepProse
	StepTool
	StepNote
	StepInterject // a message the user typed mid-turn, folded into the running turn
	StepSkill     // a server-side skill load, folded into the running turn as a card (Text = skill name)
)

// ActivityState is the per-tool lifecycle the activity tree renders.
type ActivityState int

const (
	ActQueued ActivityState = iota
	ActActive
	ActDone
	ActFailed
	ActWaiting
	// ActCancelled is a terminal state synthesized by the UI (never emitted by the
	// agent): a turn the user aborted leaves calls[c:] announced-but-never-resolved
	// (the backend stubs their MODEL-side results but emits no UI ToolResult), so
	// without re-stamping them they would freeze into scrollback rendered as ◦ queued
	// / ◌ active, falsely implying they will still run. See (*TurnCell).cancelPending.
	ActCancelled
	// ActAsyncPending marks a tool call that returned an ACCEPTED async handle
	// (ToolResult.Async): the call itself is settled — this row never mutates
	// again, so it is terminal for the flush/seal — but the underlying work keeps
	// running in the background and its completion arrives later as a separate
	// wake turn. Rendered as a yellow "running asynchronously" row.
	ActAsyncPending
)

// settledForFlush reports whether a tool activity's state can no longer change
// within THIS turn — the single predicate the incremental row flush uses to
// decide a row is safe to freeze into immutable native scrollback. Defined
// next to the ActivityState consts so a NEW state cannot be added without
// deciding its flush semantics here (an inline set in flush.go silently
// defaulted new states to "still mutating", which pins the footer). Done and
// failed are settled results; async-pending never mutates in place (its
// completion arrives as a separate wake turn); cancelled is terminal by
// definition (today it is only synthesized at seal, where the flush already
// commits everything, but the semantics hold if that ever changes).
func (s ActivityState) settledForFlush() bool {
	switch s {
	case ActDone, ActFailed, ActAsyncPending, ActCancelled:
		return true
	default:
		return false
	}
}

// Activity is one branch in the turn's activity tree: a delegated unit of work.
type Activity struct {
	ID          string
	Name        string // internal tool name (mapped to a human verb at render)
	Args        string // raw JSON args (verbatim; revealed under ^X)
	State       ActivityState
	Detail      string // target / summary line (settled result summary)
	ProgressMsg string // live in-tool substep while active ("launching terminal")
	Outcome     string // failure summary (shown alongside target on failure)
	StartedAt   int64
	EndedAt     int64
}

// SystemNote is an inline note attached to a turn (vs a standalone NoteCell).
type SystemNote struct {
	Level NoteLevel
	Text  string
}

// TurnStep is one ordered step. Exactly one payload is meaningful per Kind.
type TurnStep struct {
	Kind      TurnStepKind
	Phase     domain.RunPhase // StepStatus
	Text      string          // StepProse
	Streaming bool            // StepProse: last line is still live (caret)
	Activity  *Activity       // StepTool
	Note      *SystemNote     // StepNote
}

// TurnCell is one request → decision → delegated work → outcome. It uses ordered
// Steps (not flat text+activities).
type TurnCell struct {
	ID             string
	UserText       string // empty for system-origin turns (e.g. a scheduled wake)
	Steps          []TurnStep
	State          TurnState
	Phase          domain.RunPhase // fine-grained live phase (drives LiveRunStatus)
	PhaseStartedAt int64           // epoch ms; drives the live "· 0.4s" elapsed
	Reasoning      string          // <think> body, revealed under ^X
	Ts             int64

	// StartedAt stamps when the turn began RUNNING (not when it was queued), driving the
	// CUMULATIVE live elapsed so it doesn't reset to 0 on every phase transition.
	// LastActivityAt stamps the most recent streamed token / tool event; a long gap means
	// the turn is stalled (vs. hung), surfaced as a "still working" cue in the live status.
	StartedAt      int64
	LastActivityAt int64

	// FlushedRows is the count of this turn's rendered rows (against the canonical
	// activeTurnRows form: leading blank separator + body, UN-indented — LeftPad is added
	// at commit + footer-assembly so the COUNT matches both) already committed to native
	// scrollback by the incremental row flush (flush.go). The inline cockpit anchors its
	// View at the bottom; if the live View grows taller than the terminal the host scrolls
	// its own top into scrollback, freezing a partial copy. So each completed (FINAL) row
	// of the active turn is tea.Println'd the instant it can no longer change, advancing
	// FlushedRows; the footer renders only rows >= FlushedRows and the seal commits only
	// rows >= FlushedRows. The footer therefore stays ~[in-progress line + status +
	// composer] tall and completed lines flow into native scrollback AS THEY STREAM (the
	// host scrolls them up, the input stays pinned) — exactly the desired behaviour.
	//
	// The streaming caret rides ONLY the live LAST step (render_turn.go), so a flushed row
	// can never carry a "▌"; the flushed rows are therefore byte-identical to what the seal
	// re-renders, and sealTail strips them exactly — no duplication.
	FlushedRows int

	// flushedRowsText is the exact canonical rows[0:FlushedRows] (un-indented, "\n"-joined)
	// already committed. It is the REFLOW guard: glamour can re-wrap an already-flushed
	// prefix as more text streams in (a numbered list joining into one paragraph). Before
	// advancing we require the fresh render's prefix to still equal this; if it diverged we
	// HOLD (the seal commits the whole remaining tail consistently). Greedy word-wrapped
	// prose — the common case — never diverges, so it always advances.
	flushedRowsText string
}

// NoteCell is a standalone operational note outside any turn.
type NoteCell struct {
	ID    string
	Level NoteLevel
	// Severity, when set, is the precise queue severity of an attention-routed note.
	// The renderer uses it to draw ONE precise glyph on the spine (toned by Level)
	// rather than the coarse level glyph plus a duplicate baked into Text. Empty for
	// ordinary notes (log lines, "Turn cancelled"), which fall back to the level glyph.
	Severity domain.Severity
	// Muted renders the note BODY at reduced weight (dim) while keeping the
	// level-derived spine + glyph. Presentation only — it never changes Level
	// semantics. For high-frequency lifecycle markers (the end-of-turn cue) that
	// must be legible without competing with the turn's own prose.
	Muted bool
	Text  string
	Ts    int64
}

// CommandCell is the result of a slash command rendered into the transcript.
type CommandCell struct {
	ID    string
	Title string
	Text  string
	Ts    int64
}

// TranscriptCell is the closed cell union. Exactly one pointer is non-nil.
type TranscriptCell struct {
	Turn    *TurnCell
	Note    *NoteCell
	Command *CommandCell
}

// ID returns the cell's stable id regardless of variant.
func (c TranscriptCell) ID() string {
	switch {
	case c.Turn != nil:
		return c.Turn.ID
	case c.Note != nil:
		return c.Note.ID
	case c.Command != nil:
		return c.Command.ID
	}
	return ""
}

// isSealed is the commit gate: a turn seals when it leaves the active state;
// standalone notes and command results are immutable the moment they arrive.
func (c TranscriptCell) isSealed() bool {
	if c.Turn != nil {
		return c.Turn.State != TurnActive
	}
	return true
}

// activeProseStep returns the index of the last StepProse, or -1. New tokens
// append to it; a fresh StepProse is opened when prose resumes after a tool batch.
func (t *TurnCell) activeProseStep() int {
	for i := len(t.Steps) - 1; i >= 0; i-- {
		if t.Steps[i].Kind == StepProse {
			return i
		}
		// A tool/note/interjection/skill step between us and the last prose means prose has
		// resumed: the caller must open a NEW StepProse rather than merging.
		if t.Steps[i].Kind == StepTool || t.Steps[i].Kind == StepNote || t.Steps[i].Kind == StepInterject || t.Steps[i].Kind == StepSkill {
			return -1
		}
	}
	return -1
}

// appendProse appends streamed text, opening a new StepProse when prose resumes
// after a tool/note step so the chronological narrative is preserved.
func (t *TurnCell) appendProse(text string) {
	if text == "" {
		return
	}
	if i := t.activeProseStep(); i >= 0 {
		t.Steps[i].Text += text
		t.Steps[i].Streaming = true
		return
	}
	t.Steps = append(t.Steps, TurnStep{Kind: StepProse, Text: text, Streaming: true})
}

// sealProse marks every prose step non-streaming (the turn is done streaming).
func (t *TurnCell) sealProse() {
	for i := range t.Steps {
		if t.Steps[i].Kind == StepProse {
			t.Steps[i].Streaming = false
		}
	}
}

// cancelPending re-stamps every tool activity still queued/active/waiting as
// ActCancelled when the user aborts the turn. The agent's stubCancelledFrom only
// pushes CANCELLED results into the MODEL's message history (so replay stays valid);
// it emits no UI ToolResult, so these rows would otherwise commit to scrollback
// still rendered as live/queued. A was-active call records "cancelled"; a never-started
// (queued/waiting) call records "not run" — both shown via Outcome at render time so the
// row's resolved target detail is preserved alongside the terminal note.
func (t *TurnCell) cancelPending() {
	for i := range t.Steps {
		if t.Steps[i].Kind != StepTool || t.Steps[i].Activity == nil {
			continue
		}
		a := t.Steps[i].Activity
		switch a.State {
		case ActActive:
			a.State = ActCancelled
			a.ProgressMsg = ""
			a.Outcome = "cancelled"
		case ActQueued, ActWaiting:
			a.State = ActCancelled
			a.Outcome = "not run"
		}
	}
}

// findActivity returns the Activity pointer for a tool-call id, or nil.
func (t *TurnCell) findActivity(id string) *Activity {
	for i := range t.Steps {
		if t.Steps[i].Kind == StepTool && t.Steps[i].Activity != nil && t.Steps[i].Activity.ID == id {
			return t.Steps[i].Activity
		}
	}
	return nil
}
