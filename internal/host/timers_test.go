package host

import (
	"context"
	"strings"
	"testing"
)

const timersDesc = `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`

func timersFactory(app *fakeApp) func(context.Context, AppParams) (App, error) {
	return func(context.Context, AppParams) (App, error) { return app, nil }
}

// find the first frame of a type, or nil.
func firstOfType(lines []map[string]any, typ string) map[string]any {
	for _, m := range lines {
		if m["type"] == typ {
			return m
		}
	}
	return nil
}

// A `timers` command is answered with exactly one timers:snapshot carrying the
// engine's rows.
func TestTimersCommandAnswersWithSnapshot(t *testing.T) {
	app := &fakeApp{timerRows: []TimerRow{{
		ID: "tmr_1", Label: "Nightly suite", DueAt: 1700000000000, CreatedAt: 1699999000000,
		PayloadKind: "tool_call", ToolName: "terminal.sendCommand", RunCount: 2,
		RepeatEveryMs: 3600000, RepeatMaxRuns: 12, TargetWorktreeID: "/p/app", LiveGrants: 1,
	}}}
	lines := driveHost(t, timersFactory(app), []string{
		timersDesc,
		`{"type":"timers","sessionId":"s"}`,
		`{"type":"shutdown","sessionId":"s"}`,
	})

	snap := firstOfType(lines, "timers:snapshot")
	if snap == nil {
		t.Fatalf("no timers:snapshot: %+v", lines)
	}
	rows, _ := snap["timers"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %+v", snap["timers"])
	}
	row, _ := rows[0].(map[string]any)
	// Spot-check the fields a manager cannot render without, across all three
	// groups the row carries: identity, the repeat policy, and the grant count
	// the cancel confirmation quotes.
	for key, want := range map[string]any{
		"id":               "tmr_1",
		"label":            "Nightly suite",
		"payloadKind":      "tool_call",
		"toolName":         "terminal.sendCommand",
		"targetWorktreeId": "/p/app",
	} {
		if row[key] != want {
			t.Errorf("row[%q] = %v, want %v", key, row[key], want)
		}
	}
	for key, want := range map[string]float64{
		"dueAt": 1700000000000, "runCount": 2, "repeatEveryMs": 3600000,
		"repeatMaxRuns": 12, "liveGrants": 1,
	} {
		if got, _ := row[key].(float64); got != want {
			t.Errorf("row[%q] = %v, want %v", key, row[key], want)
		}
	}
	// takenAt is what lets a host say how stale its list is instead of implying
	// the countdown it draws is live.
	if at, _ := snap["takenAt"].(float64); at <= 0 {
		t.Errorf("takenAt must be a real timestamp, got %v", snap["takenAt"])
	}
}

// A store that could not be read is NOT an empty list. The deck can afford to drop a
// section it failed to load; a manager cannot tell a user nothing is scheduled on the
// strength of a failed read — they act on that by walking away.
func TestTimersSnapshotDistinguishesAFailedReadFromAnEmptyList(t *testing.T) {
	empty := &fakeApp{}
	lines := driveHost(t, timersFactory(empty), []string{
		timersDesc, `{"type":"timers","sessionId":"s"}`, `{"type":"shutdown","sessionId":"s"}`,
	})
	if got := firstOfType(lines, "timers:snapshot")["readFailed"]; got != false {
		t.Errorf("an empty store must report readFailed=false, got %v", got)
	}

	broken := &fakeApp{timersFailed: true}
	lines = driveHost(t, timersFactory(broken), []string{
		timersDesc, `{"type":"timers","sessionId":"s"}`, `{"type":"shutdown","sessionId":"s"}`,
	})
	snap := firstOfType(lines, "timers:snapshot")
	if snap["readFailed"] != true {
		t.Errorf("a failed read must say so, got %+v", snap)
	}
	// It still answers — a host waiting on the reply must not hang on the failure.
	if _, ok := snap["timers"].([]any); !ok {
		t.Errorf("a failed read still carries an array, got %v", snap["timers"])
	}
}

// Fixed shape: a one-shot timer with no target still carries every field, so a
// host reads a zero as "the engine does not have this" rather than having to
// tell an absent key from an empty one.
func TestTimerRowIsAFixedShape(t *testing.T) {
	app := &fakeApp{timerRows: []TimerRow{{ID: "tmr_1", Label: "Ping", DueAt: 1, PayloadKind: "reminder"}}}
	lines := driveHost(t, timersFactory(app), []string{
		timersDesc,
		`{"type":"timers","sessionId":"s"}`,
		`{"type":"shutdown","sessionId":"s"}`,
	})
	rows, _ := firstOfType(lines, "timers:snapshot")["timers"].([]any)
	row, _ := rows[0].(map[string]any)
	for _, key := range []string{
		"id", "label", "dueAt", "createdAt", "payloadKind", "toolName", "runCount",
		"repeatEveryMs", "repeatMaxRuns", "repeatUntilAt", "targetWorktreeId",
		"targetTerminalId", "liveGrants", "grantsUnknown",
	} {
		if _, present := row[key]; !present {
			t.Errorf("row is missing %q — the shape must be fixed", key)
		}
	}
}

// The deck and the manager must describe a timer identically. Both encode the
// same TimerRow through the same encoder, and this is the test that fails if
// someone gives one surface a field the other does not have.
func TestDeckAndManagerAgreeOnTheRowShape(t *testing.T) {
	row := TimerRow{ID: "tmr_1", Label: "Ping", DueAt: 5, PayloadKind: "reminder", LiveGrants: 3}
	app := &fakeApp{timerRows: []TimerRow{row}}
	app.operations = OperationsSnapshot{Timers: []TimerRow{row}}
	lines := driveHost(t, timersFactory(app), []string{
		timersDesc,
		`{"type":"timers","sessionId":"s"}`,
		`{"type":"operations","sessionId":"s"}`,
		`{"type":"shutdown","sessionId":"s"}`,
	})
	fromManager, _ := firstOfType(lines, "timers:snapshot")["timers"].([]any)
	fromDeck, _ := firstOfType(lines, "operations:snapshot")["timers"].([]any)
	if len(fromManager) != 1 || len(fromDeck) != 1 {
		t.Fatalf("want one row on each surface, got %d/%d", len(fromManager), len(fromDeck))
	}
	m, _ := fromManager[0].(map[string]any)
	d, _ := fromDeck[0].(map[string]any)
	if len(m) != len(d) {
		t.Fatalf("surfaces carry different field sets: manager=%v deck=%v", m, d)
	}
	for k, v := range m {
		if d[k] != v {
			t.Errorf("field %q differs: manager=%v deck=%v", k, v, d[k])
		}
	}
}

// A cancel reaches the app with the id the host sent, and its outcome comes back
// as one timer:cancelled.
func TestTimerCancelIsAnsweredAndReachesTheApp(t *testing.T) {
	app := &fakeApp{cancelOutcome: TimerCancelOutcome{
		Cancelled: true, PriorStatus: "scheduled", RevokedGrants: 2,
	}}
	lines := driveHost(t, timersFactory(app), []string{
		timersDesc,
		`{"type":"timer:cancel","sessionId":"s","timerId":"tmr_9"}`,
		`{"type":"shutdown","sessionId":"s"}`,
	})
	if got := app.cancelledIDs(); len(got) != 1 || got[0] != "tmr_9" {
		t.Fatalf("app saw %v, want one cancel of tmr_9", got)
	}
	ev := firstOfType(lines, "timer:cancelled")
	if ev == nil {
		t.Fatalf("no timer:cancelled: %+v", lines)
	}
	if ev["timerId"] != "tmr_9" {
		t.Errorf("timerId = %v — the id IS the correlation, a host settles its row on it", ev["timerId"])
	}
	if ev["cancelled"] != true || ev["alreadyInactive"] != false {
		t.Errorf("want cancelled/not-inactive, got %+v", ev)
	}
	if got, _ := ev["revokedGrants"].(float64); got != 2 {
		t.Errorf("revokedGrants = %v, want 2", ev["revokedGrants"])
	}
	if ev["grantRevokeFailed"] != false {
		t.Errorf("grantRevokeFailed = %v, want false", ev["grantRevokeFailed"])
	}
}

// A cancel that could not do anything still answers. The host has a row in a
// pending state, and every path — unknown id, storage fault, already fired — has
// to be able to settle it, or the UI spins forever on a call that is over.
func TestTimerCancelAnswersEvenWhenItFails(t *testing.T) {
	t.Run("unknown id", func(t *testing.T) {
		app := &fakeApp{cancelOutcome: TimerCancelOutcome{Error: "No timer with id tmr_gone"}}
		lines := driveHost(t, timersFactory(app), []string{
			timersDesc,
			`{"type":"timer:cancel","sessionId":"s","timerId":"tmr_gone"}`,
			`{"type":"shutdown","sessionId":"s"}`,
		})
		ev := firstOfType(lines, "timer:cancelled")
		if ev == nil {
			t.Fatalf("a failed cancel must still answer: %+v", lines)
		}
		if ev["cancelled"] != false {
			t.Errorf("a failed cancel must not report cancelled=true, got %+v", ev)
		}
		if msg, _ := ev["error"].(string); !strings.Contains(msg, "tmr_gone") {
			t.Errorf("error must name the timer, got %q", msg)
		}
	})

	t.Run("already fired", func(t *testing.T) {
		app := &fakeApp{cancelOutcome: TimerCancelOutcome{
			AlreadyInactive: true, PriorStatus: "fired",
		}}
		lines := driveHost(t, timersFactory(app), []string{
			timersDesc,
			`{"type":"timer:cancel","sessionId":"s","timerId":"tmr_1"}`,
			`{"type":"shutdown","sessionId":"s"}`,
		})
		ev := firstOfType(lines, "timer:cancelled")
		// Reporting cancelled:true here would have the host tell the user it
		// stopped something that had already run.
		if ev["cancelled"] != false || ev["alreadyInactive"] != true {
			t.Errorf("an already-fired timer must report alreadyInactive, got %+v", ev)
		}
		if ev["priorStatus"] != "fired" {
			t.Errorf("priorStatus = %v, want fired", ev["priorStatus"])
		}
	})

	t.Run("grant cascade failed", func(t *testing.T) {
		app := &fakeApp{cancelOutcome: TimerCancelOutcome{
			Cancelled: true, PriorStatus: "scheduled", GrantRevokeFailed: true,
		}}
		lines := driveHost(t, timersFactory(app), []string{
			timersDesc,
			`{"type":"timer:cancel","sessionId":"s","timerId":"tmr_1"}`,
			`{"type":"shutdown","sessionId":"s"}`,
		})
		ev := firstOfType(lines, "timer:cancelled")
		// The timer really is retired, so the call is not a failure — but a
		// silent revokedGrants:0 would read as "nothing left to clean up" while
		// authority is still spendable.
		if ev["cancelled"] != true || ev["grantRevokeFailed"] != true {
			t.Errorf("a retired timer with live authority must say so, got %+v", ev)
		}
	})
}

// A cancel with no timerId is not a command. It is dropped like any other
// unparseable line — never answered, and never turned into an error frame.
func TestTimerCancelWithoutAnIdIsDropped(t *testing.T) {
	app := &fakeApp{}
	for _, line := range []string{
		`{"type":"timer:cancel","sessionId":"s"}`,
		`{"type":"timer:cancel","sessionId":"s","timerId":""}`,
		`{"type":"timer:cancel","sessionId":"s","timerId":"   "}`,
		`{"type":"timer:cancel","sessionId":"s","timerId":7}`,
	} {
		lines := driveHost(t, timersFactory(app), []string{
			timersDesc, line, `{"type":"shutdown","sessionId":"s"}`,
		})
		if ev := firstOfType(lines, "timer:cancelled"); ev != nil {
			t.Errorf("%s was answered: %+v", line, ev)
		}
		if ev := firstOfType(lines, "host:error"); ev != nil {
			t.Errorf("%s produced an error frame: %+v", line, ev)
		}
	}
	if got := app.cancelledIDs(); len(got) != 0 {
		t.Fatalf("an unparseable cancel must never reach the app, got %v", got)
	}
}

// The timer commands are session-scoped like every other command: one addressed
// to a different session is dropped, not serviced.
func TestTimerCommandsIgnoreAForeignSession(t *testing.T) {
	app := &fakeApp{timerRows: []TimerRow{{ID: "tmr_1"}}}
	lines := driveHost(t, timersFactory(app), []string{
		timersDesc,
		`{"type":"timers","sessionId":"OTHER"}`,
		`{"type":"timer:cancel","sessionId":"OTHER","timerId":"tmr_1"}`,
		`{"type":"shutdown","sessionId":"s"}`,
	})
	if ev := firstOfType(lines, "timers:snapshot"); ev != nil {
		t.Errorf("a foreign session read the timer list: %+v", ev)
	}
	if got := app.cancelledIDs(); len(got) != 0 {
		t.Fatalf("a foreign session cancelled a timer: %v", got)
	}
}
