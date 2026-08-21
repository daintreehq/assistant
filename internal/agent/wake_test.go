package agent

import (
	"regexp"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// --- helpers ---

func makeWakeEvent(over func(*domain.QueueEvent)) domain.QueueEvent {
	e := domain.QueueEvent{
		ID:        "evt-1",
		Source:    domain.SourceTerminalWatcher,
		Severity:  domain.SeverityAttention,
		Title:     "supervised waiting: Terminal waiting for input",
		Summary:   "agent paused for input",
		CreatedAt: 1000,
		Count:     1,
	}
	if over != nil {
		over(&e)
	}
	return e
}

func termWakeEvent(terminalID string, over func(*domain.QueueEvent)) domain.QueueEvent {
	return makeWakeEvent(func(e *domain.QueueEvent) {
		e.Target = &domain.EventTarget{TerminalID: terminalID}
		if over != nil {
			over(e)
		}
	})
}

func setOf(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

// --- IsActionableWake ---

func TestIsActionableWake(t *testing.T) {
	tests := []struct {
		name string
		ev   domain.QueueEvent
		want bool
	}{
		{"terminal_watcher with terminalId", termWakeEvent("t1", nil), true},
		{"missing target", makeWakeEvent(func(e *domain.QueueEvent) { e.Target = nil }), false},
		{"empty terminalId", makeWakeEvent(func(e *domain.QueueEvent) { e.Target = &domain.EventTarget{TerminalID: ""} }), false},
		{"non-watcher source user", termWakeEvent("t1", func(e *domain.QueueEvent) { e.Source = domain.SourceUser }), false},
		{"non-watcher source system", termWakeEvent("t1", func(e *domain.QueueEvent) { e.Source = domain.SourceSystem }), false},
		// timer.schedule's description promises that a timer's own event never starts a
		// model turn — for BOTH payload types, and even when the timer carries a terminal
		// target that would otherwise look watcher-shaped. Pin it so the prose stays true.
		{"timer never wakes", termWakeEvent("t1", func(e *domain.QueueEvent) { e.Source = domain.SourceTimer }), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsActionableWake(tc.ev); got != tc.want {
				t.Fatalf("IsActionableWake = %v want %v", got, tc.want)
			}
		})
	}
}

// --- BuildWakePrompt ---

func TestBuildWakePromptFirstTimeRequestsFullSummary(t *testing.T) {
	// No opts: a first-time terminal earns the positive "give a concise update"
	// guidance and no follow-up ack marker.
	prompt := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, nil)
	if !strings.Contains(prompt, "give the user a concise update") {
		t.Fatalf("missing full-summary guidance:\n%s", prompt)
	}
	if strings.Contains(prompt, "already reported") {
		t.Fatalf("unexpected ack marker on first-time terminal:\n%s", prompt)
	}
}

func TestBuildWakePromptEmptySummarizedSameAsNoOpts(t *testing.T) {
	prompt := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, setOf())
	if !strings.Contains(prompt, "give the user a concise update") {
		t.Fatalf("missing full-summary guidance with empty set:\n%s", prompt)
	}
	if strings.Contains(prompt, "already reported") {
		t.Fatalf("unexpected ack marker with empty set:\n%s", prompt)
	}
}

func TestBuildWakePromptDowngradesFollowUp(t *testing.T) {
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised done: Terminal exited" })},
		setOf("t1"),
	)
	if !strings.Contains(prompt, "already reported") {
		t.Fatal("expected ack marker for already-summarized terminal")
	}
	if !strings.Contains(prompt, "do NOT call terminal.read/terminal.summarize/terminal.extract again") {
		t.Fatal("missing do-NOT-call directive")
	}
	if !strings.Contains(prompt, "[terminal t1]") {
		t.Fatal("per-event line must name the terminal")
	}
}

func TestBuildWakePromptAllFollowUpsSwapGuidance(t *testing.T) {
	// Every event is a follow-up: the positive "summarize and report" header must be
	// absent and acknowledge-only guidance present.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", nil), termWakeEvent("t1", nil)},
		setOf("t1"),
	)
	if strings.Contains(prompt, "give the user a concise update") {
		t.Fatal("all-follow-up batch must not include the positive update guidance")
	}
	if !strings.Contains(prompt, "Acknowledge each in one short line") {
		t.Fatal("all-follow-up batch must include acknowledge-only guidance")
	}
}

func TestBuildWakePromptFirstTimeEventLineFreeOfAckMarker(t *testing.T) {
	prompt := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, nil)
	if !strings.Contains(prompt, "give the user a concise update") {
		t.Fatal("missing full-summary guidance")
	}
	var eventLine string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, "- ") && strings.Contains(l, "[terminal t1]") {
			eventLine = l
		}
	}
	if eventLine == "" {
		t.Fatal("expected a per-event line naming terminal t1")
	}
	if strings.Contains(eventLine, "already reported") {
		t.Fatalf("first-time per-event line must not carry the ack marker: %q", eventLine)
	}
}

func TestBuildWakePromptSurfacesInboxIDForResolve(t *testing.T) {
	// The reactor needs the inbox id to resolve THIS exact item on a wake
	// turn — every per-event line must carry "(inbox <id>)".
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.ID = "evt-77" })},
		nil,
	)
	if !strings.Contains(prompt, "(inbox evt-77)") {
		t.Fatalf("event line must surface the inbox id for queue.resolve:\n%s", prompt)
	}
}

func TestBuildWakePromptInstructsInboxHygiene(t *testing.T) {
	// A finished watch, once reported, should be resolved (not cancelled — its
	// watcher already stopped itself). The hygiene guidance is present on BOTH the
	// summarize branch and the acknowledge-only branch.
	full := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, nil)
	ackOnly := BuildWakePrompt([]domain.QueueEvent{termWakeEvent("t1", nil)}, setOf("t1"))
	for name, prompt := range map[string]string{"summarize": full, "ack-only": ackOnly} {
		if !strings.Contains(prompt, "queue.resolve") {
			t.Fatalf("%s branch missing queue.resolve hygiene guidance:\n%s", name, prompt)
		}
		if !strings.Contains(prompt, "nothing left to cancel") {
			t.Fatalf("%s branch missing the already-stopped/no-cancel nuance:\n%s", name, prompt)
		}
	}
}

func TestBuildWakePromptIssue39Lifecycle(t *testing.T) {
	// A terminal summarized in one burst is a follow-up in the next — the caller
	// threads its summarizedTerminals set across bursts.
	summarized := setOf()
	first := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised waiting: Terminal waiting" })},
		summarized,
	)
	if !strings.Contains(first, "give the user a concise update") || strings.Contains(first, "already reported") {
		t.Fatal("first burst should be a full summary")
	}
	summarized["t1"] = struct{}{}
	second := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised done: Terminal exited" })},
		summarized,
	)
	if !strings.Contains(second, "already reported") {
		t.Fatal("second burst should be a follow-up ack")
	}
	if !strings.Contains(second, "do NOT call terminal.read/terminal.summarize/terminal.extract again") {
		t.Fatal("second burst missing do-NOT-call directive")
	}
}

func TestBuildWakePromptPerTerminalGranularity(t *testing.T) {
	// t1 is a follow-up, t2 is brand new and still earns a full summary.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{termWakeEvent("t1", nil), termWakeEvent("t2", nil)},
		setOf("t1"),
	)
	if !strings.Contains(prompt, "already reported") {
		t.Fatal("t1 should be a follow-up")
	}
	var t2Line string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.Contains(l, "[terminal t2]") {
			t2Line = l
		}
	}
	if t2Line == "" {
		t.Fatal("expected a per-event line for t2")
	}
	if strings.Contains(t2Line, "already reported") {
		t.Fatalf("brand-new t2 line must not carry ack marker: %q", t2Line)
	}
}

func TestBuildWakePromptFirstOccurrenceOnlyWhenSameTerminalTwice(t *testing.T) {
	// Same terminal appears twice in one batch: only the SECOND per-event line is
	// downgraded to an ack.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{
			termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised waiting: Terminal waiting" }),
			termWakeEvent("t1", func(e *domain.QueueEvent) { e.Title = "supervised done: Terminal exited" }),
		},
		setOf(),
	)
	var followUps []string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, "- ") && strings.Contains(l, "already reported") {
			followUps = append(followUps, l)
		}
	}
	if len(followUps) != 1 {
		t.Fatalf("expected exactly one downgraded per-event line, got %d:\n%s", len(followUps), prompt)
	}
	if !strings.Contains(followUps[0], "Terminal exited") {
		t.Fatalf("the downgraded line should be the second (exited) event: %q", followUps[0])
	}
}

func TestBuildWakePromptEventWithoutTerminalIDNeutral(t *testing.T) {
	// An event with no terminalId renders neutrally and never crashes; with no new
	// terminal-scoped summary needed, no ack marker leaks in.
	prompt := BuildWakePrompt(
		[]domain.QueueEvent{makeWakeEvent(func(e *domain.QueueEvent) { e.Target = nil })},
		setOf("t1"),
	)
	if !strings.Contains(prompt, "New events:") {
		t.Fatal("missing the 'New events:' section")
	}
	if strings.Contains(prompt, "already reported") {
		t.Fatalf("terminal-less event must not carry an ack marker:\n%s", prompt)
	}
}

// --- IsWakeFailureReply ---

func TestIsWakeFailureReplyRecognizesSentinels(t *testing.T) {
	failures := []string{
		"Model unavailable: 503",
		"Model rate-limited: provider quota/throughput exceeded",
		"Model error: boom",
		"Tool projection failed: dup name",
		"Turn cancelled",
		"Stopped: called watcher.terminal.create 3 times this turn with identical arguments, each failing the same way (INVALID_ARGS: ...).",
	}
	for _, reply := range failures {
		if !IsWakeFailureReply(reply) {
			t.Fatalf("expected wake-failure sentinel for %q", reply)
		}
	}
}

func TestIsWakeFailureReplyRealReplyIsSuccess(t *testing.T) {
	if IsWakeFailureReply("Terminal t1 finished cleanly; tests passed.") {
		t.Fatal("a real reply must not be a wake failure")
	}
	if IsWakeFailureReply("") {
		t.Fatal("an empty reply must not be a wake failure")
	}
}

// --- async wake events ---

func asyncWakeEvent(over func(*domain.QueueEvent)) domain.QueueEvent {
	e := domain.QueueEvent{
		ID:        "evt-async-1",
		Source:    domain.SourceAsyncTool,
		Severity:  domain.SeverityAttention,
		Title:     "Async finished: npm test",
		Summary:   `asy_1a2b "npm test": term-1: finished`,
		Target:    &domain.EventTarget{TerminalID: "term-1", AsyncInvocationID: "asy_1a2b"},
		CreatedAt: 1000,
		Count:     1,
	}
	if over != nil {
		over(&e)
	}
	return e
}

func TestIsActionableWakeAsyncSource(t *testing.T) {
	if !IsActionableWake(asyncWakeEvent(nil)) {
		t.Fatal("an async_tool completion must be actionable")
	}
	// Even without a terminal target (a grouped completion), async completions wake.
	if !IsActionableWake(asyncWakeEvent(func(e *domain.QueueEvent) { e.Target = &domain.EventTarget{AsyncInvocationID: "asy_x"} })) {
		t.Fatal("a grouped async completion (no terminal target) must still be actionable")
	}
}

func TestBuildWakePromptAsyncOnlyBurst(t *testing.T) {
	prompt := BuildWakePrompt([]domain.QueueEvent{asyncWakeEvent(nil)}, nil)
	if !strings.HasPrefix(prompt, "[automatic wake-up]") {
		t.Fatalf("missing wake prefix:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Asynchronous operation(s) you started") {
		t.Fatalf("missing async framing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- Async finished: npm test: asy_1a2b \"npm test\": term-1: finished [terminal term-1] (inbox evt-async-1)") {
		t.Fatalf("missing self-contained completion line:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Do NOT re-run the async operation") {
		t.Fatalf("missing no-rerun guidance:\n%s", prompt)
	}
	if !strings.Contains(prompt, "queue.resolve") {
		t.Fatalf("missing inbox hygiene guidance:\n%s", prompt)
	}
	// The watcher framing must NOT leak into an async-only burst.
	if strings.Contains(prompt, "background watcher") {
		t.Fatalf("watcher framing leaked into async-only burst:\n%s", prompt)
	}
}

func TestBuildWakePromptMixedBurstKeepsWatcherBodyAndAppendsAsync(t *testing.T) {
	events := []domain.QueueEvent{termWakeEvent("t1", nil), asyncWakeEvent(nil)}
	prompt := BuildWakePrompt(events, nil)
	if !strings.Contains(prompt, "A background watcher surfaced new activity") {
		t.Fatalf("mixed burst lost the watcher framing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "asynchronous operation(s) you started earlier have finished") {
		t.Fatalf("mixed burst lost the async section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "(inbox evt-async-1)") {
		t.Fatalf("async completion line missing from mixed burst:\n%s", prompt)
	}
}

func TestBuildWakePromptWatcherOnlyUnchangedByAsyncBranch(t *testing.T) {
	// The watcher-only output is model-facing contract text: the async partition
	// must be a pure pass-through when no async events are present.
	events := []domain.QueueEvent{termWakeEvent("t1", nil)}
	if got, want := BuildWakePrompt(events, nil), buildWatcherWakePrompt(events, nil); got != want {
		t.Fatalf("watcher-only burst diverged from the watcher prompt:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// --- wake-prompt / coreToolNames drift guard (issue #370) ---

// wakePromptToolNamePattern matches a dotted tool id the way one actually appears in
// RENDERED prompt text: a lowercase-initial segment followed by one or more dotted
// segments, with no whitespace across a dot. The segment body accepts everything the
// registry accepts (letters, digits, "_", "-") rather than just [A-Za-z0-9] — a
// narrower grammar would extract "queue.resolve" out of a hypothetical
// "queue.resolve-v2" and silently pass the non-core name, which is the exact drift this
// guards. Deliberately family-agnostic too: keying on known prefixes (terminal|queue)
// would miss a future prompt naming, say, watcher.cancel.
//
// It does assume the convention every registered tool follows — lowercase-initial dotted
// segments — rather than the wider shape the registry would technically accept. Matching
// a digit-initial segment (to catch a hypothetical "queue.resolve.2") would false-positive
// on every decimal in prose, which costs more than the case it buys.
//
// The two awkward real shapes both split correctly on word boundaries: the slash-joined
// run "terminal.read/terminal.summarize/terminal.extract" yields three names, and the
// JSON literal `queue.resolve {"id":"…"}` yields one. Sentence-ending periods do not
// match (a space follows), nor do possessives ("that event's line") or decimals ("1.2",
// whose post-dot segment starts with a digit). Prose CAN forge a match though — an
// "e.g." or a "main.go" added to a prompt would surface as a bogus name. That is a
// nuisance failure, not a hole: fix it by rewording the prompt or masking the phrase.
var wakePromptToolNamePattern = regexp.MustCompile(`\b[a-z][A-Za-z0-9_-]*(?:\.[a-z][A-Za-z0-9_-]*)+\b`)

// wakePromptProhibitions are the COMPLETE prohibition clauses that give a tool id its
// exemption from the core check — the prompt tells the model not to call it, so this
// prompt gives it no claim on coreToolNames. Masking the whole clause (rather than
// exempting the bare name) is what keeps the exemption honest: flip it to "DO call
// async.list", weaken it to "do NOT call async.list UNLESS …", or add a second positive
// mention elsewhere, and the surviving occurrence is still extracted and still checked.
// Match the clause through its last stable word, so a trailing qualifier breaks the
// match instead of riding along inside it. Every clause must still render, or it is
// stale.
var wakePromptProhibitions = []string{
	"do NOT call async.list to double-check",
}

// assertWakePromptToolsAreCore masks the known prohibition clauses, extracts every
// dotted tool-id candidate left in the rendered prompt, and reports each one that is not
// in coreToolNames. internal/supervisor/wake_test.go duplicates this logic for the
// daemon note it appends, so both halves of the assembled wake prompt meet one rule —
// change them together. The returned set holds the extracted names AND the prohibition
// clauses that matched, so a caller can tell a stale clause from a live one.
func assertWakePromptToolsAreCore(t *testing.T, label, prompt string, prohibitions []string) map[string]struct{} {
	t.Helper()
	core := setOf(coreToolNames...)
	seen := map[string]struct{}{}
	scrubbed := prompt
	for _, phrase := range prohibitions {
		if strings.Contains(scrubbed, phrase) {
			seen[phrase] = struct{}{}
			scrubbed = strings.ReplaceAll(scrubbed, phrase, " ")
		}
	}
	var missing []string
	for _, name := range wakePromptToolNamePattern.FindAllString(scrubbed, -1) {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, ok := core[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// Mirrors AssertRegistered's diagnostic: name every offender, not just the first.
		t.Errorf("%s names tools that are not in coreToolNames: %s\n%s",
			label, strings.Join(missing, ", "), prompt)
	}
	return seen
}

// TestBuildWakePromptNamesOnlyCoreTools pins the invariant behind issue #370: every tool
// the autonomous wake prompt tells the model to CALL must be in coreToolNames. A wake
// may run with no relevant skill active — nothing is guaranteed to reintroduce a tool
// the prompt assumed — so a prompt naming a non-core tool is an instruction the model
// may be unable to follow the moment anything projects a SUBSET of the registry.
//
// Scope: this covers BuildWakePrompt only. The daemon appends its own note to that
// output (internal/supervisor/wake.go), and TestUnattendedWakeNoteNamesOnlyCoreTools
// holds that half to the same rule; together they cover the assembled daemon prompt.
func TestBuildWakePromptNamesOnlyCoreTools(t *testing.T) {
	// Every interpolated event field is set explicitly and kept DOT-FREE. Event text is
	// rendered verbatim into the prompt, so a tool-shaped fixture value would be
	// indistinguishable from a real instruction and could fake either verdict.
	watcherEvent := func(inboxID, terminalID string) domain.QueueEvent {
		return termWakeEvent(terminalID, func(e *domain.QueueEvent) {
			e.ID = inboxID
			e.Title = "supervised finished"
			e.Summary = "agent finished cleanly"
		})
	}
	asyncEvent := asyncWakeEvent(func(e *domain.QueueEvent) {
		e.ID = "inbox-async"
		e.Title = "async finished"
		e.Summary = "one terminal finished"
		e.Target = &domain.EventTarget{TerminalID: "term-async", AsyncInvocationID: "asy-async"}
	})

	// One scenario per RENDERED branch. The marker proves the branch actually rendered,
	// so a refactor that stops emitting one can't quietly shrink the surface under test.
	scenarios := []struct {
		name              string
		events            []domain.QueueEvent
		alreadySummarized map[string]struct{}
		branchMarker      string
	}{
		{
			name:         "watcher summarize guidance",
			events:       []domain.QueueEvent{watcherEvent("inbox-new", "term-new")},
			branchMarker: "give the user a concise update",
		},
		{
			name:              "watcher acknowledge-only guidance",
			events:            []domain.QueueEvent{watcherEvent("inbox-known", "term-known")},
			alreadySummarized: setOf("term-known"),
			branchMarker:      "Every event below is a terminal you have already reported this session",
		},
		{
			name: "watcher mixed new and already-reported",
			events: []domain.QueueEvent{
				watcherEvent("inbox-known", "term-known"),
				watcherEvent("inbox-new", "term-new"),
			},
			alreadySummarized: setOf("term-known"),
			branchMarker:      "Some events below are marked (already reported)",
		},
		{
			name:         "async-only burst",
			events:       []domain.QueueEvent{asyncEvent},
			branchMarker: "Asynchronous operation(s) you started earlier have finished",
		},
		{
			name:         "mixed watcher and async section",
			events:       []domain.QueueEvent{watcherEvent("inbox-new", "term-new"), asyncEvent},
			branchMarker: "Also: asynchronous operation(s) you started earlier have finished",
		},
		// A bare event renders wakeEventLine's FALLBACK title and skips every optional
		// fragment. Without these two the fallbacks are unreached, so a tool id added to
		// one of them ("event — call watcher.list") would slip past the guard.
		{
			name:         "watcher event with no title or ids",
			events:       []domain.QueueEvent{{Source: domain.SourceTerminalWatcher}},
			branchMarker: "- event",
		},
		{
			name:         "async event with no title or ids",
			events:       []domain.QueueEvent{{Source: domain.SourceAsyncTool}},
			branchMarker: "- async operation",
		},
	}

	seen := map[string]struct{}{}
	for _, tc := range scenarios {
		prompt := BuildWakePrompt(tc.events, tc.alreadySummarized)
		if !strings.Contains(prompt, tc.branchMarker) {
			t.Errorf("%s: branch never rendered, missing marker %q:\n%s", tc.name, tc.branchMarker, prompt)
		}
		for name := range assertWakePromptToolsAreCore(t, tc.name, prompt, wakePromptProhibitions) {
			seen[name] = struct{}{}
		}
	}

	// A prohibition that gets reworded or deleted must not leave a silent mask behind —
	// that would hide the next real drift instead of reporting it.
	var stale []string
	for _, phrase := range wakePromptProhibitions {
		if _, ok := seen[phrase]; !ok {
			stale = append(stale, phrase)
		}
	}
	if len(stale) > 0 {
		t.Errorf("wake prompt prohibitions no longer rendered verbatim (re-check the wording, then drop or update them): %q", stale)
	}
}
