package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// warnSink records the non-fatal warnings a session raised. Pin refusals are the ONE
// runbook outcome this CLI surfaces (backend runbook loading is deliberately invisible), so
// these tests are what keep that carve-out narrow.
type warnSink struct {
	NoopEventSink
	mu    sync.Mutex
	warns []string
}

func (w *warnSink) Warn(m string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.warns = append(w.warns, m)
}

func (w *warnSink) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.warns...)
}

// pinnedSession wires a session with pins, a capability gate and a warning sink.
func pinnedSession(t *testing.T, r Router, tr ToolRunner, pins []string, gate func() bool, sink EventSink) (*Session, *recordingBackend) {
	t.Helper()
	deps, be := recordingDeps(r, tr)
	deps.PinnedRunbookIDs = pins
	deps.BackendAcceptsPinnedRunbookIDs = gate
	if sink != nil {
		deps.Events = sink
	}
	return NewSession(deps), be
}

func openGate() bool   { return true }
func closedGate() bool { return false }

// An unpinned session's selection block must be byte-for-byte what it always was. The
// backend validates Selection with extra="forbid", so a field that leaks onto an ordinary
// turn 422s every session against a deployment that predates the feature.
func TestSelectionCarriesNoPinsWhenNoneWereNamed(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s, be := pinnedSession(t, r, &fakeTools{}, nil, openGate, nil)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	reqs := be.requests()
	if len(reqs) == 0 {
		t.Fatal("no request recorded")
	}
	sel := reqs[0].Selection
	if sel == nil {
		t.Fatal("selection block missing")
	}
	if sel.Policy != "new_instruction" {
		t.Fatalf("policy = %q, want new_instruction (unchanged)", sel.Policy)
	}
	if sel.PinnedRunbookIDs != nil {
		t.Fatalf("an unpinned turn attached pins: %v", sel.PinnedRunbookIDs)
	}
}

// Pins ride EVERY round, not just the first: runbook selection is re-run per the policy,
// so a pin that only reached round 0 would silently drop out of the rest of the turn —
// the exact half-honoured outcome the flag exists to rule out.
func TestPinsRideEveryRound(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{{ID: "c1", Type: "function", Function: models.ToolCallFunction{Name: "fs__read", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	tr := &fakeTools{result: domain.Ok("ok", nil)}
	s, be := pinnedSession(t, r, tr, []string{"a.one", "b.two"}, openGate, nil)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	reqs := be.requests()
	if len(reqs) < 2 {
		t.Fatalf("rounds = %d, want >= 2", len(reqs))
	}
	for i, req := range reqs {
		if req.Selection == nil {
			t.Fatalf("round %d has no selection block", i)
		}
		got := req.Selection.PinnedRunbookIDs
		if len(got) != 2 || got[0] != "a.one" || got[1] != "b.two" {
			t.Fatalf("round %d pins = %v, want [a.one b.two] in order", i, got)
		}
	}
}

// The session must not hand the deps' own slice to the request: a serializer or a
// backend fake that sorted it in place would silently reorder what every later round
// pins, and order is part of the request (the backend budgets pins against its cap in
// the order given).
func TestPinsAreCopiedPerRound(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	pins := []string{"a.one", "b.two"}
	s, be := pinnedSession(t, r, &fakeTools{}, pins, openGate, nil)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	sent := be.requests()[0].Selection.PinnedRunbookIDs
	sent[0] = "mutated"
	if pins[0] != "a.one" {
		t.Fatal("the request aliased the session's pin slice")
	}
}

// A closed gate means the live endpoint does not advertise the field, and sending it
// would 422 the whole turn. Dropping it keeps the turn alive — but silently dropping it
// reproduces exactly the invisible-no-op this feature exists to prevent, so it must say
// so, once.
func TestClosedGateWithholdsPinsAndSaysSoOnce(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{{ID: "c1", Type: "function", Function: models.ToolCallFunction{Name: "fs__read", Arguments: "{}"}}}},
		{Content: "done"},
	}}
	sink := &warnSink{}
	s, be := pinnedSession(t, r, &fakeTools{result: domain.Ok("ok", nil)}, []string{"a.one"}, closedGate, sink)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	for i, req := range be.requests() {
		if req.Selection != nil && len(req.Selection.PinnedRunbookIDs) > 0 {
			t.Fatalf("round %d sent pins to a backend that forbids the field: %v", i, req.Selection.PinnedRunbookIDs)
		}
	}
	warns := sink.all()
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one across a two-round turn", warns)
	}
	if !strings.Contains(warns[0], "WITHOUT the pinned runbooks") {
		t.Fatalf("the warning does not say what actually happened: %q", warns[0])
	}
	// It must name the CAUSE. In production the only way to reach this is `/backend`
	// swapping the endpoint in place, and "this backend does not support pinning" would
	// be a wrong guess — the pins were negotiated fine at launch.
	if !strings.Contains(warns[0], "endpoint changed") {
		t.Fatalf("the warning blames the wrong thing: %q", warns[0])
	}
}

// A nil gate is the test/library default and must fail CLOSED, like every other
// capability read in this codebase.
func TestNilGateFailsClosed(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s, be := pinnedSession(t, r, &fakeTools{}, []string{"a.one"}, nil, &warnSink{})
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := be.requests()[0].Selection.PinnedRunbookIDs; len(got) > 0 {
		t.Fatalf("a nil capability gate must not open: %v", got)
	}
}

// Every pin-refusal code the backend can raise must reach the operator — that visibility
// IS the feature. `--runbook` exists so a runbook failure is distinguishable from a
// selection failure, and a refusal reported nowhere puts you right back to guessing.
func TestEveryPinFailureCodeSurfaces(t *testing.T) {
	for code, want := range map[string]string{
		"unknown_runbook_id_ignored":    "did not recognise",
		"pinned_runbook_not_executable": "not executable",
		"pinned_runbook_over_cap":       "active-runbook limit",
	} {
		t.Run(code, func(t *testing.T) {
			sink := &warnSink{}
			s, _ := pinnedSession(t, plainRouter(), &fakeTools{}, []string{"a.one"}, openGate, sink)
			s.applyStreamMeta(backend.StreamMeta{Warnings: []string{code}})
			warns := sink.all()
			if len(warns) != 1 {
				t.Fatalf("warnings = %v, want one", warns)
			}
			if !strings.Contains(warns[0], want) {
				t.Fatalf("warning %q does not explain %q", warns[0], code)
			}
			// The raw code rides along so a log reader and a script can both key on it.
			if !strings.Contains(warns[0], code) {
				t.Fatalf("warning %q drops the machine-readable code", warns[0])
			}
		})
	}
}

// One cause, one warning — for the whole SESSION, not just the turn. The pin list is
// session-constant, so the refusal is identical on every round of every turn; repeating
// it would bury the tool activity the operator is actually reading.
func TestPinWarningsAreReportedOncePerSession(t *testing.T) {
	sink := &warnSink{}
	s, _ := pinnedSession(t, plainRouter(), &fakeTools{}, []string{"a.one"}, openGate, sink)
	for i := 0; i < 4; i++ {
		s.applyStreamMeta(backend.StreamMeta{Warnings: []string{"unknown_runbook_id_ignored"}})
	}
	if warns := sink.all(); len(warns) != 1 {
		t.Fatalf("warnings = %v, want one after four identical metas", warns)
	}
	// A DIFFERENT cause is still news.
	s.applyStreamMeta(backend.StreamMeta{Warnings: []string{"pinned_runbook_over_cap"}})
	if warns := sink.all(); len(warns) != 2 {
		t.Fatalf("warnings = %v, want a second warning for a second cause", warns)
	}
}

// The allowlist is the whole design. Backend runbook loading is deliberately invisible in
// this CLI — no card, no cue, no /runbooks — because the delta a load card showed was
// misleading. Only pin REFUSALS are carved out, so any other warning code the backend
// adds must stay diagnostic-only rather than quietly resurrecting that surfacing.
func TestUnrelatedBackendWarningsStaySilent(t *testing.T) {
	sink := &warnSink{}
	s, _ := pinnedSession(t, plainRouter(), &fakeTools{}, []string{"a.one"}, openGate, sink)
	s.applyStreamMeta(backend.StreamMeta{Warnings: []string{
		"output_lint_markdown_table", "some_future_code", "newly_loaded",
	}})
	if warns := sink.all(); len(warns) != 0 {
		t.Fatalf("non-pin warnings must not be surfaced: %v", warns)
	}
}

// A pin refusal reported to someone who never pinned anything describes a bug the client
// cannot explain — and it would be the first "runbook" message an ordinary session ever
// showed, against the invisibility invariant.
func TestPinWarningsStaySilentWhenNothingWasPinned(t *testing.T) {
	sink := &warnSink{}
	s, _ := pinnedSession(t, plainRouter(), &fakeTools{}, nil, openGate, sink)
	s.applyStreamMeta(backend.StreamMeta{Warnings: []string{"unknown_runbook_id_ignored"}})
	if warns := sink.all(); len(warns) != 0 {
		t.Fatalf("an unpinned session must never report a pin refusal: %v", warns)
	}
}

// Greg's hard rule, and a hard invariant in CLAUDE.md: a loaded or pinned runbook NEVER
// narrows the callable toolset. Pinning is a wire field and nothing else.
func TestPinningLeavesTheToolInventoryUntouched(t *testing.T) {
	full := []models.ChatTool{
		{Function: models.ChatToolFunc{Name: "fs__read"}},
		{Function: models.ChatToolFunc{Name: "timer__schedule"}},
	}
	tools := &captureStreamTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}, full: full}
	s, _ := pinnedSession(t, plainRouter(), tools, []string{"a.one"}, openGate, nil)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.last != nil {
		t.Fatalf("a pinned runbook narrowed the toolset to %v; the filter must stay nil", tools.last)
	}
}
