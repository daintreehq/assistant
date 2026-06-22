package ui

import "github.com/daintreehq/daintree-assistant/internal/domain"

// transcript_types.go holds the run-oriented transcript model (ui-transcript.md §3)
// plus the ordered-step turn model (_interaction-ux.md §5). The transcript folds a
// flat event stream into turns; a sealed cell becomes an immutable ScrollbackBlock.

// NoteLevel tones a standalone operational note.
type NoteLevel int

const (
	NoteInfo NoteLevel = iota
	NoteWarn
	NoteError
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
// true chronological narrative (prose → tool batch → more prose) instead of the
// flat "all prose then all tools" the TS version produced.
type TurnStepKind int

const (
	StepStatus TurnStepKind = iota
	StepProse
	StepTool
	StepNote
)

// ActivityState is the per-tool lifecycle the activity tree renders (§4 glyphs).
type ActivityState int

const (
	ActQueued ActivityState = iota
	ActActive
	ActDone
	ActFailed
	ActWaiting
)

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
// Steps (not flat text+activities) per _interaction-ux.md §5.
type TurnCell struct {
	ID             string
	UserText       string // empty for system-origin turns (e.g. a scheduled wake)
	Steps          []TurnStep
	State          TurnState
	Phase          domain.RunPhase // fine-grained live phase (drives LiveRunStatus)
	PhaseStartedAt int64           // epoch ms; drives the live "· 0.4s" elapsed
	Queued         bool            // a follow-up typed while busy: dimmed, promoted in place
	Reasoning      string          // <think> body, revealed under ^X
	Ts             int64

	// FlushedRows is the count of this turn's rendered rows (against the canonical
	// activeTurnRows form: leading blank separator + LeftPad-indented body) already
	// committed to native scrollback by the incremental-flush path (update.go). The
	// streaming "output stopped then repeated" bug came from the whole in-flight turn
	// living in the live footer and GROWING row by row — in inline mode a View taller
	// than the screen scrolls its own top into native scrollback, freezing a PARTIAL
	// copy (caret and all) there. The fix keeps the footer ~constant-height by
	// tea.Println'ing each turn row the instant it goes FINAL (won't change) and
	// advancing FlushedRows; the footer then renders only rows >= FlushedRows, and the
	// seal commits only rows >= FlushedRows (so the flushed prefix is never committed
	// twice). See flushActiveTurn / liveCellsView / sealedBlock.
	FlushedRows int

	// flushedRowsText is the exact canonical rows[0:FlushedRows] (un-indented, "\n"-
	// joined) that were committed to scrollback. It is the REFLOW guard: markdown
	// (glamour) can re-wrap an already-flushed prefix as more text streams in (a
	// numbered list joins into one reflowing paragraph), which would make the next
	// flush splice MISALIGNED rows. Before advancing we require the fresh render's
	// prefix to still equal this; if it diverged we hold (the seal commits the whole
	// remaining tail consistently). Greedy wrapCells prose never diverges, so the
	// common case always advances.
	flushedRowsText string
}

// NoteCell is a standalone operational note outside any turn.
type NoteCell struct {
	ID    string
	Level NoteLevel
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

// isSealed is the commit gate (§3): a turn seals when it leaves the active state;
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
		// A tool/note step between us and the last prose means prose has resumed:
		// the caller must open a NEW StepProse rather than merging.
		if t.Steps[i].Kind == StepTool || t.Steps[i].Kind == StepNote {
			return -1
		}
	}
	return -1
}

// appendProse appends streamed text, opening a new StepProse when prose resumes
// after a tool/note step so the chronological narrative is preserved (§5).
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

// findActivity returns the Activity pointer for a tool-call id, or nil.
func (t *TurnCell) findActivity(id string) *Activity {
	for i := range t.Steps {
		if t.Steps[i].Kind == StepTool && t.Steps[i].Activity != nil && t.Steps[i].Activity.ID == id {
			return t.Steps[i].Activity
		}
	}
	return nil
}
