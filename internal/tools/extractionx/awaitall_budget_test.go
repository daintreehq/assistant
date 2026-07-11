package extractionx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/waitbudget"
)

// These tests pin the per-turn cumulative foreground-wait budget contract on
// terminal.awaitAll: a budget carried on the ctx is drawn down by the poll sleeps;
// hitting zero mid-wait stops the poll and returns the normal stillWorking shape plus
// the machine-readable budgetExhausted marker; a call arriving with zero balance
// returns immediately; and a ctx WITHOUT a budget (every pre-existing caller/test)
// behaves exactly as before budgets existed.

// stuckReader reports every requested terminal permanently working, with a live
// roster so the handler's id-resolution passes. statusReads counts poll ticks.
type stuckReader struct {
	live        []string
	statusReads int
	listCalls   int
}

func (r *stuckReader) Connected() bool { return true }
func (r *stuckReader) ListTerminals(context.Context) ([]string, bool) {
	r.listCalls++
	return r.live, true
}
func (r *stuckReader) ReadStatuses(_ context.Context, ids []string, _ bool) StatusReadResult {
	r.statusReads++
	byID := make(map[string]TerminalStatusEntry, len(ids))
	for _, id := range ids {
		byID[id] = ent("working", "", "grinding")
	}
	return StatusReadResult{OK: true, ByID: byID}
}
func (r *stuckReader) ReadOutput(context.Context, string, int) OutputReadResult {
	return OutputReadResult{OK: false}
}

func awaitAllWithBudget(t *testing.T, reader *stuckReader, budget *waitbudget.Budget, args string) map[string]any {
	t.Helper()
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}})
	ctx := waitbudget.With(context.Background(), budget)
	res := tool.Handle(ctx, json.RawMessage(args), nil)
	if !res.Ok {
		t.Fatalf("awaitAll under budget should degrade, never fail: %+v", res.Error)
	}
	m, ok := res.Result.(map[string]any)
	if !ok {
		t.Fatalf("result shape = %T, want map", res.Result)
	}
	return m
}

// Exhaustion MID-WAIT: a budget that covers only a couple of poll sleeps stops the
// wait far short of maxAttempts, keeps the normal result shape (stillWorking lists
// the stragglers), sets budgetExhausted:true, and the note routes the model to the
// watcher/async handoff instead of a re-await.
func TestAwaitAll_BudgetExhaustedMidPollReturnsStillWorking(t *testing.T) {
	reader := &stuckReader{live: []string{"t1", "t2"}}
	budget := waitbudget.New(12 * time.Millisecond) // ~2 sleeps of 5ms, then cut off
	m := awaitAllWithBudget(t, reader, budget,
		`{"terminalIds":["t1","t2"],"pollIntervalMs":5,"maxAttempts":240}`)

	if be, _ := m["budgetExhausted"].(bool); !be {
		t.Fatalf("mid-wait exhaustion must set budgetExhausted:true, got %+v", m)
	}
	if af, _ := m["allFinished"].(bool); af {
		t.Fatal("nothing settled — allFinished must be false")
	}
	if sw, _ := m["stillWorking"].([]string); len(sw) != 2 {
		t.Fatalf("both stragglers should be stillWorking, got %v", sw)
	}
	attempts, _ := m["attempts"].(int)
	if attempts == 0 || attempts >= 240 {
		t.Fatalf("the wait must stop early on the budget, not the 240 cap; attempts = %d", attempts)
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "watcher.terminal.create") || !strings.Contains(note, "do NOT") {
		t.Fatalf("the note must route to the async handoff and forbid re-awaiting, got %q", note)
	}
	if !budget.Exhausted() {
		t.Fatal("the shared budget should be fully drawn down")
	}
}

// A SECOND call on the same drained budget short-circuits: no roster resolve, no
// status read, zero attempts — the same shape with budgetExhausted:true, immediately.
func TestAwaitAll_SecondCallOnDrainedBudgetShortCircuits(t *testing.T) {
	reader := &stuckReader{live: []string{"t1"}}
	budget := waitbudget.New(6 * time.Millisecond)
	_ = awaitAllWithBudget(t, reader, budget, `{"terminalIds":["t1"],"pollIntervalMs":5,"maxAttempts":240}`)
	if !budget.Exhausted() {
		t.Fatal("first call should drain the budget")
	}
	listCalls, statusReads := reader.listCalls, reader.statusReads

	m := awaitAllWithBudget(t, reader, budget, `{"terminalIds":["t1"],"pollIntervalMs":5,"maxAttempts":240}`)
	if be, _ := m["budgetExhausted"].(bool); !be {
		t.Fatalf("a zero-balance call must report budgetExhausted, got %+v", m)
	}
	if attempts, _ := m["attempts"].(int); attempts != 0 {
		t.Fatalf("a zero-balance call must not poll, attempts = %d", attempts)
	}
	if sw, _ := m["stillWorking"].([]string); len(sw) != 1 || sw[0] != "t1" {
		t.Fatalf("the requested terminal should be echoed as stillWorking, got %v", sw)
	}
	if reader.listCalls != listCalls || reader.statusReads != statusReads {
		t.Fatalf("a zero-balance call must make NO MCP reads (list %d→%d, status %d→%d)",
			listCalls, reader.listCalls, statusReads, reader.statusReads)
	}
}

// interruptedByUser is UNTOUCHED by budgeting: with plenty of budget left, a pending
// injection still breaks the wait early, flags interruptedByUser, keeps the user-first
// note, and does NOT report budgetExhausted.
func TestAwaitAll_UserInterruptUnchangedUnderBudget(t *testing.T) {
	reader := &stuckReader{live: []string{"t1"}}
	tool := newAwaitAllTool(Deps{
		Reader:            reader,
		Router:            &safeRouter{},
		InjectionsPending: func() bool { return true },
	})
	ctx := waitbudget.With(context.Background(), waitbudget.New(time.Hour))
	res := tool.Handle(ctx, json.RawMessage(`{"terminalIds":["t1"],"pollIntervalMs":5,"maxAttempts":240}`), nil)
	if !res.Ok {
		t.Fatalf("interrupted wait should still be ok, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if iu, _ := m["interruptedByUser"].(bool); !iu {
		t.Fatalf("a pending injection must still interrupt, got %+v", m)
	}
	if _, present := m["budgetExhausted"]; present {
		t.Fatalf("a barely-touched budget must not be reported exhausted, got %+v", m)
	}
	if note, _ := m["note"].(string); !strings.Contains(note, "user sent a message") {
		t.Fatalf("the user-interrupt note must win, got %q", note)
	}
}

// An UNBUDGETED ctx (every caller that doesn't wire waitbudget — including all
// pre-existing tests) behaves exactly as before: the wait runs to its own attempt cap
// and the result carries no budgetExhausted key.
func TestAwaitAll_UnbudgetedContextBehavesAsBefore(t *testing.T) {
	reader := &stuckReader{live: []string{"t1"}}
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}})
	res := tool.Handle(context.Background(), json.RawMessage(`{"terminalIds":["t1"],"pollIntervalMs":0,"maxAttempts":3}`), nil)
	if !res.Ok {
		t.Fatalf("unbudgeted wait failed: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if attempts, _ := m["attempts"].(int); attempts != 3 {
		t.Fatalf("unbudgeted wait should run to its own cap (3), attempts = %d", attempts)
	}
	if _, present := m["budgetExhausted"]; present {
		t.Fatalf("no budget in ctx ⇒ no budgetExhausted key, got %+v", m)
	}
}

// The tool surface itself must state the ENFORCED budget so the model learns the rule
// at point-of-use even with no orchestration skill loaded (the same lock the re-await
// bound has).
func TestAwaitAllTool_DocumentsEnforcedBudget(t *testing.T) {
	tool := newAwaitAllTool(Deps{})
	if !strings.Contains(tool.Description, "120s") || !strings.Contains(tool.Description, "budgetExhausted") {
		t.Error("awaitAll Description should state the enforced 120s cumulative budget and the budgetExhausted marker")
	}
	if !strings.Contains(string(tool.Schema), "budgetExhausted") {
		t.Error("awaitAll maxAttempts schema description should mention the enforced budget cutoff")
	}
}
