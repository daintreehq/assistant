package watcher

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

func ctxDaemon(active *bool) *tools.ToolContext {
	if active == nil {
		return &tools.ToolContext{}
	}
	return &tools.ToolContext{DaemonActive: func() bool { return *active }}
}

// The full WatchCondition validation matrix, exercised through the create tool's
// strict decode (a degenerate condition must be rejected so a watcher can fire).
func TestTerminalCreateWatchConditionMatrix(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "watcher.terminal.create")
	base := `{"terminalIds":["t1"],"title":"x","goal":"g","stopWhen":`

	reject := []string{
		`{"contains":""}`,         // empty contains
		`{"contains":"   "}`,      // whitespace-only contains
		`{"regex":"["}`,           // invalid regex
		`{"regex":""}`,            // empty regex
		`{"noOutputForMs":0}`,     // zero
		`{"noOutputForMs":-1}`,    // negative
		`{"modelJudge":""}`,       // empty modelJudge
		`{"modelJudge":"  "}`,     // whitespace modelJudge
		`{"not":{"contains":""}}`, // invalid leaf wrapped in not
		`{"all":[]}`,              // empty all
		`{"any":[]}`,              // empty any
		`{"all":[{"any":[]}]}`,    // nested invalid
	}
	for _, cond := range reject {
		res := tool.Handle(context.Background(), json.RawMessage(base+cond+`}`), &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeInvalidArgs {
			t.Errorf("expected INVALID_ARGS for stopWhen=%s, got %+v", cond, res)
		}
	}

	accept := []string{
		`{"contains":"done"}`,
		`{"regex":"done|failed"}`,
		`{"noOutputForMs":1}`,
		`{"all":[{"contains":"done"},{"runtimeStatusIs":"exited"}]}`,
	}
	for _, cond := range accept {
		res := tool.Handle(context.Background(), json.RawMessage(base+cond+`}`), &tools.ToolContext{})
		if !res.Ok {
			t.Errorf("expected ok for stopWhen=%s, got %+v", cond, res.Error)
		}
	}
}

// Lifecycle behaviour across scheduler running / stopped / absent. When the
// daemon runs (or is absent → assume-active), creation succeeds with the
// session-scoped foreground-only NOTE. When the daemon is stopped (one-shot /
// --json mode), creation hard-fails with a non-retryable
// WATCHER_REQUIRES_INTERACTIVE and inserts nothing. Watchers do NOT resume
// across sessions (unlike timers).
func TestTerminalCreateLifecycleNotice(t *testing.T) {
	args := json.RawMessage(`{"terminalIds":["t1"],"title":"build","goal":"green"}`)

	// Each sub-case gets a fresh store so its insert count is unambiguous.
	on := true
	stOn := &memStore{}
	running := find(Tools(Deps{Store: stOn}), "watcher.terminal.create").
		Handle(context.Background(), args, ctxDaemon(&on))
	if !running.Ok || !strings.Contains(running.Summary, "session-scoped") ||
		!strings.Contains(running.Summary, "does not resume") ||
		strings.Contains(running.Summary, "scheduler is NOT running") {
		t.Fatalf("running note: %q", running.Summary)
	}
	if len(stOn.inserted) != 1 {
		t.Fatalf("running should insert exactly one watcher, got %d", len(stOn.inserted))
	}

	off := false
	stOff := &memStore{}
	stopped := find(Tools(Deps{Store: stOff}), "watcher.terminal.create").
		Handle(context.Background(), args, ctxDaemon(&off))
	if stopped.Ok || stopped.Error.Code != codeWatcherRequiresInteractive || stopped.Error.Recoverable {
		t.Fatalf("stopped: expected non-retryable %s, got %+v", codeWatcherRequiresInteractive, stopped)
	}
	if len(stOff.inserted) != 0 {
		t.Fatalf("stopped must not insert a watcher row, got %d", len(stOff.inserted))
	}

	absent := find(Tools(Deps{Store: &memStore{}}), "watcher.terminal.create").
		Handle(context.Background(), args, ctxDaemon(nil))
	if !absent.Ok || !strings.Contains(absent.Summary, "session-scoped") {
		t.Fatalf("absent note: %q", absent.Summary)
	}
}

// Arg validation must run before the daemon gate, so a call with both invalid
// args and an inactive daemon reports INVALID_ARGS — not
// WATCHER_REQUIRES_INTERACTIVE — and inserts nothing.
func TestWatcherCreateArgsBeatDaemonGate(t *testing.T) {
	off := false
	cases := []struct {
		tool string
		args string
	}{
		{"watcher.terminal.create", `{"terminalIds":[],"title":"x","goal":"g"}`},
		{"watcher.watchPR", `{"prNumber":0}`},
	}
	for _, tc := range cases {
		st := &memStore{}
		res := find(Tools(Deps{Store: st}), tc.tool).
			Handle(context.Background(), json.RawMessage(tc.args), ctxDaemon(&off))
		if res.Ok || res.Error.Code != codeInvalidArgs {
			t.Fatalf("%s: expected %s to beat the daemon gate, got %+v", tc.tool, codeInvalidArgs, res)
		}
		if len(st.inserted) != 0 {
			t.Fatalf("%s: invalid args must not insert, got %d", tc.tool, len(st.inserted))
		}
	}
}

// watchPR hard-fails identically when the daemon is inactive: non-retryable
// WATCHER_REQUIRES_INTERACTIVE and zero rows inserted (no orphan).
func TestWatchPRDaemonInactive(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "watcher.watchPR")

	off := false
	res := tool.Handle(context.Background(), json.RawMessage(`{"prNumber":42}`), ctxDaemon(&off))
	if res.Ok || res.Error.Code != codeWatcherRequiresInteractive || res.Error.Recoverable {
		t.Fatalf("expected non-retryable %s, got %+v", codeWatcherRequiresInteractive, res)
	}
	if len(st.inserted) != 0 {
		t.Fatalf("inactive daemon must not insert a watcher row, got %d", len(st.inserted))
	}
}

// watchPR persists cwd into optionsJson and title/stopAfterMs into the record.
func TestWatchPRPersistsCwdAndStopAfter(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "watcher.watchPR")
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"prNumber":7,"cwd":"/repo","title":"my pr","stopAfterMs":5000}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	w := st.inserted[0]
	if w.Title != "my pr" {
		t.Fatalf("title: %q", w.Title)
	}
	if w.StopAfterMs == nil || *w.StopAfterMs != 5000 {
		t.Fatalf("stopAfterMs: %v", w.StopAfterMs)
	}
	if w.OptionsJson == nil {
		t.Fatal("optionsJson is nil")
	}
	var opts map[string]any
	_ = json.Unmarshal([]byte(*w.OptionsJson), &opts)
	if opts["cwd"] != "/repo" {
		t.Fatalf("cwd not persisted: %v", opts["cwd"])
	}
	if opts["prNumber"] != float64(7) {
		t.Fatalf("prNumber: %v", opts["prNumber"])
	}
	// First check has no baseline yet.
	if _, hasState := opts["lastState"]; hasState {
		t.Fatal("lastState should be absent on first check")
	}
}

// grantRevokingStore records the actors whose grants were revoked so we can assert
// watcher.cancel revokes the cancelled watcher's live grants.
type grantRevokingStore struct {
	memStore
	revokedActors []string
}

func (s *grantRevokingStore) RevokeGrantsByActor(_ context.Context, actorID string) (int, error) {
	s.revokedActors = append(s.revokedActors, actorID)
	return 1, nil
}

func TestWatcherCancelRevokesGrants(t *testing.T) {
	st := &grantRevokingStore{}
	st.inserted = []domain.WatcherRecord{{ID: "wch_1", Status: "active"}}
	tool := find(Tools(Deps{Store: st}), "watcher.cancel")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"wch_1"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.revokedActors) != 1 || st.revokedActors[0] != "wch_1" {
		t.Fatalf("expected grant revoke for cancelled watcher, got %v", st.revokedActors)
	}
}
