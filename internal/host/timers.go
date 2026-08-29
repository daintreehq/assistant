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
	return marshalEvent("timers:snapshot", sid, seq, map[string]any{
		"timers":     rows,
		"takenAt":    e.TakenAt,
		"readFailed": e.ReadFailed,
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
