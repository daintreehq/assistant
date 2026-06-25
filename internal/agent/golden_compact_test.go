package agent

// golden_compact_test.go is the "golden transcript replay" guard for compaction
// correctness (issue #259). Where compact_test.go exercises the mechanics of each
// compaction path in isolation, this file pins the load-bearing INVARIANTS a long run
// depends on — the ones whose silent regression would only surface as a confused
// orchestrator many turns later:
//
//  1. the three control messages (base prompt, runtime context, loaded skills) survive
//     compaction unchanged, on BOTH the manual /compact and the in-turn auto paths;
//  2. ParseDistilledFacts dedups, caps, and truncates in a single pass (the durable
//     memory mined from a discarded transcript stays bounded and clean);
//  3. live identifiers (term_*/run_*/watcher_*/wkf_*) present before compaction survive
//     into the summarizer input (auto path) and into the persisted note (manual path),
//     so a mid-run compaction never strands the references the orchestrator still needs;
//  4. a PURE tool-call assistant turn (no prose, only ToolCalls) contributes the tool
//     name + argument JSON to the summarizer input, not a bare "[tool call]" placeholder
//     — the only place the older history's load-bearing IDs (which live ONLY in the
//     arguments) can reach the text-only summarizer.
//
// Scope note: issue #259 lists the structured-checkpoint object (#256) as something this
// guard will eventually cover, but #256 is not yet merged (no checkpoint.go exists), so
// these tests assert only the behavior that ships today. New assertions can be appended
// here when the checkpoint lands.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
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
}

func (r *summaryInputCaptureRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}

func (r *summaryInputCaptureRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.mu.Lock()
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

// goldenWorkingMessages drives both auto-path tests: two working messages clear the
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
		s, _ := compactSession(t, &summaryInputCaptureRouter{summary: "S"})
		before := snapshotControls(s)
		goldenForceAutoCompact(s,
			models.TextMessage("user", "alpha"),
			models.TextMessage("assistant", "beta"),
		)
		s.maybeAutoCompact(context.Background(), "run_golden")
		assertControlsUnchanged(t, before, s)
		// The summary note must sit immediately after the controls — proves the auto path
		// actually compacted rather than skipping the gate.
		after := s.Messages()
		if len(after) <= domain.ControlMessageCount ||
			!strings.Contains(after[domain.ControlMessageCount].StringContent, "compacted summary") {
			t.Fatalf("auto compact did not produce a summary note: %+v", after)
		}
	})
}

// TestGoldenParseDistilledFactsDedupCapTruncate pins invariant (2) end-to-end in one
// ParseDistilledFacts call (distill_test.go covers each behavior in isolation; this is
// the integration guard the issue asks for): a single noisy reply with a blank, a
// case-insensitive duplicate, an over-long fact, and more uniques than the cap must
// yield exactly MaxDistilledFacts clean, deduped, truncated entries.
func TestGoldenParseDistilledFactsDedupCapTruncate(t *testing.T) {
	const longPrefix = "L"
	long := strings.Repeat(longPrefix, 500) // > maxDistilledFactRunes (200) ⇒ truncated

	raw := []string{
		"  Project pins Go 1.25  ", // leading/trailing space trimmed
		long,                       // truncated to 200 runes
		"project pins go 1.25",     // case-insensitive dup of #1 ⇒ dropped
		"",                         // blank ⇒ dropped
		"   ",                      // whitespace-only ⇒ dropped
	}
	// Pad past the cap so the count is forced down to MaxDistilledFacts.
	for i := 0; i < prompts.MaxDistilledFacts+3; i++ {
		raw = append(raw, fmt.Sprintf("unique fact %02d", i))
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}

	got := prompts.ParseDistilledFacts(string(blob))

	if len(got) != prompts.MaxDistilledFacts {
		t.Fatalf("len = %d, want cap %d", len(got), prompts.MaxDistilledFacts)
	}
	// Dedup: the case-insensitive pair collapses to exactly one surviving entry.
	dupCount := 0
	for _, f := range got {
		if strings.EqualFold(f, "project pins go 1.25") {
			dupCount++
		}
	}
	if dupCount != 1 {
		t.Fatalf("case-insensitive duplicate not collapsed: count=%d in %v", dupCount, got)
	}
	// Truncation: the 500-rune fact is cut to maxDistilledFactRunes (200, unexported in
	// prompts — pinned here as the literal the package documents).
	sawLong := false
	for _, f := range got {
		if strings.HasPrefix(f, longPrefix) {
			sawLong = true
			if n := len([]rune(f)); n != 200 {
				t.Fatalf("over-long fact = %d runes, want 200", n)
			}
		}
	}
	if !sawLong {
		t.Fatal("truncated over-long fact missing from results")
	}
	// No blank/whitespace fact survived.
	for _, f := range got {
		if strings.TrimSpace(f) == "" {
			t.Fatalf("blank fact survived parsing: %v", got)
		}
	}
}

// TestGoldenAutoCompactIDsSurviveInSummarizerInput pins invariant (3) for the auto path:
// load-bearing IDs sitting in the history reach the summarizer's conversation input, so
// the ID-preservation instruction has something concrete to act on.
func TestGoldenAutoCompactIDsSurviveInSummarizerInput(t *testing.T) {
	const (
		termID    = "term_abc123"
		runID     = "run_def456"
		watcherID = "watcher_ghi789"
		wkfID     = "wkf_jkl012"
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
	// captured[0] is the system instruction; captured[1:] is the flattened history. Scan
	// only the history so a match proves the IDs flowed through the conversation, not the
	// instruction's term_*/run_* pattern hints.
	var body strings.Builder
	for _, m := range captured[1:] {
		body.WriteString(m.StringContent)
		body.WriteByte('\n')
	}
	for _, id := range []string{termID, runID, watcherID, wkfID} {
		if !strings.Contains(body.String(), id) {
			t.Fatalf("summarizer input dropped ID %q:\n%s", id, body.String())
		}
	}
}

// TestGoldenManualCompactIDsSurviveInNote pins invariant (3) for the manual /compact
// path: the summary handed to Compact is stored verbatim in the persisted note, IDs
// intact (compactLocked must not truncate or mangle it).
func TestGoldenManualCompactIDsSurviveInNote(t *testing.T) {
	const (
		termID    = "term_xyz999"
		runID     = "run_uvw888"
		watcherID = "watcher_rst777"
		wkfID     = "wkf_opq666"
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
