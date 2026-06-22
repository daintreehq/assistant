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

// The lifecycle note differs across scheduler running / not / absent. Watchers do
// NOT resume across sessions (unlike timers).
func TestTerminalCreateLifecycleNotice(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "watcher.terminal.create")
	args := json.RawMessage(`{"terminalIds":["t1"],"title":"build","goal":"green"}`)

	on := true
	running := tool.Handle(context.Background(), args, ctxDaemon(&on))
	if !running.Ok || !strings.Contains(running.Summary, "session-scoped") ||
		!strings.Contains(running.Summary, "does not resume") ||
		strings.Contains(running.Summary, "scheduler is NOT running") {
		t.Fatalf("running note: %q", running.Summary)
	}

	off := false
	stopped := tool.Handle(context.Background(), args, ctxDaemon(&off))
	if !stopped.Ok || !strings.Contains(stopped.Summary, "scheduler is NOT running") ||
		!strings.Contains(stopped.Summary, "will not check") {
		t.Fatalf("stopped note: %q", stopped.Summary)
	}

	absent := tool.Handle(context.Background(), args, ctxDaemon(nil))
	if !absent.Ok || !strings.Contains(absent.Summary, "session-scoped") {
		t.Fatalf("absent note: %q", absent.Summary)
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
