package timer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

type memStore struct {
	inserted    []domain.TimerRecord
	revoked     []string
	revokeCount int
	revokeErr   error
	patched     []map[string]any
	grants      map[string][]domain.AutomationGrantRecord
}

func (m *memStore) InsertTimer(rec domain.TimerRecord) (string, error) {
	m.inserted = append(m.inserted, rec)
	return rec.ID, nil
}
func (m *memStore) ListTimers(string) ([]domain.TimerRecord, error) {
	return m.inserted, nil
}
func (m *memStore) GetTimer(id string) (*domain.TimerRecord, error) {
	for i := range m.inserted {
		if m.inserted[i].ID == id {
			return &m.inserted[i], nil
		}
	}
	return nil, nil
}
func (m *memStore) ClaimDueTimer(id string, expectFireAt int64, patch map[string]any) (bool, error) {
	for i := range m.inserted {
		if m.inserted[i].ID != id || m.inserted[i].Status != "scheduled" ||
			m.inserted[i].FireAt != expectFireAt {
			continue
		}
		m.patched = append(m.patched, patch)
		if st, ok := patch["status"].(string); ok {
			m.inserted[i].Status = st
		}
		return true, nil
	}
	return false, nil
}
func (m *memStore) ListGrants(actorID string, _ int64) ([]domain.AutomationGrantRecord, error) {
	return m.grants[actorID], nil
}

// revokeCount is deliberately NOT 1: a handler that hard-coded the count, or read it
// from the wrong place, would sail past an assertion of 1. revokeErr, when set, drives
// the cascade-failure branch.
func (m *memStore) RevokeGrantsByActor(id string, _ int64) (int, error) {
	m.revoked = append(m.revoked, id)
	if m.revokeErr != nil {
		return 0, m.revokeErr
	}
	return m.revokeCount, nil
}

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// An invalid fireAt is a non-recoverable TIMER_FIRE_AT (the model can't recover
// by retrying the same bad value).
func TestScheduleBadFireAt(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "timer.schedule")
	args := json.RawMessage(`{"title":"x","fireAt":"not-a-date","payload":{"type":"enqueue"}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeTimerFireAt || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable TIMER_FIRE_AT, got %+v", res)
	}
}

// delayMs computes fireAt from now and persists a scheduled timer.
func TestScheduleDelayMs(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	args := json.RawMessage(`{"title":"ping","delayMs":5000,"payload":{"type":"enqueue","message":"hi"}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.inserted) != 1 || st.inserted[0].Status != "scheduled" {
		t.Fatalf("timer not persisted scheduled: %+v", st.inserted)
	}
	if st.inserted[0].FireAt <= domain.NowMS() {
		t.Fatal("fireAt should be in the future")
	}
}

// call_safe_tool payload requires a toolName.
func TestScheduleCallSafeToolRequiresName(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "timer.schedule")
	args := json.RawMessage(`{"title":"x","delayMs":1000,"payload":{"type":"call_safe_tool"}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS, got %+v", res)
	}
}

// cancel of a known timer revokes its grants; an unknown id is non-recoverable.
func TestCancel(t *testing.T) {
	st := &memStore{inserted: []domain.TimerRecord{{ID: "tmr_1", Status: "scheduled"}}, revokeCount: 7}
	tool := find(Tools(Deps{Store: st}), "timer.cancel")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"tmr_1"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.revoked) != 1 || st.revoked[0] != "tmr_1" {
		t.Fatalf("expected grant revoke for cancelled timer, got %v", st.revoked)
	}
	// The cascade has to be OBSERVABLE, not just performed: without revokedGrants in
	// the result the model's only record of it is a sentence in the tool description,
	// and it revokes the grant again — which used to fail. 7 is the store's number, so
	// a hard-coded count cannot satisfy this.
	if got := res.Result.(map[string]any)["revokedGrants"]; got != 7 {
		t.Fatalf("expected revokedGrants=7 (the store's count) in the result, got %v", got)
	}
	if got := res.Result.(map[string]any)["grantRevokeFailed"]; got != false {
		t.Fatalf("a successful cascade must report grantRevokeFailed=false, got %v", got)
	}

	res = tool.Handle(context.Background(), json.RawMessage(`{"id":"nope"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeTimerNotFound || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable TIMER_NOT_FOUND, got %+v", res)
	}
}

// A cascade that found nothing still reports the field, and one that FAILED must not
// report a confident 0 — the description tells the model that 0 means "no follow-up
// grant.revoke needed", so a swallowed storage error would strand a live grant behind
// a tool result that says everything is clean.
func TestCancelReportsCascadeOutcomeHonestly(t *testing.T) {
	t.Run("no grants held", func(t *testing.T) {
		st := &memStore{inserted: []domain.TimerRecord{{ID: "tmr_1", Status: "scheduled"}}, revokeCount: 0}
		tool := find(Tools(Deps{Store: st}), "timer.cancel")
		res := tool.Handle(context.Background(), json.RawMessage(`{"id":"tmr_1"}`), &tools.ToolContext{})
		if !res.Ok {
			t.Fatalf("expected ok, got %+v", res.Error)
		}
		got, present := res.Result.(map[string]any)["revokedGrants"]
		if !present || got != 0 {
			t.Fatalf("revokedGrants must be present and 0, got %v (present=%v)", got, present)
		}
		if failed := res.Result.(map[string]any)["grantRevokeFailed"]; failed != false {
			t.Fatalf("want grantRevokeFailed=false, got %v", failed)
		}
	})

	t.Run("cascade failed", func(t *testing.T) {
		st := &memStore{
			inserted:  []domain.TimerRecord{{ID: "tmr_1", Status: "scheduled"}},
			revokeErr: errors.New("db locked"),
		}
		tool := find(Tools(Deps{Store: st}), "timer.cancel")
		res := tool.Handle(context.Background(), json.RawMessage(`{"id":"tmr_1"}`), &tools.ToolContext{})
		// Still Ok: the timer really is cancelled, and failing the call would be the
		// bigger lie. The failure rides the result instead.
		if !res.Ok {
			t.Fatalf("a failed cascade must not fail the cancel, got %+v", res.Error)
		}
		if failed := res.Result.(map[string]any)["grantRevokeFailed"]; failed != true {
			t.Fatalf("want grantRevokeFailed=true, got %v", failed)
		}
		if !strings.Contains(res.Summary, "grant.revoke") {
			t.Fatalf("the summary must point at the recovery, got %q", res.Summary)
		}
	})
}

// A payload the registry says can never run at fire time is refused AT SCHEDULE TIME,
// and nothing is stored.
//
// This is the bug the preflight seam exists for: a timer-dispatched spawn that named
// no worktree was accepted, reported back as "Scheduled.", and then failed on its only
// firing into a queue row nobody had open. Refusing here is what puts the error in
// front of the model while it can still fix the call.
func TestScheduleRefusesAnUnrunnablePayload(t *testing.T) {
	st := &memStore{}
	var sawTool string
	var sawArgs string
	tool := find(Tools(Deps{
		Store: st,
		PrepareScheduledCall: func(name string, args json.RawMessage) (string, string) {
			sawTool, sawArgs = name, string(args)
			return "", "it names no worktreeId"
		},
	}), "timer.schedule")

	args := json.RawMessage(`{"title":"spawn","delayMs":10000,"payload":` +
		`{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits","args":{"title":"go"}}}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})

	if res.Ok || res.Error.Code != codeTimerUnrunnable {
		t.Fatalf("expected %s, got %+v", codeTimerUnrunnable, res)
	}
	// Retrying the identical call cannot help — only a different call can.
	if res.Error.Recoverable {
		t.Fatal("an unrunnable payload is not recoverable by retrying it")
	}
	// The reason has to reach the model, not just a code: the code says "no" and the
	// reason is the only part that says what to write instead.
	if !strings.Contains(res.Error.Message, "names no worktreeId") {
		t.Fatalf("failure should carry the tool's own reason, got %q", res.Error.Message)
	}
	if len(st.inserted) != 0 {
		t.Fatalf("a refused schedule must persist nothing, got %+v", st.inserted)
	}
	// The tool is asked about ITS OWN arguments, not the timer's.
	if sawTool != "agentTask.spawnForEdits" || !strings.Contains(sawArgs, `"title":"go"`) {
		t.Fatalf("preflight got (%q, %q)", sawTool, sawArgs)
	}
}

// The preflight is consulted for the tool payload only — a reminder has no tool to ask.
func TestScheduleSkipsPreflightForAReminder(t *testing.T) {
	called := false
	st := &memStore{}
	tool := find(Tools(Deps{
		Store: st,
		PrepareScheduledCall: func(n string, _ json.RawMessage) (string, string) {
			called = true
			return n, "no"
		},
	}), "timer.schedule")
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":1000,"payload":{"type":"enqueue","message":"hi"}}`),
		&tools.ToolContext{})
	if !res.Ok || called {
		t.Fatalf("a reminder should schedule without a preflight; ok=%v called=%v", res.Ok, called)
	}
}

// No preflight wired ⇒ scheduling behaves exactly as it did before the seam existed.
func TestScheduleWithoutPreflightIsUnchanged(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"spawn","delayMs":1000,"payload":`+
			`{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits"}}}`),
		&tools.ToolContext{})
	if !res.Ok || len(st.inserted) != 1 {
		t.Fatalf("expected a stored timer, got ok=%v inserted=%+v", res.Ok, st.inserted)
	}
}

// The name that gets STORED is the one dispatch will look up, not the one that was
// typed. Fire-time dispatch resolves nothing, so persisting a wire spelling is how a
// payload that passed every check still dies with UNKNOWN_TOOL on its only firing.
func TestScheduleStoresTheCanonicalToolName(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{
		Store: st,
		PrepareScheduledCall: func(name string, _ json.RawMessage) (string, string) {
			if name == "agentTask__spawnForEdits" {
				return "agentTask.spawnForEdits", ""
			}
			return name, ""
		},
	}), "timer.schedule")

	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"spawn","delayMs":1000,"payload":`+
			`{"type":"call_safe_tool","toolCall":{"toolName":"agentTask__spawnForEdits","args":{"worktreeId":"/w"}}}}`),
		&tools.ToolContext{})
	if !res.Ok || len(st.inserted) != 1 {
		t.Fatalf("expected a stored timer, got ok=%v inserted=%+v", res.Ok, st.inserted)
	}
	if !strings.Contains(st.inserted[0].PayloadJson, `"toolName":"agentTask.spawnForEdits"`) {
		t.Fatalf("the stored payload should carry the resolved name, got %s", st.inserted[0].PayloadJson)
	}
}

// Absent args reach the check as `{}` — what dispatch will hand the handler — rather
// than as the `null` a nil map marshals to, which no decoder accepts.
func TestSchedulePassesAbsentArgsAsAnEmptyObject(t *testing.T) {
	var saw string
	tool := find(Tools(Deps{
		Store: &memStore{},
		PrepareScheduledCall: func(n string, args json.RawMessage) (string, string) {
			saw = string(args)
			return n, ""
		},
	}), "timer.schedule")

	tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":1000,"payload":`+
			`{"type":"call_safe_tool","toolCall":{"toolName":"fs.read"}}}`),
		&tools.ToolContext{})
	if saw != "{}" {
		t.Fatalf("absent args should arrive as {}, got %q", saw)
	}
}

// The recursion cut, in the configuration where it was previously inert.
//
// The guard used to key off the dispatch ACTOR, which is fixed when the App is built
// and immutable after: the attached host is ActorMain for its whole life, including
// while it runs a wake turn. So in the one configuration a user actually watches — the
// panel open — a scheduled message could schedule another, and the loop could sustain
// itself. The signal has to be per-TURN, and this pins that.
func TestScheduleRefusesAMessageFromAMessageStartedTurn(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	args := json.RawMessage(`{"title":"again","delayMs":1000,` +
		`"payload":{"type":"message","message":"do it once more"}}`)

	// ActorMain is the attached session's actor — the case the old check waved through.
	res := tool.Handle(context.Background(), args,
		&tools.ToolContext{Actor: domain.ActorMain, FromTimerMessage: true})

	if res.Ok || res.Error.Code != codeTimerMessageRecursion {
		t.Fatalf("expected %s, got %+v", codeTimerMessageRecursion, res)
	}
	if res.Error.Recoverable {
		t.Error("retrying the same call cannot help; the refusal is structural")
	}
	if len(st.inserted) != 0 {
		t.Fatalf("a refused recursion must store nothing, got %+v", st.inserted)
	}
}

// ...and an ordinary interactive turn still schedules one. A guard that also blocked
// the user would have replaced a loop with a dead feature.
func TestScheduleAllowsAMessageFromAnOrdinaryTurn(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"tests","delayMs":25000,`+
			`"payload":{"type":"message","message":"Send npm test to the build terminal"}}`),
		&tools.ToolContext{Actor: domain.ActorMain})

	if !res.Ok {
		t.Fatalf("an interactive turn must be able to schedule a message, got %+v", res.Error)
	}
	if len(st.inserted) != 1 || st.inserted[0].PayloadType != "message" {
		t.Fatalf("expected one stored message timer, got %+v", st.inserted)
	}
	if !strings.Contains(st.inserted[0].PayloadJson, "Send npm test to the build terminal") {
		t.Errorf("the stored payload must carry the message verbatim, got %s", st.inserted[0].PayloadJson)
	}
}

// A message with no text is refused: waking a turn to carry out nothing spends a model
// call to discover it has no instruction.
func TestScheduleRejectsABlankMessage(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	for _, bad := range []string{`{"type":"message"}`, `{"type":"message","message":"   "}`} {
		res := tool.Handle(context.Background(),
			json.RawMessage(`{"title":"x","delayMs":1000,"payload":`+bad+`}`),
			&tools.ToolContext{Actor: domain.ActorMain})
		if res.Ok {
			t.Errorf("a blank message should be refused: %s", bad)
		}
	}
	if len(st.inserted) != 0 {
		t.Fatalf("nothing should be stored, got %+v", st.inserted)
	}
}

// A timer may never schedule a timer.
//
// The recursion cut was originally scoped to `message` payloads, which left a longer
// way round open: a message turn schedules a repeating `call_safe_tool` whose target is
// `timer.schedule`, and every firing mints the next message. The outer call was not a
// message so it passed, and the inner one runs under the daemon's own context, which
// carries no turn flag at all. Blocking the tool by NAME closes that route at the far
// end, where the turn flag cannot reach.
func TestScheduleRefusesATimerThatSchedulesATimer(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	for name, spelling := range map[string]string{
		"dotted": "timer.schedule",
		"wire":   "timer__schedule",
		"padded": "  timer.schedule  ",
	} {
		res := tool.Handle(context.Background(), json.RawMessage(`{"title":"chain","delayMs":1000,`+
			`"payload":{"type":"call_safe_tool","toolCall":{"toolName":"`+spelling+`"}}}`),
			&tools.ToolContext{Actor: domain.ActorMain})
		if res.Ok || res.Error.Code != codeTimerMessageRecursion {
			t.Errorf("%s spelling should be refused, got %+v", name, res)
		}
	}
	if len(st.inserted) != 0 {
		t.Fatalf("nothing should be stored, got %+v", st.inserted)
	}
}

// A message turn may schedule NOTHING, not merely no message. Any payload type is a
// route back to a message, so the cut has to sit above the type switch.
func TestScheduleRefusesEveryPayloadFromAMessageStartedTurn(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	for name, payload := range map[string]string{
		"reminder":  `{"type":"enqueue","message":"ping"}`,
		"tool call": `{"type":"call_safe_tool","toolCall":{"toolName":"fs.read"}}`,
		"message":   `{"type":"message","message":"again"}`,
	} {
		res := tool.Handle(context.Background(),
			json.RawMessage(`{"title":"x","delayMs":1000,"payload":`+payload+`}`),
			&tools.ToolContext{Actor: domain.ActorMain, FromTimerMessage: true})
		if res.Ok || res.Error.Code != codeTimerMessageRecursion {
			t.Errorf("a message turn must not schedule a %s, got %+v", name, res)
		}
	}
	if len(st.inserted) != 0 {
		t.Fatalf("nothing should be stored, got %+v", st.inserted)
	}
}

// An interval large enough to overflow int64 is worse than no repeat: now+everyMs wraps
// negative, every due check reads the row as permanently overdue, and it fires on every
// scheduler tick for ever — defeating both the minimum spacing and the finite bound
// that were supposed to make a repeating message safe.
func TestScheduleRefusesAnOverflowingRepeat(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	res := tool.Handle(context.Background(), json.RawMessage(`{"title":"forever","delayMs":1000,`+
		`"payload":{"type":"message","message":"tick"},`+
		`"repeat":{"everyMs":9223372036854775807,"until":"9999-12-31T23:59:59Z"}}`),
		&tools.ToolContext{Actor: domain.ActorMain})
	if res.Ok {
		t.Fatal("an overflowing repeat interval must be refused")
	}
	if len(st.inserted) != 0 {
		t.Fatalf("nothing should be stored, got %+v", st.inserted)
	}
}

// A repeating message must be slow AND bounded. Either alone still permits a spend
// loop: a fast repeat burns its cap in seconds, a slow one without a cap never stops.
func TestScheduleRequiresRepeatingMessagesToBeSlowAndBounded(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	for name, repeat := range map[string]string{
		"too fast":   `{"everyMs":1000,"maxRuns":5}`,
		"unbounded":  `{"everyMs":3600000}`,
		"both wrong": `{"everyMs":1}`,
	} {
		res := tool.Handle(context.Background(), json.RawMessage(`{"title":"x","delayMs":1000,`+
			`"payload":{"type":"message","message":"tick"},"repeat":`+repeat+`}`),
			&tools.ToolContext{Actor: domain.ActorMain})
		if res.Ok {
			t.Errorf("%s repeat should be refused", name)
		}
	}
	// ...and a well-formed one is accepted, so the rule did not just kill repeats.
	res := tool.Handle(context.Background(), json.RawMessage(`{"title":"x","delayMs":1000,`+
		`"payload":{"type":"message","message":"tick"},"repeat":{"everyMs":3600000,"maxRuns":3}}`),
		&tools.ToolContext{Actor: domain.ActorMain})
	if !res.Ok {
		t.Fatalf("a slow, bounded repeating message must be allowed, got %+v", res.Error)
	}
	// An enqueue reminder is deliberately NOT subject to these rules: it costs nothing.
	res = tool.Handle(context.Background(), json.RawMessage(`{"title":"y","delayMs":1000,`+
		`"payload":{"type":"enqueue","message":"ping"},"repeat":{"everyMs":1000}}`),
		&tools.ToolContext{Actor: domain.ActorMain})
	if !res.Ok {
		t.Fatalf("a fast unbounded reminder is still legal, got %+v", res.Error)
	}
}

// A bound must actually bind.
//
// "maxRuns: 4000000000" and "until: 9999-12-31" both satisfy "is it bounded?" while
// describing billions of paid turns — which is the thing the rule was written to
// prevent. A limit nobody could ever reach is not a limit.
func TestScheduleRefusesABoundThatDoesNotBind(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	huge := 4_000_000_000
	for name, repeat := range map[string]string{
		"absurd maxRuns":    fmt.Sprintf(`{"everyMs":60000,"maxRuns":%d}`, huge),
		"absurd until":      `{"everyMs":60000,"until":"9999-12-31T23:59:59Z"}`,
		"overflowing every": `{"everyMs":9223372036854775807,"until":"9999-12-31T23:59:59Z"}`,
	} {
		res := tool.Handle(context.Background(), json.RawMessage(`{"title":"x","delayMs":1000,`+
			`"payload":{"type":"message","message":"tick"},"repeat":`+repeat+`}`),
			&tools.ToolContext{Actor: domain.ActorMain})
		if res.Ok {
			t.Errorf("%s should be refused", name)
		}
	}
	// A realistic schedule — nightly for a fortnight — is still fine.
	res := tool.Handle(context.Background(), json.RawMessage(`{"title":"x","delayMs":1000,`+
		`"payload":{"type":"message","message":"tick"},"repeat":{"everyMs":86400000,"maxRuns":14}}`),
		&tools.ToolContext{Actor: domain.ActorMain})
	if !res.Ok {
		t.Fatalf("a nightly fortnight must be allowed, got %+v", res.Error)
	}
	if len(st.inserted) != 1 {
		t.Fatalf("exactly the good one should be stored, got %+v", st.inserted)
	}
}

// A delay near MaxInt64 wraps now+delayMs negative, and a negative fireAt reads as
// permanently overdue — the timer fires at once and on every tick after.
func TestScheduleRefusesAnOverflowingDelay(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":9223372036854775807,`+
			`"payload":{"type":"enqueue","message":"ping"}}`),
		&tools.ToolContext{Actor: domain.ActorMain})
	if res.Ok {
		t.Fatal("an overflowing delayMs must be refused")
	}
	if len(st.inserted) != 0 {
		t.Fatalf("nothing should be stored, got %+v", st.inserted)
	}
}

// No autonomous turn may schedule a timer — not just a scheduled message.
//
// Lineage does not survive a hop. A timed message that starts an async wait sheds its
// own marker at the completion wake, and that turn was then free to schedule again: a
// cycle with one extra step in it. Every descendant of an autonomous turn is itself
// autonomous, so gating on THAT closes the class rather than chasing a tag through
// async completions and watcher digests.
func TestScheduleRefusesEveryAutonomousTurn(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	args := json.RawMessage(`{"title":"x","delayMs":60000,"payload":{"type":"message","message":"go"}}`)

	for name, tctx := range map[string]*tools.ToolContext{
		"scheduled message": {Actor: domain.ActorMain, FromTimerMessage: true, FromWake: true},
		"watcher wake":      {Actor: domain.ActorMain, FromWake: true},
		"async completion":  {Actor: domain.ActorWake, FromWake: true},
	} {
		res := tool.Handle(context.Background(), args, tctx)
		if res.Ok || res.Error.Code != codeTimerMessageRecursion {
			t.Errorf("a %s must not schedule a timer, got %+v", name, res)
		}
	}
	if len(st.inserted) != 0 {
		t.Fatalf("nothing should be stored, got %+v", st.inserted)
	}

	// The interactive user is untouched — they are the only one who ever asks.
	if res := tool.Handle(context.Background(), args, &tools.ToolContext{Actor: domain.ActorMain}); !res.Ok {
		t.Fatalf("an interactive turn must still schedule, got %+v", res.Error)
	}
}

// The paid-repeat limits cover call_safe_tool too, not messages alone.
//
// Scoping them to messages left the same spend loop one step away: a call_safe_tool
// repeating every millisecond can target a tool that calls the model itself, or register
// an async wait whose completion is a full paid wake.
func TestScheduleBoundsEveryPayloadThatCostsPerFire(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	for name, body := range map[string]string{
		"fast tool repeat":      `"payload":{"type":"call_safe_tool","toolCall":{"toolName":"terminal.extract"}},"repeat":{"everyMs":1,"maxRuns":5}`,
		"unbounded tool repeat": `"payload":{"type":"call_safe_tool","toolCall":{"toolName":"terminal.extract"}},"repeat":{"everyMs":3600000}`,
	} {
		res := tool.Handle(context.Background(),
			json.RawMessage(`{"title":"x","delayMs":1000,`+body+`}`),
			&tools.ToolContext{Actor: domain.ActorMain})
		if res.Ok {
			t.Errorf("%s should be refused", name)
		}
	}
	// A reminder costs nothing per fire and stays exempt.
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"y","delayMs":1000,"payload":{"type":"enqueue","message":"ping"},"repeat":{"everyMs":1000}}`),
		&tools.ToolContext{Actor: domain.ActorMain})
	if !res.Ok {
		t.Fatalf("a fast unbounded reminder is still legal, got %+v", res.Error)
	}
}
