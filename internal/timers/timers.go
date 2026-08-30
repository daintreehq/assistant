// Package timers holds the timer operations shared by every surface that can
// read or retire a durable timer: the model's timer.list / timer.cancel tools,
// the embedded host's timers snapshot and timer:cancel command, and (later) the
// supervisor's control socket.
//
// It exists so those surfaces cannot drift. Cancelling a timer is not one write:
// it retires the schedule row AND revokes any automation grant minted against
// that timer as its actor, and the second half is the half that matters — a live
// grant outliving the timer it was scoped to is standing unattended authority
// nobody can see. When only the model's tool knew that, a host that cancelled a
// timer by any other route would have left the grant behind.
//
// The package is deliberately I/O-only over a narrow Store seam and holds no
// policy about WHO may cancel. Authorization is the caller's: the model's tool
// runs through the normal dispatch gates, and a host surface confirms with the
// human in front of it before calling in.
package timers

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
)

// StatusScheduled is the only status a timer can fire from. Everything else —
// fired, cancelled, done — is terminal, and a terminal timer is not cancellable
// because there is nothing left to stop.
const StatusScheduled = "scheduled"

// Store is the slice of storage these operations touch. *storage.Store satisfies
// it directly; tests use a fake.
type Store interface {
	GetTimer(id string) (*domain.TimerRecord, error)
	ListTimers(status string) ([]domain.TimerRecord, error)
	// ClaimDueTimer applies the patch ONLY while the row is still status 'scheduled'
	// with the fireAt the caller read, and reports whether it won. It is the same
	// primitive the scheduler claims a due timer with, which is exactly why cancel
	// uses it too: the two are competing for one row.
	ClaimDueTimer(id string, expectFireAt int64, patch map[string]any) (bool, error)
	RevokeGrantsByActor(actorID string, now int64) (int, error)
	ListGrants(actorID string, now int64) ([]domain.AutomationGrantRecord, error)
}

// ErrNotFound is returned by Cancel when no timer carries the id.
var ErrNotFound = errors.New("timers: no such timer")

// PayloadKind is the closed vocabulary a surface renders a timer's action from.
// It is derived from the stored payload rather than passed through raw, because
// the payload itself carries free text and tool arguments that a host has no
// business displaying (see View).
type PayloadKind string

const (
	// KindReminder posts an inbox item and runs nothing.
	KindReminder PayloadKind = "reminder"
	// KindToolCall dispatches one registered tool at fire time.
	KindToolCall PayloadKind = "tool_call"
	// KindMessage is a timer that delivers an INSTRUCTION to the assistant at fire
	// time, which it then carries out in an ordinary turn. Distinct from KindReminder
	// because the difference is the whole point: one of them does the work.
	KindMessage PayloadKind = "message"
	// KindLegacy is a row written by a retired payload type (run_check). It still
	// fires, as a plain reminder, so it is still worth showing — but it cannot be
	// described in terms of either shape above without lying about one of them.
	KindLegacy PayloadKind = "legacy"
)

// Repeat is a timer's repeat policy, already resolved to what a reader needs.
type Repeat struct {
	EveryMs int64  `json:"everyMs"`
	MaxRuns *int   `json:"maxRuns,omitempty"`
	UntilAt *int64 `json:"untilAt,omitempty"`
}

// View is one active timer, reduced to what is SAFE to show outside the engine.
//
// What is missing is the point. The stored payload holds `message` (a reminder
// the model wrote, quoting whatever the user said) and `toolCall.args` (an
// arbitrary object — prompts, paths, and whatever else the model put there).
// Neither is needed to decide whether to cancel a timer, and both would cross a
// process boundary and land in a renderer, a log, or a screenshot. So a View
// carries the tool's NAME and not its arguments, and the timer's title (which
// the redactor still scrubs at the wire edge) and not its message body.
type View struct {
	ID          string              `json:"id"`
	Title       string              `json:"title"`
	NextFireAt  int64               `json:"nextFireAt"`
	CreatedAt   int64               `json:"createdAt"`
	PayloadKind PayloadKind         `json:"payloadKind"`
	ToolName    string              `json:"toolName,omitempty"`
	RunCount    int                 `json:"runCount"`
	Repeat      *Repeat             `json:"repeat,omitempty"`
	Target      *domain.EventTarget `json:"target,omitempty"`
	// LiveGrants is how many unrevoked, unexpired, unspent automation grants name
	// this timer as their actor. It is what makes the cancel confirmation able to
	// say what else is being withdrawn, which is the difference between a D1
	// confirmation that states its consequence and one that just asks twice.
	//
	// Only meaningful when GrantsUnknown is false.
	LiveGrants int `json:"liveGrants"`
	// GrantsUnknown reports that the grant count could NOT be read.
	//
	// It exists because LiveGrants alone cannot tell "this timer holds no authority"
	// apart from "we could not find out", and the difference is load-bearing: the
	// number is quoted in a destructive confirmation. Collapsing a failed read to 0
	// would tell the user there is nothing to revoke at the exact moment they are
	// deciding whether to revoke it — a silent fallback default on a destructive
	// path, which this project treats as a defect rather than a rounding error.
	GrantsUnknown bool `json:"grantsUnknown"`
}

// storedPayload is the on-disk payload shape, read only for the fields a View needs.
type storedPayload struct {
	Type     string `json:"type"`
	ToolCall *struct {
		ToolName string `json:"toolName"`
	} `json:"toolCall,omitempty"`
}

// describePayload resolves the display kind + tool name for one row. A payload
// that will not parse is reported as legacy rather than dropped: the row is still
// a real scheduled timer the user may want to cancel, and refusing to list what
// we cannot fully describe would hide exactly the timer most worth retiring.
func describePayload(rec domain.TimerRecord) (PayloadKind, string) {
	var p storedPayload
	_ = json.Unmarshal([]byte(rec.PayloadJson), &p)
	typ := p.Type
	if typ == "" {
		typ = rec.PayloadType
	}
	switch typ {
	case "enqueue":
		return KindReminder, ""
	case "message":
		return KindMessage, ""
	case "call_safe_tool":
		name := ""
		if p.ToolCall != nil {
			name = strings.TrimSpace(p.ToolCall.ToolName)
		}
		return KindToolCall, name
	default:
		return KindLegacy, ""
	}
}

// decodeTarget parses a row's stored target, or nil when it has none / it is
// unreadable. A corrupt target must not cost the caller the whole row.
func decodeTarget(rec domain.TimerRecord) *domain.EventTarget {
	if rec.TargetJson == nil || strings.TrimSpace(*rec.TargetJson) == "" {
		return nil
	}
	var t domain.EventTarget
	if err := json.Unmarshal([]byte(*rec.TargetJson), &t); err != nil {
		return nil
	}
	if t == (domain.EventTarget{}) {
		return nil
	}
	return &t
}

// ToView reduces one record to its safe view, resolving the live grant count
// through the store.
//
// A grant-count read that fails does not fail the row — losing the whole timer
// list because the grants table hiccuped is the worse outcome — but it does NOT
// quietly become 0 either. It sets GrantsUnknown, so the surface that quotes the
// number can say it does not know rather than say there is nothing there.
func ToView(s Store, rec domain.TimerRecord, now int64) View {
	kind, tool := describePayload(rec)
	v := View{
		ID:          rec.ID,
		Title:       rec.Title,
		NextFireAt:  rec.FireAt,
		CreatedAt:   rec.CreatedAt,
		PayloadKind: kind,
		ToolName:    tool,
		RunCount:    rec.RunCount,
		Target:      decodeTarget(rec),
	}
	if rec.RepeatEveryMs != nil && *rec.RepeatEveryMs > 0 {
		v.Repeat = &Repeat{EveryMs: *rec.RepeatEveryMs, MaxRuns: rec.MaxRuns, UntilAt: rec.RepeatUntil}
	}
	if s != nil {
		grants, err := s.ListGrants(rec.ID, now)
		if err != nil {
			v.GrantsUnknown = true
		} else {
			v.LiveGrants = len(grants)
		}
	}
	return v
}

// List returns every SCHEDULED timer as a safe view, soonest first (ListTimers
// already orders by fireAt).
func List(s Store, now int64) ([]View, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.ListTimers(StatusScheduled)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for i := range rows {
		out = append(out, ToView(s, rows[i], now))
	}
	return out, nil
}

// CancelOutcome reports what a cancel actually did.
type CancelOutcome struct {
	TimerID string
	// PriorStatus is the status the row held when we read it.
	PriorStatus string
	// AlreadyInactive is true when the timer had already fired, finished or been
	// cancelled, so this call retired nothing. The schedule row is left ALONE in
	// that case: stamping 'cancelled' over 'fired' would erase the record that it
	// ran, and a surface that then reported "cancelled" would be describing a
	// timer that had already done its work.
	AlreadyInactive bool
	// RevokedGrants counts the live grants the cascade withdrew.
	RevokedGrants int
	// GrantRevokeErr is non-nil when the schedule row was retired but its grants
	// could not be. Reported rather than returned as the call's error, because the
	// timer really is cancelled and failing the whole call would be the bigger lie
	// — but a silent 0 would read as "nothing to clean up" while authority is
	// still live, so the caller MUST surface this.
	GrantRevokeErr error
	// Contended means the scheduler fired this timer out from under the cancel and
	// rescheduled it, so nothing was retired and the timer is still live.
	//
	// It is a distinct outcome from every other one here because it is the only one
	// where the honest answer is "try again": the row still exists, still has a
	// future, and the user's intent was not carried out. Reporting it as cancelled
	// would be the worst available lie — the fire already ran.
	Contended bool
}

// cancelAttempts bounds the retry when the scheduler wins the row.
//
// Two, not one: the overwhelmingly common contended case is a repeating timer whose
// fire completed and rescheduled between our read and our write, and a second pass
// then claims the fresh row cleanly. It is bounded rather than a loop because a
// sub-tick repeat could otherwise keep a user's click spinning indefinitely, and
// "it fired while you were cancelling, try again" is a better answer than a hang.
const cancelAttempts = 2

// Cancel retires a timer and revokes any automation grant held against it.
//
// Grants are revoked even when the timer was already inactive. A terminal fire
// defers its own revoke until after the payload dispatches, and a process that
// died in that window leaves the grant live with no timer left to spend it —
// so the sweep is the one part worth doing unconditionally.
func Cancel(s Store, id string, now int64) (CancelOutcome, error) {
	out := CancelOutcome{TimerID: id}
	if s == nil {
		return out, ErrNotFound
	}
	for attempt := 0; attempt < cancelAttempts; attempt++ {
		rec, err := s.GetTimer(id)
		if err != nil {
			return out, err
		}
		if rec == nil {
			return out, ErrNotFound
		}
		out.PriorStatus = rec.Status

		// Already terminal: nothing to retire, and nothing is racing us for the row —
		// so the grant sweep is both safe and the only part still worth doing.
		if rec.Status != StatusScheduled {
			out.AlreadyInactive = true
			out.RevokedGrants, out.GrantRevokeErr = s.RevokeGrantsByActor(id, now)
			return out, nil
		}

		// CONDITIONAL, not a plain update. A read-then-write here loses to the
		// scheduler: fireTimer claims the row, dispatches the payload, and defers its
		// grant revoke past the dispatch — so an unconditional write landing in that
		// window would stamp "cancelled" over a timer that had just RUN, and the sweep
		// below would pull the grant out from under a dispatch still using it. The
		// claim makes the two writers compete for the same row on the same terms.
		claimed, err := s.ClaimDueTimer(id, rec.FireAt, map[string]any{"status": "cancelled"})
		if err != nil {
			return out, err
		}
		if claimed {
			out.RevokedGrants, out.GrantRevokeErr = s.RevokeGrantsByActor(id, now)
			return out, nil
		}
		// Lost the row. Deliberately NO grant sweep on this path: whoever won it owns
		// the grant lifecycle now — a fire that is mid-dispatch still needs the grant
		// it was minted for, and a competing cancel has already swept.
	}

	// Still contended after a retry. Report what the row actually says rather than
	// guessing: a one-shot that fired is inactive, a repeat that fired is live again.
	rec, err := s.GetTimer(id)
	if err != nil {
		return out, err
	}
	if rec == nil {
		return out, ErrNotFound
	}
	out.PriorStatus = rec.Status
	if rec.Status != StatusScheduled {
		out.AlreadyInactive = true
		out.RevokedGrants, out.GrantRevokeErr = s.RevokeGrantsByActor(id, now)
		return out, nil
	}
	out.Contended = true
	return out, nil
}
