package host

import "github.com/daintreehq/assistant/internal/redact"

// timers.go carries the durable-timer manager surface: the list a host draws and
// the one mutation it can make.
//
// It is separate from operations.go on purpose. The operations deck is a
// SEVEN-section reading of everything the assistant is doing, answered on demand
// because most of it is expensive and most of it is rarely looked at. A timer
// manager is a different thing: one small section, refreshed on its own cadence,
// with a control on it. Making a host re-pull agents, workflows, async futures and
// the audit tail every time someone opens a timer popover would be paying for six
// sections to show one.
//
// The two must not disagree, so both build their rows from internal/timers — the
// deck's `timers[]` array is the SAME TimerRow this file encodes.

// TimerRow is one scheduled timer.
//
// Fixed shape, per the row contract in DAINTREE_HOST.md: every field is always
// present, and a zero value means "the engine does not have this", never "an
// engine too old to say". So repeat/target/tool fields are zero-valued rather
// than omitted, and a host reads `repeatEveryMs == 0` as "does not repeat".
//
// What is NOT here is deliberate. The stored payload carries the reminder text
// the model wrote and the arbitrary argument object for a scheduled tool call;
// neither is needed to decide whether to cancel a timer, and both would cross
// into a renderer. The row names the TOOL and not its arguments — see the View
// doc comment in internal/timers.
type TimerRow struct {
	ID string
	// Label is the timer's title. Named for what a host does with it rather than
	// where it came from, and kept as the original field name so the operations
	// deck's existing binding still reads.
	Label string
	// DueAt is the next fire time (Unix ms).
	DueAt     int64
	CreatedAt int64
	// PayloadKind is "reminder" | "tool_call" | "legacy".
	PayloadKind string
	// ToolName is set only for "tool_call".
	ToolName string
	// RunCount is how many times a repeating timer has already fired.
	RunCount int
	// RepeatEveryMs is 0 for a one-shot timer.
	RepeatEveryMs int64
	// RepeatMaxRuns / RepeatUntilAt are 0 when the repeat is unbounded on that axis.
	RepeatMaxRuns int
	RepeatUntilAt int64
	// TargetWorktreeID / TargetTerminalID label which object the fire is about,
	// empty when the timer named none.
	TargetWorktreeID string
	TargetTerminalID string
	// LiveGrants is how many automation grants are still spendable by this timer.
	// It is what lets a host's cancel confirmation state its real consequence
	// ("this also revokes its 1 automation grant") instead of asking twice.
	// Meaningful only when GrantsUnknown is false.
	LiveGrants int
	// GrantsUnknown reports that the count could not be read. A host must say so
	// rather than render the 0 — on a destructive confirmation, "no grants" and
	// "we could not check" are different sentences and only one of them is true.
	GrantsUnknown bool
}

// encodeTimerRow renders one row's wire object. Shared by both snapshots so the
// deck and the manager cannot drift on field names or redaction.
func encodeTimerRow(r TimerRow) map[string]any {
	return map[string]any{
		"id": r.ID,
		// The title is free text the model wrote from what the user said, so it
		// gets the same scrub as every other free-text field on this protocol.
		"label":            redact.String(r.Label),
		"dueAt":            r.DueAt,
		"createdAt":        r.CreatedAt,
		"payloadKind":      r.PayloadKind,
		"toolName":         r.ToolName,
		"runCount":         r.RunCount,
		"repeatEveryMs":    r.RepeatEveryMs,
		"repeatMaxRuns":    r.RepeatMaxRuns,
		"repeatUntilAt":    r.RepeatUntilAt,
		"targetWorktreeId": r.TargetWorktreeID,
		"targetTerminalId": r.TargetTerminalID,
		"liveGrants":       r.LiveGrants,
		"grantsUnknown":    r.GrantsUnknown,
	}
}

// TimerOutcomeRow is one thing a timer DID — the record left behind when it fired.
//
// It is a different dimension from the schedule row, and the two must not be folded
// together: `status = fired` is not success. The scheduler claims a timer and advances
// it BEFORE running the payload, so a row can be fired and its tool have failed, been
// blocked for want of authority, or never run at all. This is the half that says which.
//
// Sourced from the attention queue, joined on the timer id the scheduler stamps onto
// every event a fire publishes.
type TimerOutcomeRow struct {
	EventID string
	TimerID string
	// Severity is the queue's own grading: "info" for a success (below the deck's
	// attention threshold, which is exactly why the deck could not show these),
	// "error" for a failure, "attention" for a reminder waiting to be read.
	Severity  string
	Title     string
	Summary   string
	CreatedAt int64
	UpdatedAt int64
	// Count is how many firings this row stands for. A repeating timer publishes under
	// one stable dedupe key, so the twelfth failure updates the first row rather than
	// adding a twelfth — and without the count a surface would report one.
	Count int
}

// encodeTimerOutcome renders one outcome's wire object.
func encodeTimerOutcome(r TimerOutcomeRow) map[string]any {
	return map[string]any{
		"eventId":  r.EventID,
		"timerId":  r.TimerID,
		"severity": r.Severity,
		// Both are free text the model wrote, and the summary can carry a tool's own
		// error output, which is the likeliest place on this path for a credential.
		"title":     redact.String(r.Title),
		"summary":   redact.String(r.Summary),
		"createdAt": r.CreatedAt,
		"updatedAt": r.UpdatedAt,
		"count":     r.Count,
	}
}

// EvTimers — timers:snapshot. Answers an inbound `timers`.
//
// Pull, like the operations deck, and for the same reason: a countdown ticks
// perfectly well in the host from `dueAt`, so streaming would be spending frames
// to retransmit a number the host can compute. What a host cannot compute is a
// TRANSITION — a timer appearing, firing or being retired — and that is what the
// invalidation event (Phase 2) is for.
type EvTimers struct {
	Timers []TimerRow
	// TakenAt is when the engine read the store, so a host can say how stale its
	// list is rather than implying it is live.
	TakenAt int64
	// Outcomes are what recently-fired timers actually DID, newest first.
	//
	// They ride the same snapshot as the schedule rows because they answer one
	// question together — "what is my assistant doing on a clock, and did the last
	// one work" — and because a fired timer leaves the Timers list entirely, so a
	// surface with only that list can never report an outcome at all. That was the
	// original hole: a timer fired, failed, and the panel showed nothing.
	Outcomes []TimerOutcomeRow
	// ReadFailed reports that the timer table could not be read, so the empty list
	// above means NOTHING.
	//
	// The operations deck is best-effort by design — a section that fails to load is
	// better than a deck that will not open. A timer manager cannot inherit that:
	// "no timers scheduled" is a claim a user acts on by walking away, and making it
	// out of a failed read is the worst thing this surface could say. The deck's
	// timers[] keeps the old semantics; this field is why the manager has its own
	// event rather than reusing that one.
	ReadFailed bool
}

func (e EvTimers) encode(sid string, seq uint64) ([]byte, error) {
	rows := make([]map[string]any, 0, len(e.Timers))
	for _, r := range e.Timers {
		rows = append(rows, encodeTimerRow(r))
	}
	outcomes := make([]map[string]any, 0, len(e.Outcomes))
	for _, o := range e.Outcomes {
		outcomes = append(outcomes, encodeTimerOutcome(o))
	}
	return marshalEvent("timers:snapshot", sid, seq, map[string]any{
		"timers":     rows,
		"outcomes":   outcomes,
		"takenAt":    e.TakenAt,
		"readFailed": e.ReadFailed,
	})
}

// EvTimerFired — timer:fired. An INVALIDATION, not a payload.
//
// It carries the id and nothing else, deliberately. A host reacts by re-reading
// `timers`, which is one round trip and cannot drift from the snapshot; pushing the
// outcome inline would be a second encoding of the same facts that has to be kept in
// step with the first, for a view that is usually not even open.
//
// This is the event the feature was missing. A timer's own fire never wakes the
// assistant (agent.IsActionableWake is false for SourceTimer, by design — a reminder
// is for a human, not a prompt), and a successful tool call publishes at `info`, below
// the attention threshold. So nothing at all reached the host: a timer fired, and the
// panel showed exactly what it showed a second earlier.
type EvTimerFired struct {
	TimerID string
	// FiredAt is when the scheduler ran it, so a host can order a burst of these
	// without inventing a receipt time.
	FiredAt int64
}

func (e EvTimerFired) encode(sid string, seq uint64) ([]byte, error) {
	return marshalEvent("timer:fired", sid, seq, map[string]any{
		"timerId": e.TimerID,
		"firedAt": e.FiredAt,
	})
}

// TimerCancelOutcome is the result of one `timer:cancel`.
type TimerCancelOutcome struct {
	TimerID string
	// Cancelled is true only when THIS call retired a live timer. A timer that had
	// already fired reports false with AlreadyInactive true, because a host that
	// showed "cancelled" for a timer that had already done its work would be
	// describing something that never happened.
	Cancelled bool
	// AlreadyInactive means the row was fired/cancelled/done before this call.
	AlreadyInactive bool
	// PriorStatus is what the row held when the engine read it.
	PriorStatus string
	// RevokedGrants is how many live grants the cascade withdrew.
	RevokedGrants int
	// GrantRevokeFailed reports that the timer is retired but its authority is NOT.
	// A host must surface this: a silent 0 reads as "nothing left to clean up"
	// while a grant is still spendable.
	GrantRevokeFailed bool
	// Error is set when the cancel itself failed (unknown id, storage fault). The
	// host renders it; the engine does not raise a separate host:error, so a UI can
	// settle the row's pending state from exactly one event either way.
	Error string
}

// EvTimerCancelled — timer:cancelled. Answers an inbound `timer:cancel`.
//
// Always emitted, success or failure, and always carrying the timerId. That is
// the correlation: a host has at most one cancel in flight per timer, so the id
// settles the right row without a request-id round trip.
type EvTimerCancelled struct {
	Outcome TimerCancelOutcome
}

func (e EvTimerCancelled) encode(sid string, seq uint64) ([]byte, error) {
	o := e.Outcome
	return marshalEvent("timer:cancelled", sid, seq, map[string]any{
		"timerId":           o.TimerID,
		"cancelled":         o.Cancelled,
		"alreadyInactive":   o.AlreadyInactive,
		"priorStatus":       o.PriorStatus,
		"revokedGrants":     o.RevokedGrants,
		"grantRevokeFailed": o.GrantRevokeFailed,
		"error":             o.Error,
	})
}
