package agent

// golden_compact_test.go is the "golden transcript replay" guard for compaction
// correctness (issue #259). Where compact_test.go exercises the mechanics of each
// compaction path in isolation, this file pins the load-bearing INVARIANTS a long run
// depends on — the ones whose silent regression would only surface as a confused
// orchestrator many turns later:
//
//  1. (removed) there is no longer a client-side control-message prefix to preserve —
//     the backend owns the system prompt + skills, so compaction operates on a history
//     that begins at index 0 with user/assistant/tool messages only;
//  2. (removed) distillation dedup/cap/truncate is server-owned now (memory_distill);
//  3. live identifiers (term_*/run_*/wch_*/wfr_*, matching domain.PrefixWatcher /
//     domain.PrefixWorkflow) present before compaction survive into the summarizer input
//     (auto path) and into the persisted note (manual path), so a mid-run compaction
//     never strands the references the orchestrator still needs;
//  4. a PURE tool-call assistant turn (no prose, only ToolCalls) contributes the tool
//     name + argument JSON to the summarizer input, not a bare "[tool call]" placeholder
//     — the only place the older history's load-bearing IDs (which live ONLY in the
//     arguments) can reach the text-only summarizer.
//
// Scope note: issue #259 lists the structured-checkpoint object (#256) as something this
// guard covers. #256 has since landed (see checkpoint.go), so the auto path now renders
// the "[checkpoint | depth N]" framing rather than the old prose summary, and invariant (1)
// is pinned against that. The other invariants are format-agnostic and unchanged.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// summaryInputCaptureRouter records the FULL message slice handed to the small model,
// not just the system instruction (promptCaptureRouter captures only Messages[0]).
// Assertions 3 and 4 need the flattened conversation history fed to the summarizer —
// summaryMsgs[1:] in maybeAutoCompact — to prove IDs and tool-call expansions reach it.
// It copies the slice on capture (opts.Messages may be reused by the caller) and is
// safe for the single summary Chat call compactSession drives (MemoryStore is nil, so
// the background distill goroutine returns before making a second, racing Chat call).
type summaryInputCaptureRouter struct {
	mu      sync.Mutex
	summary string
	msgs    []models.ChatMessage
	calls   int
}

func (r *summaryInputCaptureRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}

func (r *summaryInputCaptureRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.mu.Lock()
	r.calls++
	r.msgs = append([]models.ChatMessage(nil), opts.Messages...)
	r.mu.Unlock()
	return models.ChatResult{Content: r.summary}, nil
}

func (r *summaryInputCaptureRouter) ModelFor(domain.ModelTier) string { return "deepseek-v4-flash" }
func (r *summaryInputCaptureRouter) FlushMeter() []models.TierUsage   { return nil }

func (r *summaryInputCaptureRouter) captured() []models.ChatMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]models.ChatMessage(nil), r.msgs...)
}

// callCount is the number of Chat calls captured. The auto tests assert it is exactly 1
// (only the summary call), making the "MemoryStore is nil ⇒ no racing distill Chat call"
// assumption an explicit, self-verifying invariant rather than a silent comment: if a
// second call ever sneaks in, captured() would hold the wrong input and the count guard
// trips instead of an assertion passing for the wrong reason.
func (r *summaryInputCaptureRouter) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// snapshotControls copies the three control messages (a defensive copy so a later
// reslice/append in s.messages cannot mutate the comparison baseline).
func snapshotControls(s *Session) []models.ChatMessage {
	all := s.Messages()
	return append([]models.ChatMessage(nil), all[:domain.ControlMessageCount]...)
}

// assertControlsUnchanged fails if any of the three control messages differs from the
// pre-compaction snapshot. reflect.DeepEqual compares the whole struct (role + content
// + parts + tool fields), so a change to ANY field of a control message trips it.
func assertControlsUnchanged(t *testing.T, before []models.ChatMessage, s *Session) {
	t.Helper()
	after := s.Messages()
	if len(after) < domain.ControlMessageCount {
		t.Fatalf("history dropped below %d control messages: len=%d", domain.ControlMessageCount, len(after))
	}
	for i := 0; i < domain.ControlMessageCount; i++ {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Fatalf("control message %d changed across compaction:\n before=%+v\n after =%+v", i, before[i], after[i])
		}
	}
}

// goldenForceAutoCompact drives both auto-path tests: two working messages clear the
// "no real history to summarize" guard (len(messages) <= ControlMessageCount+1), and a
// real prompt figure over the soft threshold clears the size gate without allocating a
// quarter-megabyte filler string just to trip the char estimate. Caller already holds
// nothing; this takes the lock itself.
func goldenForceAutoCompact(s *Session, working ...models.ChatMessage) {
	s.mu.Lock()
	s.messages = append(s.messages, working...)
	s.lastPromptTokens = domain.AutoCompactTokenThreshold + 1
	s.mu.Unlock()
}

// TestGoldenCompactControlMessagesSurviveByteIdentical pins invariant (1): neither
// compaction path rebuilds or reorders the three control messages — they pass through
// untouched, so the cached prompt prefix and the loaded skills stay stable across an
// arbitrarily long, repeatedly-compacted run.
func TestGoldenCompactControlMessagesSurviveByteIdentical(t *testing.T) {
	// Load a real skill first so messages[2] is a NON-trivial loaded-skills body, not the
	// empty "no skills" default. Otherwise a regression that rebuilt the controls from
	// fresh defaults would still match the snapshot (default == default) and pass — the
	// loaded skill gives the byte-equality check real bite.
	t.Run("manual", func(t *testing.T) {
		s, _ := compactSession(t, plainRouter())
		before := snapshotControls(s)
		s.InjectNote("alpha")
		s.InjectNote("beta")
		if err := s.Compact("goals: X. open: none. next: Y."); err != nil {
			t.Fatalf("Compact: %v", err)
		}
		assertControlsUnchanged(t, before, s)
		// Guard against a vacuous pass: confirm a compaction note actually replaced the
		// working history (controls + exactly one summary note).
		after := s.Messages()
		if len(after) != domain.ControlMessageCount+1 {
			t.Fatalf("manual compact = %d messages, want %d (controls + 1 note)", len(after), domain.ControlMessageCount+1)
		}
	})

	t.Run("auto", func(t *testing.T) {
		r := &summaryInputCaptureRouter{summary: "S"}
		s, _ := compactSession(t, r)
		before := snapshotControls(s)
		goldenForceAutoCompact(s,
			models.TextMessage("user", "alpha"),
			models.TextMessage("assistant", "beta"),
		)
		s.maybeAutoCompact(context.Background(), "run_golden")
		assertControlsUnchanged(t, before, s)
		if r.callCount() != 1 {
			t.Fatalf("summarizer Chat calls = %d, want exactly 1", r.callCount())
		}
		// The checkpoint note must sit immediately after the controls — proves the auto path
		// actually compacted rather than skipping the gate. Since #256 (the structured
		// checkpoint) landed, the auto path renders the "[checkpoint | depth N]" framing
		// (compactionNotePrefix) instead of the old prose "compacted summary".
		after := s.Messages()
		if len(after) <= domain.ControlMessageCount ||
			!strings.Contains(after[domain.ControlMessageCount].StringContent, "[checkpoint | depth") {
			t.Fatalf("auto compact did not produce a checkpoint note: %+v", after)
		}
	})
}

// (TestGoldenParseDistilledFactsDedupCapTruncate was removed: distillation dedup/cap/
// truncate is now server-owned — the backend's memory_distill task does it — so the
// local prompts.ParseDistilledFacts it exercised no longer exists.)

// TestGoldenAutoCompactIDsSurviveInSummarizerInput pins invariant (3) for the auto path:
// load-bearing IDs sitting in the history reach the summarizer's conversation input, so
// the ID-preservation instruction has something concrete to act on.
func TestGoldenAutoCompactIDsSurviveInSummarizerInput(t *testing.T) {
	const (
		termID    = "term_abc123"
		runID     = "run_def456"
		watcherID = "wch_ghi789" // domain.PrefixWatcher
		wkfID     = "wfr_jkl012" // domain.PrefixWorkflow
	)
	r := &summaryInputCaptureRouter{summary: "S"}
	s, _ := compactSession(t, r)
	goldenForceAutoCompact(s,
		models.TextMessage("user", "working on "+termID+" and "+runID+" via "+watcherID+" in "+wkfID),
		models.TextMessage("assistant", "ack"),
	)
	s.maybeAutoCompact(context.Background(), "run_golden")

	captured := r.captured()
	if len(captured) == 0 {
		t.Fatal("summarizer was never called (auto-compact gate not tripped)")
	}
	if r.callCount() != 1 {
		t.Fatalf("summarizer Chat calls = %d, want exactly 1", r.callCount())
	}
	// The CLI now sends only the flattened transcript to the checkpoint task (no
	// client-owned system instruction), so scan the whole captured input: a match proves
	// the load-bearing IDs flowed through the discarded history into the summarizer input.
	var body strings.Builder
	for _, m := range captured {
		body.WriteString(m.StringContent)
		body.WriteByte('\n')
	}
	for _, id := range []string{termID, runID, watcherID, wkfID} {
		if !strings.Contains(body.String(), id) {
			t.Fatalf("summarizer input dropped ID %q:\n%s", id, body.String())
		}
	}
}

// TestGoldenManualCompactStoresNoteVerbatim pins the manual side of invariant (3) at the
// storage layer: whatever summary the /compact command produces, compactLocked must
// persist it verbatim — IDs intact, untruncated, unmangled. It deliberately guards ONLY
// compactLocked, not the upstream transcript-building in internal/commands
// (handlers_ui.go transcriptString), which is a separate code path with its own tests.
// (That path currently emits a bare "[tool call]" for empty-content turns, so tool-arg
// IDs in the manual flow are a known pre-existing gap outside this test's scope.)
func TestGoldenManualCompactStoresNoteVerbatim(t *testing.T) {
	const (
		termID    = "term_xyz999"
		runID     = "run_uvw888"
		watcherID = "wch_rst777" // domain.PrefixWatcher
		wkfID     = "wfr_opq666" // domain.PrefixWorkflow
	)
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("history")
	summary := fmt.Sprintf("goals: ship. live refs: %s %s %s %s.", termID, runID, watcherID, wkfID)
	if err := s.Compact(summary); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	msgs := s.Messages()
	if len(msgs) <= domain.ControlMessageCount {
		t.Fatalf("compaction note missing: len=%d", len(msgs))
	}
	note := msgs[domain.ControlMessageCount].StringContent
	for _, id := range []string{termID, runID, watcherID, wkfID} {
		if !strings.Contains(note, id) {
			t.Fatalf("manual compaction note dropped ID %q: %q", id, note)
		}
	}
}

// TestGoldenAutoCompactPureToolCallExpandsNameArgs pins invariant (4): a pure tool-call
// assistant turn (ContentNull, only ToolCalls) contributes "[tool call NAME ARGS]" to
// the summarizer input — never a bare "[tool call]". This is the only path by which IDs
// that live solely in tool arguments (e.g. terminal.read {"terminalId":"term_x"}) reach
// the text-only summarizer; regressing to the bare placeholder would silently strand
// them.
func TestGoldenAutoCompactPureToolCallExpandsNameArgs(t *testing.T) {
	const (
		toolName = "terminal.read"
		toolArgs = `{"terminalId":"term_tool42"}`
	)
	r := &summaryInputCaptureRouter{summary: "S"}
	s, _ := compactSession(t, r)
	goldenForceAutoCompact(s,
		models.TextMessage("user", "please read the terminal"),
		// A PURE tool-call assistant turn: ContentNull, no prose, only ToolCalls.
		models.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: []models.ToolCallRequest{{
			ID: "call_1", Type: "function",
			Function: models.ToolCallFunction{Name: toolName, Arguments: toolArgs},
		}}},
	)
	s.maybeAutoCompact(context.Background(), "run_golden")

	captured := r.captured()
	if len(captured) == 0 {
		t.Fatal("summarizer was never called (auto-compact gate not tripped)")
	}
	if r.callCount() != 1 {
		t.Fatalf("summarizer Chat calls = %d, want exactly 1", r.callCount())
	}
	want := "[tool call " + toolName + " " + toolArgs + "]"
	sawExpanded, sawBarePlaceholder := false, false
	for _, m := range captured {
		if strings.Contains(m.StringContent, want) {
			sawExpanded = true
		}
		if strings.TrimSpace(m.StringContent) == "[tool call]" {
			sawBarePlaceholder = true
		}
	}
	if !sawExpanded {
		t.Fatalf("pure tool-call turn did not contribute name+args %q to summarizer input:\n%+v", want, captured)
	}
	if sawBarePlaceholder {
		t.Fatal("summarizer input contains a bare [tool call] placeholder; tool name+args were lost")
	}
}
