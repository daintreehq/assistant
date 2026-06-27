package backend

import (
	"encoding/json"
	"strings"
	"testing"
)

// The open-terminal inventory must serialize with snake_case keys (the backend reads them)
// and exit_code must distinguish a clean 0 exit (present) from "no exit code" (absent) —
// the reason ExitCode is a pointer.
func TestOpenTerminal_WireShape(t *testing.T) {
	zero := 0
	rc := RuntimeContext{OpenTerminals: []OpenTerminal{
		{ID: "terminal-1", WorktreeID: "/wt/a", AgentState: "exited", ExitCode: &zero},
		{ID: "terminal-2", AgentState: "running"},
	}}
	b, err := json.Marshal(rc)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		OpenTerminals []map[string]any `json:"open_terminals"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.OpenTerminals) != 2 {
		t.Fatalf("want 2 terminals, got %d", len(parsed.OpenTerminals))
	}
	t1 := parsed.OpenTerminals[0]
	if _, ok := t1["worktree_id"]; !ok {
		t.Errorf("expected snake_case worktree_id, got keys %v", keysOf(t1))
	}
	if v, ok := t1["exit_code"]; !ok || v.(float64) != 0 {
		t.Errorf("a clean 0 exit must serialize as exit_code:0, got %v (present=%v)", v, ok)
	}
	if _, ok := parsed.OpenTerminals[1]["exit_code"]; ok {
		t.Errorf("a terminal with no exit code must omit exit_code, got %v", parsed.OpenTerminals[1])
	}
}

// An empty inventory is omitted from the wire entirely (omitempty) so an old backend that
// has not yet learned the field is never sent an empty list to choke on.
func TestRuntimeContext_OmitsEmptyOpenTerminals(t *testing.T) {
	b, err := json.Marshal(RuntimeContext{PermissionTier: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "open_terminals") {
		t.Fatalf("an empty inventory must be omitted, got %s", b)
	}
}

// Clamp truncates each over-limit field to its backend max_length (rune count) so a verbose
// agent value cannot trip the backend's pre-sanitization validation, while in-limit values
// pass through untouched.
func TestOpenTerminal_Clamp(t *testing.T) {
	in := OpenTerminal{
		ID:            "terminal-1",
		Title:         strings.Repeat("t", openTerminalTitleMax+50),
		WaitingReason: strings.Repeat("w", openTerminalWaitingReasonMax+50),
		AgentState:    "running",
	}
	got := in.Clamp()
	if len([]rune(got.Title)) != openTerminalTitleMax {
		t.Errorf("title should clamp to %d, got %d", openTerminalTitleMax, len([]rune(got.Title)))
	}
	if len([]rune(got.WaitingReason)) != openTerminalWaitingReasonMax {
		t.Errorf("waiting_reason should clamp to %d, got %d", openTerminalWaitingReasonMax, len([]rune(got.WaitingReason)))
	}
	if got.ID != "terminal-1" || got.AgentState != "running" {
		t.Errorf("in-limit fields must pass through untouched, got %+v", got)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
