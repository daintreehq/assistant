package backend

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRespondRequest_AlwaysSerializesStartupValue(t *testing.T) {
	b, err := json.Marshal(RespondRequest{
		Session: RespondSession{ID: "s", TurnID: "t"},
		Startup: StartupContext{},
		Input: RespondInput{Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"startup":{}`) {
		t.Fatalf("required empty startup value missing: %s", b)
	}
}

func TestRuntimeContext_WorktreeReadStates(t *testing.T) {
	unavailable, err := json.Marshal(RuntimeContext{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unavailable), `"worktree"`) {
		t.Fatalf("unavailable read should omit worktree: %s", unavailable)
	}

	none, err := json.Marshal(RuntimeContext{Worktree: &CurrentWorktreeSnapshot{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(none), `"worktree":{"current":null}`) {
		t.Fatalf("definitive none lost current:null: %s", none)
	}

	current, err := json.Marshal(RuntimeContext{Worktree: &CurrentWorktreeSnapshot{
		Current: &WorktreeSnapshot{ID: "wt-1", Branch: "feature/x", IsMain: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), `"current":{"id":"wt-1","branch":"feature/x","is_main":false}`) {
		t.Fatalf("typed current worktree wire mismatch: %s", current)
	}
}

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

// The geometry serializes under the keys the backend reads, and an unmeasured surface
// omits the block entirely — the backend distinguishes "no display block" (fall back to
// its own default width) from a reported one, so a zero-filled block would be a lie.
func TestDisplayInfo_WireShape(t *testing.T) {
	b, err := json.Marshal(RuntimeContext{PermissionTier: "operator", Display: NewDisplayInfo(120, 97)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"display":{"columns":120,"content_width":97}`) {
		t.Fatalf("display wire mismatch: %s", b)
	}

	absent, err := json.Marshal(RuntimeContext{PermissionTier: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(absent), "display") {
		t.Fatalf("an unmeasured surface must omit display: %s", absent)
	}
}

// NewDisplayInfo is the boundary that keeps a bad measurement off the wire: the backend
// VALIDATES runtime before it uses it, so an out-of-range width would 422 the whole turn
// rather than degrade to a default.
func TestNewDisplayInfoBounds(t *testing.T) {
	if got := NewDisplayInfo(80, 0); got != nil {
		t.Errorf("a zero content width means unmeasured, want nil, got %+v", got)
	}
	if got := NewDisplayInfo(80, -5); got != nil {
		t.Errorf("a negative content width must not reach the wire, got %+v", got)
	}
	got := NewDisplayInfo(1<<30, 1<<30)
	if got == nil || got.Columns != displayWidthMax || got.ContentWidth != displayWidthMax {
		t.Errorf("absurd geometry must clamp to the contract bound, got %+v", got)
	}
	// A terminal so narrow only one column remains is still a real, reportable state, so
	// the CLI reports it: clamping here is for nonsense, not for a genuinely tiny window.
	// What the server then DOES with a hostile width is its own call (it floors anything
	// under 20 cells back to its default), and that judgement belongs there, not in a
	// client that quietly rewrote the measurement first.
	if got := NewDisplayInfo(1, 1); got == nil || got.ContentWidth != 1 || got.Columns != 1 {
		t.Errorf("a 1-cell terminal is measurable and must survive, got %+v", got)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// THE pre-#54 safety regression. Selection is validated with extra="forbid" server-side,
// so a `pinned_runbook_ids` key on a deployment that predates the field 422s the whole turn
// before the model opens. omitempty is what keeps an unpinned turn's bytes identical to
// what every existing backend already accepts — if this ever regresses, every session
// against an older deployment breaks at once, and the CLI has no way to tell why.
func TestSelectionOmitsPinnedRunbookIDsWhenUnpinned(t *testing.T) {
	b, err := json.Marshal(Selection{Policy: "new_instruction"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "pinned_runbook_ids") {
		t.Fatalf("an unpinned selection must not mention the field at all: %s", b)
	}
	if got, want := string(b), `{"policy":"new_instruction"}`; got != want {
		t.Fatalf("unpinned selection = %s, want %s (byte-identical to the pre-feature wire)", got, want)
	}
	// An EMPTY list is the same statement as no list, and must serialize the same way:
	// a caller who normalized their pins down to nothing must not accidentally probe a
	// capability they are not using.
	empty, err := json.Marshal(Selection{Policy: "new_instruction", PinnedRunbookIDs: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "pinned_runbook_ids") {
		t.Fatalf("an empty pin list must be omitted too: %s", empty)
	}
}

// Order is part of the request. The backend admits pins in the order given and budgets
// them against max_active_runbooks, so a re-ordered list is a genuinely different ask.
func TestSelectionSerializesPinnedRunbookIDsInOrder(t *testing.T) {
	b, err := json.Marshal(Selection{Policy: "new_instruction", PinnedRunbookIDs: []string{"b.two", "a.one"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"pinned_runbook_ids":["b.two","a.one"]`) {
		t.Fatalf("pins missing or reordered on the wire: %s", b)
	}
}

// nil and empty are DIFFERENT answers from the capability endpoint and nothing may
// collapse them: nil means the deployment cannot answer what it can load, empty means it
// answered "nothing". The first cannot validate an id; the second knows every id is wrong.
func TestCapabilitiesDistinguishesAbsentFromEmptyRunbookCatalog(t *testing.T) {
	var absent Capabilities
	if err := json.Unmarshal([]byte(`{"runbooks":{"catalog_revision":"r","manual_resolve":true}}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Runbooks.Catalog != nil {
		t.Fatalf("a backend that omits the catalog must decode to nil, got %#v", absent.Runbooks.Catalog)
	}
	if absent.Runbooks.PinnedRunbookIDs {
		t.Fatal("an absent pinned_runbook_ids capability must decode false — the gate fails closed")
	}

	var empty Capabilities
	if err := json.Unmarshal([]byte(`{"runbooks":{"catalog":[],"pinned_runbook_ids":true}}`), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Runbooks.Catalog == nil {
		t.Fatal("an advertised empty catalog must decode to a non-nil empty slice")
	}
	if !empty.Runbooks.PinnedRunbookIDs {
		t.Fatal("pinned_runbook_ids:true must decode true")
	}

	var full Capabilities
	if err := json.Unmarshal([]byte(
		`{"runbooks":{"catalog":[{"id":"a.one","title":"One"}],"catalog_revision":"sha256:x","pinned_runbook_ids":true}}`), &full); err != nil {
		t.Fatal(err)
	}
	if len(full.Runbooks.Catalog) != 1 || full.Runbooks.Catalog[0].ID != "a.one" || full.Runbooks.Catalog[0].Title != "One" {
		t.Fatalf("catalog entry decoded wrong: %#v", full.Runbooks.Catalog)
	}
	if full.Runbooks.CatalogRevision != "sha256:x" {
		t.Fatalf("catalog revision = %q, want sha256:x (it is what a cache keys the list on)", full.Runbooks.CatalogRevision)
	}
}
