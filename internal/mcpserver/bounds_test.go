package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// Every page size existed only as a DEFAULT, which is not a bound: a caller choosing
// maxEvents:100000 got them. A default protects the caller that does not think about it;
// a maximum protects the server from the caller that does.
func TestPageSizesAreClampedNotJustDefaulted(t *testing.T) {
	for _, tc := range []struct {
		name                string
		requested, def, max int
		want                int
	}{
		{"unset takes the default", 0, 40, 500, 40},
		{"negative takes the default", -1, 40, 500, 40},
		{"a modest request is honoured", 100, 40, 500, 100},
		{"an absurd request is clamped", 100000, 40, 500, 500},
		{"exactly the max is honoured", 500, 40, 500, 500},
	} {
		if got := clampPageSize(tc.requested, tc.def, tc.max); got != tc.want {
			t.Errorf("%s: clampPageSize(%d, %d, %d) = %d, want %d",
				tc.name, tc.requested, tc.def, tc.max, got, tc.want)
		}
	}
}

// A poll must never silently truncate: a caller that cannot see it was paged reads a
// partial timeline as the whole one.
func TestAnOverLargePollIsClampedAndSaysSo(t *testing.T) {
	fake := newFakeRuntime("ses_bounds")
	fake.script = func(sink agent.EventSink) {
		for i := 0; i < MaxPollEvents+120; i++ {
			sink.Info("step")
		}
		sink.AssistantEnd("done", "")
	}
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	fake.letFinish()
	run, err := sess.Ask(context.Background(), "x", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Done()

	out := renderRun(run, 0, clampPageSize(1_000_000, defaultPollEvents, MaxPollEvents), sess.Approvals())
	if len(out.Events) != MaxPollEvents {
		t.Fatalf("a request for a million events returned %d, want the server maximum of %d",
			len(out.Events), MaxPollEvents)
	}
	if out.WithheldEvents == 0 {
		t.Error("the response was truncated without saying so")
	}
	if got := len(out.Events) + out.WithheldEvents; got != out.TotalEvents {
		t.Errorf("shown+withheld = %d, but the run has %d events — the count does not add up", got, out.TotalEvents)
	}
}

// Snapshot slices on the Seq==index identity rather than scanning. The window it returns
// must be exactly right at every boundary, and must be a COPY: the returned slice
// outlives the lock, and appending to the run can write into the same backing array.
func TestSnapshotWindowsAreExactAndCopied(t *testing.T) {
	run := NewRun("mrun_slice", "ses", "p", func() {})
	for i := 0; i < 10; i++ {
		run.append(Event{Type: "info", Text: "e"})
	}

	evs, remaining, _, _, _, _, _, _ := run.Snapshot(0, 0)
	if len(evs) != 10 || remaining != 0 {
		t.Fatalf("unbounded snapshot returned %d events, %d remaining", len(evs), remaining)
	}
	evs, remaining, _, _, _, _, _, _ = run.Snapshot(7, 0)
	if len(evs) != 3 || evs[0].Seq != 7 {
		t.Fatalf("sinceSeq=7 returned %d events starting at seq %d", len(evs), evs[0].Seq)
	}
	evs, _, _, _, _, _, _, _ = run.Snapshot(10, 0)
	if len(evs) != 0 {
		t.Errorf("sinceSeq at the tail returned %d events, want none", len(evs))
	}
	// Past the end must clamp, not panic or wrap.
	evs, _, _, _, _, _, _, _ = run.Snapshot(9999, 0)
	if len(evs) != 0 {
		t.Errorf("sinceSeq past the end returned %d events", len(evs))
	}
	// Negative means "from the start", not an offset from the end.
	evs, _, _, _, _, _, _, _ = run.Snapshot(-5, 0)
	if len(evs) != 10 {
		t.Errorf("a negative sinceSeq returned %d events, want all 10", len(evs))
	}

	// The copy: mutating the returned slice must not reach the run, and appending to
	// the run must not reach a window already handed out.
	evs, _, _, _, _, _, _, _ = run.Snapshot(0, 4)
	evs[0].Text = "clobbered"
	for i := 0; i < 20; i++ {
		run.append(Event{Type: "info", Text: "more"})
	}
	fresh, _, _, _, _, _, _, _ := run.Snapshot(0, 1)
	if fresh[0].Text == "clobbered" {
		t.Error("Snapshot aliased the run's own event slice; a caller mutated it")
	}
}

// Args on an approval can be a whole file's worth of content. A caller deciding whether
// to allow a call needs their shape, not all of them.
func TestApprovalPreviewsAndContentAreBounded(t *testing.T) {
	long := strings.Repeat("a", MaxApprovalPreviewBytes*3)
	got := truncateBytes(long, MaxApprovalPreviewBytes)
	if len(got) >= len(long) {
		t.Fatalf("a %d-byte preview was not truncated", len(long))
	}
	if !strings.Contains(got, "truncated") {
		t.Error("the truncation is silent; a caller cannot tell it is looking at part of the arguments")
	}

	// Under the cap, nothing is touched — not even a marker.
	short := "git push origin main"
	if truncateBytes(short, MaxApprovalPreviewBytes) != short {
		t.Error("a short value was modified")
	}
}

// truncateBytes cuts on a rune boundary. A byte-exact cut through a multi-byte character
// produces invalid UTF-8, which a JSON encoder then silently replaces — so the caller
// sees corruption rather than truncation.
func TestTruncationNeverSplitsARune(t *testing.T) {
	// Every rune here is 3 bytes, so a cap that is not a multiple of 3 must walk back.
	s := strings.Repeat("界", 100)
	for _, max := range []int{10, 11, 12, 13, 50, 99} {
		got := truncateBytes(s, max)
		head, _, _ := strings.Cut(got, "\n")
		if !utf8ValidString(head) {
			t.Errorf("truncating to %d bytes produced invalid UTF-8", max)
		}
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// The inbox is DURABLE and unbounded — a project left running overnight accumulates every
// watcher finding and async completion — so the first read after a long detachment must
// not be the whole night in one response.
// The page has to be applied INSIDE the runtime, before anything is acknowledged.
//
// Two things go wrong otherwise. The inbox is durable and can hold a night's worth of
// rows, so paging after the fetch still materializes all of them. And acknowledgement is
// version-conditional on the exact rows READ — so acknowledging a fetch the handler then
// truncated stamps rows nobody received, and re-reading to acknowledge a page consumes a
// NEWER version than the one that was shown, silently swallowing an update.
func TestAttentionPagesInsideTheRuntimeAndAcknowledgesOnlyWhatItReturned(t *testing.T) {
	fake := newFakeRuntime("ses_inbox")
	for i := 0; i < 300; i++ {
		fake.attention = append(fake.attention, domain.QueueEvent{ID: domain.NewID("q_"), Title: "finding"})
	}
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	events, more, err := sess.Attention(context.Background(), 50, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 50 {
		t.Fatalf("the page returned %d items, want 50", len(events))
	}
	if !more {
		t.Error("250 items were left behind without saying so")
	}
	fake.mu.Lock()
	consumed := append([]string(nil), fake.ackedIDs...)
	fake.mu.Unlock()
	if len(consumed) != 50 {
		t.Fatalf("%d items were acknowledged but 50 were delivered — the rest are lost", len(consumed))
	}
	for i, id := range consumed {
		if id != events[i].ID {
			t.Fatalf("acknowledged %q but delivered %q", id, events[i].ID)
		}
	}

	if got := clampPageSize(1000, 50, MaxAttentionItems); got != MaxAttentionItems {
		t.Errorf("an over-large inbox page was not clamped: %d", got)
	}
}

// Same overflow as the run deadline, with a worse ending: NewApprovals reads a
// non-positive timeout as "use the default", so a caller asking for an enormous approval
// window silently got five minutes — and the dispatch it thought it had an hour to answer
// is denied while it is still deciding.
func TestApprovalTimeoutValidatesBeforeConverting(t *testing.T) {
	if d, err := resolveApprovalTimeout(0); err != nil || d != 0 {
		t.Errorf("an omitted timeout gave (%v, %v), want the broker's default", d, err)
	}
	if _, err := resolveApprovalTimeout(-1); err == nil {
		t.Error("a negative timeout was accepted")
	}
	if _, err := resolveApprovalTimeout(1 << 53); err == nil {
		t.Error("an overflowing timeout was accepted rather than refused")
	}
	over := int(MaxApprovalTimeout/time.Millisecond) + 1
	if _, err := resolveApprovalTimeout(over); err == nil {
		t.Error("a timeout above the server maximum was accepted")
	}
	if d, err := resolveApprovalTimeout(60_000); err != nil || d != time.Minute {
		t.Errorf("a valid timeout gave (%v, %v)", d, err)
	}
}

// waitBudget had the same wrap: converting a large millisecond count straight to a
// Duration wraps int64 nanoseconds negative, which then takes the same branch as "unset".
func TestWaitBudgetDoesNotOverflow(t *testing.T) {
	if got := waitBudget(1 << 53); got != maxBlockWait {
		t.Errorf("an overflowing wait gave %v, want the cap %v", got, maxBlockWait)
	}
	if got := waitBudget(0); got != maxBlockWait {
		t.Errorf("an unset wait gave %v, want the cap %v", got, maxBlockWait)
	}
	if got := waitBudget(5000); got != 5*time.Second {
		t.Errorf("a valid wait gave %v", got)
	}
}

// The transcript resource used to return every retained event, which made it the largest
// single response this server could produce — reachable by a caller with no idea how long
// the run was.
func TestTranscriptURIParsesItsPage(t *testing.T) {
	sessionID, runID, page, err := parseRunURI("daintree://session/ses_1/run/mrun_1")
	if err != nil || sessionID != "ses_1" || runID != "mrun_1" {
		t.Fatalf("plain URI: %q %q %v", sessionID, runID, err)
	}
	if page.fromSeq != 0 || page.limit != MaxPollEvents {
		t.Errorf("an unpaged URI gave %+v, want the server page size", page)
	}

	_, _, page, err = parseRunURI("daintree://session/ses_1/run/mrun_1?fromSeq=400&limit=100")
	if err != nil {
		t.Fatalf("paged URI: %v", err)
	}
	if page.fromSeq != 400 || page.limit != 100 {
		t.Errorf("page = %+v, want fromSeq 400 limit 100", page)
	}

	// An over-large limit is clamped, not honoured and not refused.
	_, _, page, err = parseRunURI("daintree://session/ses_1/run/mrun_1?limit=999999")
	if err != nil || page.limit != MaxPollEvents {
		t.Errorf("an over-large limit gave %+v (%v)", page, err)
	}

	for _, bad := range []string{
		"daintree://session/ses_1/run/mrun_1?fromSeq=-1",
		"daintree://session/ses_1/run/mrun_1?fromSeq=abc",
		"daintree://session/ses_1/run/mrun_1?limit=-5",
	} {
		if _, _, _, err := parseRunURI(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// A page COUNT is not a size bound. 500 events whose text is unbounded is unbounded, and
// event text carries a round's whole assistant answer, a flushed prose buffer, or a tool
// result summary — so the per-event cap and the page cap are both load-bearing.
func TestEventTextIsBoundedPerEventAndInAggregate(t *testing.T) {
	huge := strings.Repeat("x", MaxEventTextBytes*4)
	evs := make([]Event, 200)
	for i := range evs {
		evs[i] = Event{Type: "info", Text: huge, Summary: huge, Error: huge}
	}

	bounded, dropped := boundEventText(evs)
	if dropped == 0 {
		t.Fatal("200 oversized events fitted the aggregate budget; the budget is not applied")
	}
	if len(bounded)+dropped != len(evs) {
		t.Errorf("kept %d + dropped %d != %d input events", len(bounded), dropped, len(evs))
	}

	total := 0
	for _, e := range bounded {
		if len(e.Text) > MaxEventTextBytes+200 {
			t.Errorf("an event's text survived at %d bytes, past the %d cap", len(e.Text), MaxEventTextBytes)
		}
		total += len(e.Text) + len(e.Summary) + len(e.Error)
	}
	// The budget is checked before each event, so one event may overshoot it; what must
	// not happen is unbounded growth.
	if total > MaxResponseTextBytes+3*(MaxEventTextBytes+200) {
		t.Errorf("the response carried %d bytes of event text against a %d budget", total, MaxResponseTextBytes)
	}
}

// Small events must pass through completely untouched — the bound exists for the
// pathological case, and a marker appended to ordinary output would be noise in every
// response.
func TestOrdinaryEventsAreNotTouchedByTheBound(t *testing.T) {
	evs := []Event{
		{Type: "tool:result", Tool: "terminal.list", Summary: "3 terminals", Text: "ok"},
		{Type: "assistant:end", Text: "Two worktrees are ready."},
	}
	bounded, dropped := boundEventText(evs)
	if dropped != 0 {
		t.Fatalf("two small events were dropped for budget")
	}
	if bounded[0].Summary != "3 terminals" || bounded[1].Text != "Two worktrees are ready." {
		t.Errorf("small events were modified: %+v", bounded)
	}
}

// A ceiling the truncation itself pushes you over is not a ceiling, and the overshoot
// compounds — every event in a page can carry one marker.
func TestTruncationMarkerIsCountedInsideTheMaximum(t *testing.T) {
	// The ceiling holds at every size, including ones too small to hold the marker.
	for _, max := range []int{1, 4, 16, 64, 256, 4096} {
		got := truncateBytes(strings.Repeat("x", max*4+64), max)
		if len(got) > max {
			t.Errorf("truncating to %d bytes produced %d bytes — the marker escaped the bound", max, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncating to %d bytes produced invalid UTF-8", max)
		}
	}
	// At any size a real field is actually capped to, the marker survives — a truncation
	// the caller cannot see is the one failure mode worse than truncating at all.
	for _, max := range []int{64, 256, MaxEventTextBytes, MaxApprovalPreviewBytes} {
		got := truncateBytes(strings.Repeat("x", max*4), max)
		if !strings.Contains(got, "truncated") {
			t.Errorf("truncating to %d bytes lost the marker: %q", max, got)
		}
	}
}

// The caps were applied in one place and bypassed in two: the run projection truncated
// the argument preview while daintree.approvals returned the whole object and the
// elicitation message interpolated the raw string. A bound that one of three exits
// honours is not a bound.
func TestEveryApprovalExitUsesTheSameBoundedProjection(t *testing.T) {
	huge := strings.Repeat("s", MaxApprovalPreviewBytes*4)
	pa := PendingApproval{ID: "apr_1", Tool: "git.push", Args: huge, Consequence: huge, Summary: huge}

	bounded := boundedApproval(pa)
	if len(bounded.Args) > MaxApprovalPreviewBytes {
		t.Errorf("args survived at %d bytes", len(bounded.Args))
	}
	if len(bounded.Consequence) > MaxEventTextBytes || len(bounded.Summary) > MaxEventTextBytes {
		t.Errorf("consequence/summary survived at %d/%d bytes", len(bounded.Consequence), len(bounded.Summary))
	}

	// The listing projection caps the COUNT too, and says what it left out.
	many := make([]PendingApproval, MaxPendingApprovals+7)
	for i := range many {
		many[i] = pa
	}
	out, remaining := boundedApprovals(many, MaxPendingApprovals)
	if len(out) != MaxPendingApprovals || remaining != 7 {
		t.Errorf("boundedApprovals returned %d with %d remaining, want %d and 7",
			len(out), remaining, MaxPendingApprovals)
	}
	for _, got := range out {
		if len(got.Args) > MaxApprovalPreviewBytes {
			t.Fatal("a listed approval escaped the preview cap")
		}
	}

	// And the elicitation message, which interpolates the preview.
	msg := elicitMessage(boundedApproval(pa))
	if len(msg) > maxElicitMessageBytes {
		t.Errorf("the elicitation message reached %d bytes against a %d bound; it interpolates the preview",
			len(msg), maxElicitMessageBytes)
	}
	if !strings.Contains(msg, "git.push") {
		t.Error("bounding the message dropped the tool name, which is the one thing a decision needs")
	}
}

// Taking the events, then the total, then the async ledger as three separate reads
// produced responses describing a run that never existed: a page could report
// complete:true beside a total that had already grown past it.
func TestRunSnapshotIsOneConsistentRead(t *testing.T) {
	run := NewRun("mrun_snap", "ses", "p", func() {})
	for i := 0; i < 25; i++ {
		run.append(Event{Type: "info", Text: "e"})
	}
	snap := run.SnapshotFull(0, 10)
	if len(snap.Events) != 10 || snap.Remaining != 15 || snap.TotalEvents != 25 {
		t.Fatalf("snapshot = %d events, %d remaining, %d total", len(snap.Events), snap.Remaining, snap.TotalEvents)
	}
	if got := len(snap.Events) + snap.Remaining; got != snap.TotalEvents {
		t.Errorf("shown+remaining = %d but total = %d, from one hold", got, snap.TotalEvents)
	}
	if snap.NextSeq != 10 {
		t.Errorf("nextSeq = %d, want 10", snap.NextSeq)
	}
}

// An out-of-range cursor used to be echoed straight back, so a caller that asked for
// fromSeq 9999 on a 25-event run was told to continue at 9999 — and would then skip every
// event the run went on to produce.
func TestAnOutOfRangeCursorIsNormalizedToTheTail(t *testing.T) {
	run := NewRun("mrun_cursor", "ses", "p", func() {})
	for i := 0; i < 25; i++ {
		run.append(Event{Type: "info", Text: "e"})
	}
	snap := run.SnapshotFull(9999, 10)
	if len(snap.Events) != 0 {
		t.Fatalf("a cursor past the tail returned %d events", len(snap.Events))
	}
	if snap.NextSeq != 25 {
		t.Errorf("nextSeq = %d for an out-of-range cursor, want the tail at 25 — otherwise every "+
			"later event falls below the cursor and is skipped", snap.NextSeq)
	}
	// And continuing from there picks up exactly what arrives next.
	run.append(Event{Type: "info", Text: "the next one"})
	next := run.SnapshotFull(snap.NextSeq, 10)
	if len(next.Events) != 1 || next.Events[0].Text != "the next one" {
		t.Errorf("continuing from the normalized cursor returned %d events", len(next.Events))
	}
}

// Event carries a Skills slice, which a plain struct copy leaves aliased to the retained
// event — a caller could mutate the run's own history, and a JSON encode could race an
// append.
func TestSnapshotDeepCopiesEventSkills(t *testing.T) {
	run := NewRun("mrun_skills", "ses", "p", func() {})
	run.append(Event{Type: "skill:loaded", Skills: []string{"daintree.orchestration.edit"}})

	snap := run.SnapshotFull(0, 0)
	snap.Events[0].Skills[0] = "clobbered"

	fresh := run.SnapshotFull(0, 0)
	if fresh.Events[0].Skills[0] == "clobbered" {
		t.Error("Snapshot aliased Event.Skills; a caller mutated the run's retained history")
	}
}
