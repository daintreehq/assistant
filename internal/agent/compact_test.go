package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// recordingStore captures persisted conversation rows so seq + marker writes can be
// asserted (the agent MessageStore seam — storage.Store satisfies it in prod).
type recordingStore struct {
	msgs []domain.ConversationMessageRecord
}

func (s *recordingStore) InsertMessage(rec domain.ConversationMessageRecord) (domain.ConversationMessageRecord, error) {
	s.msgs = append(s.msgs, rec)
	return rec, nil
}
func (s *recordingStore) InsertSkillSelection(rec domain.SkillSelectionLogRecord) (domain.SkillSelectionLogRecord, error) {
	return rec, nil
}

// compactSession builds a session with the real registry + a recording store.
func compactSession(t *testing.T, r Router) (*Session, *recordingStore) {
	t.Helper()
	store := &recordingStore{}
	reg, err := skills.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	deps := SessionDeps{
		Router:        r,
		Tools:         &fakeTools{},
		SkillSelector: fakeSelector{},
		SkillCatalog:  reg,
		Store:         store,
		SessionID:     "ses_compact",
		Events:        NoopEventSink{},
	}
	return NewSession(deps), store
}

func TestCompactKeepsControlsPlusSummary(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("first")
	s.InjectNote("second")
	s.InjectNote("third")
	if len(s.Messages()) <= 4 {
		t.Fatal("expected accumulated history")
	}
	s.Compact("goals: X. open: none. next: Y.")
	msgs := s.Messages()
	if len(msgs) != 4 {
		t.Fatalf("messages = %d want 4 (3 controls + 1 summary)", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("msg[0] role = %q", msgs[0].Role)
	}
	if !strings.Contains(msgs[1].StringContent, "# Runtime context") {
		t.Fatal("msg[1] should be runtime context")
	}
	if !strings.Contains(msgs[2].StringContent, "# Loaded skills") {
		t.Fatal("msg[2] should be loaded skills")
	}
	if msgs[3].Role != "user" || !strings.Contains(msgs[3].StringContent, "checkpoint") ||
		!strings.Contains(msgs[3].StringContent, "goals: X") {
		t.Fatalf("msg[3] = %+v want a checkpoint note", msgs[3])
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "first") {
			t.Fatal("old turns must be gone from context")
		}
	}
}

// TestCompactNoteEmbedsDepthTag proves each compaction tags its persisted note with
// the running depth, so a summary-of-summary chain is visible (issue #251): the first
// note carries "depth 1", a second compaction "depth 2".
func TestCompactNoteEmbedsDepthTag(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("history")
	s.Compact("goals: X.")
	first := s.Messages()[3]
	if first.Role != "user" || !strings.Contains(first.StringContent, "depth 1") ||
		!strings.Contains(first.StringContent, "checkpoint") {
		t.Fatalf("first compaction note = %q, want a depth-1 checkpoint", first.StringContent)
	}
	s.Compact("goals: Y.")
	second := s.Messages()[3]
	if !strings.Contains(second.StringContent, "depth 2") {
		t.Fatalf("second compaction note = %q, want depth 2", second.StringContent)
	}
}

func TestClearKeepsControlsNoSummary(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("alpha")
	s.InjectNote("beta")
	s.Clear()
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d want 3 (controls only, no summary)", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "alpha") || strings.Contains(m.StringContent, "[checkpoint") {
			t.Fatal("clear must drop turns and any checkpoint note")
		}
	}
}

func TestClearAppendsMarkerWithoutResettingSeq(t *testing.T) {
	s, store := compactSession(t, plainRouter())
	s.InjectNote("history-row")
	maxBefore := 0
	for _, r := range store.msgs {
		if r.Seq > maxBefore {
			maxBefore = r.Seq
		}
	}
	s.Clear()
	// The clear marker is appended ABOVE all prior rows (seq keeps climbing — a
	// reset would collide on the UNIQUE (sessionId, seq) index).
	var markerSeq = -1
	var historyKept bool
	for _, r := range store.msgs {
		if r.Content == domain.ClearMarker {
			markerSeq = r.Seq
		}
		if r.Content == injectNotePrefix+"history-row" {
			historyKept = true
		}
	}
	if markerSeq < 0 {
		t.Fatal("clear marker was not persisted")
	}
	if markerSeq <= maxBefore {
		t.Fatalf("marker seq %d should sit above prior max %d (no reset)", markerSeq, maxBefore)
	}
	if !historyKept {
		t.Fatal("clear is append-only — the history row must remain in the durable log")
	}
}

func TestClearIsIdempotent(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("once")
	s.Clear()
	s.Clear()
	if len(s.Messages()) != 3 {
		t.Fatalf("a second clear must keep exactly 3 control messages, got %d", len(s.Messages()))
	}
}

func TestClearDropsPriorCompactionSummary(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("pre")
	s.Compact("goals: X. open: none. next: Y.")
	if len(s.Messages()) != 4 {
		t.Fatalf("compact should leave 4 messages, got %d", len(s.Messages()))
	}
	s.Clear()
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("clear after compact should leave 3, got %d", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "[checkpoint") || strings.Contains(m.StringContent, "goals: X") {
			t.Fatal("clear must drop a prior compaction checkpoint")
		}
	}
}

// --- auto-compaction threshold ---

// chatCountRouter counts Chat (auto-compact summarizer) invocations and returns a
// fixed summary; Stream returns a plain answer.
type chatCountRouter struct {
	chatCalls int
	summary   string
}

func (r *chatCountRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *chatCountRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.chatCalls++
	return models.ChatResult{Content: r.summary}, nil
}
func (r *chatCountRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }
func (r *chatCountRouter) FlushMeter() []models.TierUsage   { return nil }

func TestAutoCompactsAboveThreshold(t *testing.T) {
	r := &chatCountRouter{summary: `{"goal":"AUTO_SUMMARY"}`}
	s, _ := compactSession(t, r)
	s.InjectNote("keep-small")
	// One huge note pushes the estimate past the 60k-token threshold (≈240k chars).
	s.InjectNote("GIANT_MARKER" + strings.Repeat("x", 260_000))
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.chatCalls != 1 {
		t.Fatalf("auto-compact summarizer called %d times, want 1", r.chatCalls)
	}
	msgs := s.Messages()
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "GIANT_MARKER") {
			t.Fatal("oversized history should have been summarized away")
		}
	}
	var replaced bool
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "checkpoint") && strings.Contains(m.StringContent, "AUTO_SUMMARY") {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("history should be replaced with the checkpoint note")
	}
}

// TestAutoCompactPreSweepAvoidsSummary proves the issue #257 lossless rung: when the
// pre-sweep alone drops the history back under the soft threshold, maybeAutoCompact must
// skip the small-model summary entirely (no Chat call) and leave the deduped refs behind.
func TestAutoCompactPreSweepAvoidsSummary(t *testing.T) {
	r := &chatCountRouter{summary: "SHOULD_NOT_BE_CALLED"}
	s, _ := compactSession(t, r)

	// Three byte-identical oversized tool results: ~360k chars ≈ 90k tokens together
	// (over the 60k gate), but a single retained copy (~30k tokens) plus the small
	// controls sits comfortably under it. No model call is needed to reach a valid
	// history, so the bare tool messages (no declaring assistant) are fine here.
	big := strings.Repeat("z", 120_000)
	s.messages = append(s.messages,
		models.ChatMessage{Role: "tool", ToolCallID: "call_a", StringContent: big},
		models.ChatMessage{Role: "tool", ToolCallID: "call_b", StringContent: big},
		models.ChatMessage{Role: "tool", ToolCallID: "call_c", StringContent: big},
	)

	s.maybeAutoCompact(context.Background(), "run_presweep")

	if r.chatCalls != 0 {
		t.Fatalf("pre-sweep should have averted the summary, but Chat was called %d times", r.chatCalls)
	}
	refs := 0
	var keptBig bool
	for _, m := range s.Messages() {
		switch {
		case strings.HasPrefix(m.StringContent, "[duplicate of "):
			refs++
		case m.StringContent == big:
			keptBig = true
		}
	}
	if refs != 2 {
		t.Fatalf("expected 2 duplicate refs after sweep, got %d", refs)
	}
	if !keptBig {
		t.Fatal("the most-recent copy of the duplicated result must be retained verbatim")
	}
}

// TestAutoCompactPreSweepStillSummarizesWhenOverThreshold proves the other branch of the
// #257 rung: when the sweep shrinks history but a SINGLE retained copy is itself still
// over the soft threshold, the summary rung must still fire (the sweep is a cheap first
// pass, not a replacement for compaction).
func TestAutoCompactPreSweepStillSummarizesWhenOverThreshold(t *testing.T) {
	r := &chatCountRouter{summary: "STILL_SUMMARIZED"}
	s, _ := compactSession(t, r)

	// A single retained copy (~280k chars ≈ 70k tokens) already exceeds the 60k gate, so
	// even after the two duplicates collapse to refs the summary must still run.
	big := strings.Repeat("z", 280_000)
	s.messages = append(s.messages,
		models.ChatMessage{Role: "tool", ToolCallID: "call_a", StringContent: big},
		models.ChatMessage{Role: "tool", ToolCallID: "call_b", StringContent: big},
	)

	s.maybeAutoCompact(context.Background(), "run_presweep_over")

	if r.chatCalls != 1 {
		t.Fatalf("sweep left history over threshold; summary should fire once, got %d Chat calls", r.chatCalls)
	}
}

// --- distill-on-compact ---

// fakeMemoryStore satisfies the agent.MemoryStore seam: it records inserts and
// answers MemoryExists from its seeded/inserted content set.
type fakeMemoryStore struct {
	existing  map[string]bool
	inserted  []domain.MemoryRecord
	insertErr error
}

func (f *fakeMemoryStore) MemoryExists(content string) (bool, error) {
	return f.existing[content], nil
}

func (f *fakeMemoryStore) InsertMemory(rec domain.MemoryRecord) (domain.MemoryRecord, error) {
	if f.insertErr != nil {
		return domain.MemoryRecord{}, f.insertErr
	}
	if f.existing == nil {
		f.existing = map[string]bool{}
	}
	if rec.ID == "" {
		rec.ID = "mem_fake"
	}
	f.inserted = append(f.inserted, rec)
	f.existing[rec.Content] = true
	return rec, nil
}

// jsonChatRouter returns a fixed Chat body (the distillation reply); Stream is a stub.
type jsonChatRouter struct {
	content   string
	chatCalls int
}

func (r *jsonChatRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *jsonChatRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.chatCalls++
	return models.ChatResult{Content: r.content}, nil
}
func (r *jsonChatRouter) ModelFor(domain.ModelTier) string { return "deepseek-v4-flash" }
func (r *jsonChatRouter) FlushMeter() []models.TierUsage   { return nil }

func TestDistillCompactSavesNovelFacts(t *testing.T) {
	r := &jsonChatRouter{content: `["fact A", "fact B"]`}
	s, _ := compactSession(t, r)
	mem := &fakeMemoryStore{existing: map[string]bool{"fact B": true}}
	s.deps.MemoryStore = mem
	saved := s.distillCompact(context.Background(), "run_test", "user: did X\nassistant: ok")
	if saved != 1 {
		t.Fatalf("saved=%d want 1 (fact B already exists, only fact A is novel)", saved)
	}
	if len(mem.inserted) != 1 || mem.inserted[0].Content != "fact A" {
		t.Fatalf("unexpected inserts: %+v", mem.inserted)
	}
	if mem.inserted[0].Source != domain.MemoryCompact {
		t.Fatalf("source = %q, want compact", mem.inserted[0].Source)
	}
	// Distilled facts are semantic and carry the turn's runId as provenance.
	if mem.inserted[0].Kind != domain.MemoryKindSemantic {
		t.Fatalf("kind = %q, want semantic", mem.inserted[0].Kind)
	}
	if mem.inserted[0].RunID == nil || *mem.inserted[0].RunID != "run_test" {
		t.Fatalf("runId = %v, want run_test", mem.inserted[0].RunID)
	}
}

func TestDistillCompactNilStoreSkipsModelCall(t *testing.T) {
	r := &jsonChatRouter{content: `["x"]`}
	s, _ := compactSession(t, r) // no MemoryStore wired
	if saved := s.distillCompact(context.Background(), "", "transcript"); saved != 0 {
		t.Fatalf("nil MemoryStore should save nothing, got %d", saved)
	}
	if r.chatCalls != 0 {
		t.Fatalf("nil MemoryStore must not call the model, got %d calls", r.chatCalls)
	}
}

func TestDistillCompactMalformedReply(t *testing.T) {
	r := &jsonChatRouter{content: "sorry, I cannot do that"}
	s, _ := compactSession(t, r)
	mem := &fakeMemoryStore{}
	s.deps.MemoryStore = mem
	if saved := s.distillCompact(context.Background(), "", "transcript"); saved != 0 {
		t.Fatalf("malformed reply should save nothing, got %d", saved)
	}
	if len(mem.inserted) != 0 {
		t.Fatalf("malformed reply must not insert, got %+v", mem.inserted)
	}
}

func TestDistillCompactEmptyTranscriptSkips(t *testing.T) {
	r := &jsonChatRouter{content: `["x"]`}
	s, _ := compactSession(t, r)
	s.deps.MemoryStore = &fakeMemoryStore{}
	if saved := s.distillCompact(context.Background(), "", "   "); saved != 0 {
		t.Fatalf("blank transcript should save nothing, got %d", saved)
	}
	if r.chatCalls != 0 {
		t.Fatalf("blank transcript must not call the model, got %d", r.chatCalls)
	}
}

// seqChatRouter returns Chat replies in sequence (summary, then distillation JSON).
// chatCalls is mutex-guarded: distill now runs on a background goroutine, so the
// summary call (turn goroutine) and the distill call (background goroutine) touch the
// counter concurrently and the race detector would otherwise flag it.
type seqChatRouter struct {
	mu        sync.Mutex
	replies   []string
	chatCalls int
}

func (r *seqChatRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *seqChatRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.mu.Lock()
	i := r.chatCalls
	r.chatCalls++
	r.mu.Unlock()
	if i < len(r.replies) {
		return models.ChatResult{Content: r.replies[i]}, nil
	}
	return models.ChatResult{Content: ""}, nil
}
func (r *seqChatRouter) ModelFor(domain.ModelTier) string { return "deepseek-v4-flash" }
func (r *seqChatRouter) FlushMeter() []models.TierUsage   { return nil }
func (r *seqChatRouter) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chatCalls
}

// TestAutoCompactDistillsBeforeDiscard is the integration guard: with a MemoryStore
// wired, an over-threshold auto-compact makes TWO model calls (summary + distill) and
// saves the distilled fact as source=compact. The distill pass now runs OFF the
// critical path (a detached goroutine), so the test joins it via DrainBackgroundWork
// before asserting — it still catches the distill pass being removed entirely.
func TestAutoCompactDistillsBeforeDiscard(t *testing.T) {
	r := &seqChatRouter{replies: []string{`{"goal":"AUTO_SUMMARY"}`, `["distilled durable fact"]`}}
	s, _ := compactSession(t, r)
	mem := &fakeMemoryStore{}
	s.deps.MemoryStore = mem
	s.InjectNote("keep-small")
	s.InjectNote("GIANT_MARKER" + strings.Repeat("x", 260_000))
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork() // distill is async now — join it before asserting effects
	if got := r.calls(); got != 2 {
		t.Fatalf("want 2 Chat calls (summary + distill), got %d", got)
	}
	if len(mem.inserted) != 1 || mem.inserted[0].Content != "distilled durable fact" {
		t.Fatalf("distilled memory not saved: %+v", mem.inserted)
	}
	if mem.inserted[0].Source != domain.MemoryCompact {
		t.Fatalf("source = %q, want compact", mem.inserted[0].Source)
	}
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "GIANT_MARKER") {
			t.Fatal("oversized history should have been compacted away")
		}
	}
}

func TestDoesNotAutoCompactSmallConversation(t *testing.T) {
	r := &chatCountRouter{summary: "X"}
	s, _ := compactSession(t, r)
	s.InjectNote("small note one")
	s.InjectNote("small note two")
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.chatCalls != 0 {
		t.Fatalf("small conversation must NOT auto-compact (chat called %d)", r.chatCalls)
	}
	var kept bool
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "small note one") {
			kept = true
		}
	}
	if !kept {
		t.Fatal("small conversation history must survive untouched")
	}
}

// --- distill runs off the critical path (issue #202) ---

// blockingDistillRouter answers the FIRST Chat (summary) immediately, then blocks the
// SECOND Chat (the background distill) until release is closed — so a test can prove
// the turn returns without waiting on distill. started fires when the distill call
// begins. counter is mutex-guarded (two goroutines).
type blockingDistillRouter struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (r *blockingDistillRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *blockingDistillRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.mu.Unlock()
	if n == 1 {
		return models.ChatResult{Content: `{"goal":"AUTO_SUMMARY"}`}, nil
	}
	// Distill call: announce, then block until the test releases us.
	r.started <- struct{}{}
	<-r.release
	return models.ChatResult{Content: `["distilled fact"]`}, nil
}
func (r *blockingDistillRouter) ModelFor(domain.ModelTier) string { return "deepseek-v4-flash" }
func (r *blockingDistillRouter) FlushMeter() []models.TierUsage   { return nil }

// TestAutoCompactDistillRunsOffCriticalPath proves the fix for the "Analyzing" stall:
// the turn compacts and returns BEFORE the (now background) distill round-trip
// finishes. With distill blocked, Send must still complete and the oversized history
// must already be gone; only after releasing distill does the fact land in memory.
func TestAutoCompactDistillRunsOffCriticalPath(t *testing.T) {
	r := &blockingDistillRouter{started: make(chan struct{}, 1), release: make(chan struct{})}
	s, _ := compactSession(t, r)
	mem := &fakeMemoryStore{}
	s.deps.MemoryStore = mem
	s.InjectNote("keep-small")
	s.InjectNote("GIANT_MARKER" + strings.Repeat("x", 260_000))

	done := make(chan struct{})
	go func() {
		_, _ = s.Send(context.Background(), "hi", SendOptions{})
		close(done)
	}()

	// Distill has begun (and is now blocked) — the summary+compaction already ran.
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("distill never started — auto-compact did not run")
	}
	// The turn must return even though distill is still blocked.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked on the background distill — critical path still stalls")
	}
	// History was compacted on the turn path, before distill completed.
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "GIANT_MARKER") {
			t.Fatal("history should have been compacted before the turn returned")
		}
	}
	// Release distill and join it; the fact lands only after it completes.
	close(r.release)
	s.DrainBackgroundWork()
	if len(mem.inserted) != 1 || mem.inserted[0].Content != "distilled fact" {
		t.Fatalf("distilled fact not saved after drain: %+v", mem.inserted)
	}
}

// --- bounded-growth fallback on sustained small-model outage (issue #202) ---

// errChatRouter fails every Chat (summary) call, simulating a small-model outage;
// Stream still answers so the turn itself can proceed. Counter is mutex-guarded for
// symmetry, though the failure path spawns no background goroutine.
type errChatRouter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (r *errChatRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *errChatRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return models.ChatResult{}, r.err
}
func (r *errChatRouter) ModelFor(domain.ModelTier) string { return "deepseek-v4-flash" }
func (r *errChatRouter) FlushMeter() []models.TierUsage   { return nil }

// flakyRouter fails the first failFirst Chat calls, then returns summary. Used to
// prove the consecutive-failure counter resets on a successful compaction.
type flakyRouter struct {
	mu        sync.Mutex
	calls     int
	failFirst int
	summary   string
}

func (r *flakyRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *flakyRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	r.mu.Unlock()
	if n <= r.failFirst {
		return models.ChatResult{}, errors.New("small model down")
	}
	return models.ChatResult{Content: r.summary}, nil
}
func (r *flakyRouter) ModelFor(domain.ModelTier) string { return "deepseek-v4-flash" }
func (r *flakyRouter) FlushMeter() []models.TierUsage   { return nil }

// overHardThresholdNote is a single note large enough to exceed
// AutoCompactHardTruncationThreshold (≈200k tokens ⇒ ≈800k chars).
func overHardThresholdNote() string {
	return strings.Repeat("x", domain.AutoCompactHardTruncationThreshold*domain.CharsPerToken+50_000)
}

// TestAutoCompactFallbackTruncatesAfterSustainedOutage proves history stays bounded
// when the summary model is down: the first AutoCompactFailureThreshold-1 failures
// leave history intact (no premature data loss), and the threshold'th failure — with
// history over the hard ceiling — lossy-truncates it back under bound without a model.
func TestAutoCompactFallbackTruncatesAfterSustainedOutage(t *testing.T) {
	r := &errChatRouter{err: errors.New("provider down")}
	s, _ := compactSession(t, r)
	// Two notes: a single note gives len==ControlMessageCount+1, which trips the
	// "no real history" guard and skips compaction entirely.
	s.InjectNote("keep-small")
	s.InjectNote(overHardThresholdNote())
	ctx := context.Background()
	initialLen := len(s.Messages())

	// Failures below the threshold must NOT mutate history.
	for i := 0; i < domain.AutoCompactFailureThreshold-1; i++ {
		s.maybeAutoCompact(ctx, "run_test")
		if got := len(s.Messages()); got != initialLen {
			t.Fatalf("failure %d truncated history too early: %d messages (want %d)", i+1, got, initialLen)
		}
	}
	// The threshold'th failure crosses the line → lossy truncation.
	s.maybeAutoCompact(ctx, "run_test")
	if got := s.estimateTokens(); got >= domain.AutoCompactHardTruncationThreshold {
		t.Fatalf("history still over hard ceiling after fallback: %d tokens", got)
	}
	for _, m := range s.Messages() {
		if len(m.StringContent) > 100_000 {
			t.Fatal("oversized note should have been truncated away")
		}
	}
	if r.calls != domain.AutoCompactFailureThreshold {
		t.Fatalf("summary attempted %d times, want %d", r.calls, domain.AutoCompactFailureThreshold)
	}
}

// TestAutoCompactFailureCounterResetsOnSuccess proves one transient outage doesn't
// permanently disarm the soft path: after two failures, a successful summary compacts
// and resets the consecutive-failure counter to zero.
func TestAutoCompactFailureCounterResetsOnSuccess(t *testing.T) {
	r := &flakyRouter{failFirst: 2, summary: `{"goal":"RECOVERED_SUMMARY"}`}
	s, _ := compactSession(t, r) // no MemoryStore ⇒ success path spawns no distill goroutine
	// Two notes (a lone note trips the "no real history" guard). Over the SOFT
	// threshold but under the hard ceiling, so failures climb the counter without
	// ever truncating.
	s.InjectNote("keep-small")
	s.InjectNote(strings.Repeat("x", (domain.AutoCompactTokenThreshold+20_000)*domain.CharsPerToken))
	ctx := context.Background()

	s.maybeAutoCompact(ctx, "run_test")
	s.maybeAutoCompact(ctx, "run_test")
	if s.compactFailures != 2 {
		t.Fatalf("compactFailures = %d after two outages, want 2", s.compactFailures)
	}
	s.maybeAutoCompact(ctx, "run_test") // succeeds → compacts + resets
	if s.compactFailures != 0 {
		t.Fatalf("compactFailures = %d after a successful compaction, want 0 (reset)", s.compactFailures)
	}
	var replaced bool
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "RECOVERED_SUMMARY") {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("recovered summary should have replaced the history")
	}
}

// TestAutoCompactFailureIgnoresUserCancel proves a turn cancelled mid-summary is not
// counted as a provider outage (and never mutates history while the turn aborts).
func TestAutoCompactFailureIgnoresUserCancel(t *testing.T) {
	r := &errChatRouter{err: errors.New("cancelled-ish")}
	s, _ := compactSession(t, r)
	s.InjectNote("keep-small")
	s.InjectNote(overHardThresholdNote())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	initialLen := len(s.Messages())

	s.maybeAutoCompact(ctx, "run_test")
	if s.compactFailures != 0 {
		t.Fatalf("a user cancel must not count as a compact failure, got %d", s.compactFailures)
	}
	if got := len(s.Messages()); got != initialLen {
		t.Fatalf("a cancelled compact must not mutate history, got %d messages (want %d)", got, initialLen)
	}
}

// --- truncateLocked direct unit tests ---

// TestTruncateLockedKeepsRecentTail proves the lossy fallback keeps the 3 controls
// plus at most keepN of the most-recent working messages, dropping the oldest.
func TestTruncateLockedKeepsRecentTail(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	const total = 30
	for i := 0; i < total; i++ {
		s.InjectNote(fmt.Sprintf("UNIQ_%03d", i))
	}
	keepN := domain.AutoCompactHardTruncationKeepMessages
	s.mu.Lock()
	s.truncateLocked(keepN)
	s.mu.Unlock()

	msgs := s.Messages()
	if len(msgs) != domain.ControlMessageCount+keepN {
		t.Fatalf("kept %d messages, want %d (controls + keepN)", len(msgs), domain.ControlMessageCount+keepN)
	}
	// Newest survives; oldest is gone.
	if !strings.Contains(msgs[len(msgs)-1].StringContent, fmt.Sprintf("UNIQ_%03d", total-1)) {
		t.Fatalf("most-recent message should survive, got %q", msgs[len(msgs)-1].StringContent)
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "UNIQ_000") {
			t.Fatal("oldest message should have been dropped")
		}
	}
}

// TestTruncateLockedShedsOversizedTail proves the iterative head-shed: even within
// keepN, a single message bigger than the hard ceiling is dropped so the bound holds.
func TestTruncateLockedShedsOversizedTail(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote(overHardThresholdNote()) // one message, itself over the ceiling
	s.mu.Lock()
	s.truncateLocked(domain.AutoCompactHardTruncationKeepMessages)
	s.mu.Unlock()
	if got := s.estimateTokens(); got >= domain.AutoCompactHardTruncationThreshold {
		t.Fatalf("history still over ceiling after shed: %d tokens", got)
	}
	if got := len(s.Messages()); got != domain.ControlMessageCount {
		t.Fatalf("an over-ceiling lone note should shed to controls only, got %d messages", got)
	}
}

// TestTruncateLockedRehydratesFromMarker proves the truncation persists a compaction
// marker + the retained tail, so a resume rebuilds exactly the kept tail (not the
// dropped head).
func TestTruncateLockedRehydratesFromMarker(t *testing.T) {
	s, store := compactSession(t, plainRouter())
	const total = 20
	for i := 0; i < total; i++ {
		s.InjectNote(fmt.Sprintf("UNIQ_%03d", i))
	}
	keepN := domain.AutoCompactHardTruncationKeepMessages
	s.mu.Lock()
	s.truncateLocked(keepN)
	s.mu.Unlock()

	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("expected a resume from the truncation marker")
	}
	if len(res.RestoredMessages) != keepN {
		t.Fatalf("rehydrated %d working messages, want %d", len(res.RestoredMessages), keepN)
	}
	// 20 notes, keep last keepN ⇒ first restored is index total-keepN.
	if !strings.Contains(res.RestoredMessages[0].StringContent, fmt.Sprintf("UNIQ_%03d", total-keepN)) {
		t.Fatalf("first restored = %q, want UNIQ_%03d", res.RestoredMessages[0].StringContent, total-keepN)
	}
	for _, m := range res.RestoredMessages {
		if strings.Contains(m.StringContent, "UNIQ_000") {
			t.Fatal("dropped head leaked into the rehydrated history")
		}
	}
}

// --- review fixes ---

// TestStartDistillSkippedWhileDraining proves the Add-after-Wait gate: once
// DrainBackgroundWork has set draining, a subsequent startDistill spawns nothing — so
// no wg.Add can race wg.Wait at counter zero and no distill touches a closing
// Router/Store during App.Shutdown.
func TestStartDistillSkippedWhileDraining(t *testing.T) {
	r := &jsonChatRouter{content: `["x"]`}
	s, _ := compactSession(t, r)
	s.deps.MemoryStore = &fakeMemoryStore{}

	s.DrainBackgroundWork() // nothing in flight → returns immediately, sets draining
	s.startDistill("run_test", "user: did X\nassistant: ok")
	s.DrainBackgroundWork() // joins anything that (incorrectly) spawned

	if r.chatCalls != 0 {
		t.Fatalf("distill must be skipped once draining, got %d Chat calls", r.chatCalls)
	}
}

// TestAutoCompactFailureCounterResetByManualCompact proves a manual /compact clears the
// auto-compact failure streak — otherwise a stale count from a prior outage could trip
// the lossy fallback right after the user manually compacted.
func TestAutoCompactFailureCounterResetByManualCompact(t *testing.T) {
	r := &errChatRouter{err: errors.New("provider down")}
	s, _ := compactSession(t, r)
	s.InjectNote("keep-small")
	s.InjectNote(strings.Repeat("x", (domain.AutoCompactTokenThreshold+20_000)*domain.CharsPerToken))
	ctx := context.Background()

	s.maybeAutoCompact(ctx, "run_test")
	s.maybeAutoCompact(ctx, "run_test")
	if s.compactFailures != 2 {
		t.Fatalf("compactFailures = %d after two outages, want 2", s.compactFailures)
	}
	if err := s.Compact("manual summary"); err != nil {
		t.Fatal(err)
	}
	if s.compactFailures != 0 {
		t.Fatalf("manual compact must reset the failure streak, got %d", s.compactFailures)
	}
}

// TestAutoCompactSingleHugeMessageTruncates proves the early-return guard no longer
// strands a single working message that is itself over the hard ceiling: with a failing
// summarizer, the bounded-growth fallback still fires and shrinks it under the ceiling.
func TestAutoCompactSingleHugeMessageTruncates(t *testing.T) {
	r := &errChatRouter{err: errors.New("provider down")}
	s, _ := compactSession(t, r)
	s.InjectNote(overHardThresholdNote()) // ONE working message, over the hard ceiling
	ctx := context.Background()

	for i := 0; i < domain.AutoCompactFailureThreshold; i++ {
		s.maybeAutoCompact(ctx, "run_test")
	}
	if got := s.estimateTokens(); got >= domain.AutoCompactHardTruncationThreshold {
		t.Fatalf("a single over-ceiling message must still be truncated, got %d tokens", got)
	}
	if r.calls != domain.AutoCompactFailureThreshold {
		t.Fatalf("summary attempted %d times, want %d (guard must not skip an over-ceiling lone message)", r.calls, domain.AutoCompactFailureThreshold)
	}
}

// --- real-prompt-token gating (issue #246) ---

// TestMaybeAutoCompactPrefersRealPromptTokens proves the gate fires on the REAL
// provider-reported prompt_tokens even when the char estimate is tiny: a small
// history (well under the soft threshold by chars) compacts once lastPromptTokens —
// which counts the tool schemas the estimate is blind to — crosses the threshold.
func TestMaybeAutoCompactPrefersRealPromptTokens(t *testing.T) {
	r := &chatCountRouter{summary: `{"goal":"REAL_TOKEN_SUMMARY"}`}
	s, _ := compactSession(t, r)
	// Two small notes: char estimate stays far under the soft threshold, and len is
	// above ControlMessageCount+1 so the "no real history" guard doesn't skip.
	s.InjectNote("keep-small-a")
	s.InjectNote("keep-small-b")
	ctx := context.Background()

	// With no real figure yet, the char estimate governs — far under threshold, so no
	// compaction. This is the behaviour the issue is fixing: the estimate alone misses.
	s.maybeAutoCompact(ctx, "run_test")
	if r.chatCalls != 0 {
		t.Fatalf("char estimate is under threshold; compaction should not fire yet, got %d calls", r.chatCalls)
	}

	// Stash a real prompt_tokens figure over the soft threshold (what Fireworks would
	// report once the ~68 tool schemas are counted) and re-check.
	s.mu.Lock()
	s.lastPromptTokens = domain.AutoCompactTokenThreshold + 10_000
	s.mu.Unlock()

	s.maybeAutoCompact(ctx, "run_test")
	if r.chatCalls != 1 {
		t.Fatalf("real prompt_tokens over threshold should trigger compaction, got %d calls", r.chatCalls)
	}
	var replaced bool
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "REAL_TOKEN_SUMMARY") {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("history should be replaced with the checkpoint note")
	}
}

// TestMaybeAutoCompactFallsBackToEstimateWhenNoRealTokens proves the char estimate is
// still the gate before any provider figure has landed (lastPromptTokens == 0): a
// history large by chars compacts even though no real token count exists yet.
func TestMaybeAutoCompactFallsBackToEstimateWhenNoRealTokens(t *testing.T) {
	r := &chatCountRouter{summary: "ESTIMATE_SUMMARY"}
	s, _ := compactSession(t, r)
	s.InjectNote("keep-small")
	// One huge note pushes the CHAR estimate past the soft threshold (≈240k chars).
	s.InjectNote("GIANT_MARKER" + strings.Repeat("x", 260_000))

	s.mu.Lock()
	stash := s.lastPromptTokens
	s.mu.Unlock()
	if stash != 0 {
		t.Fatalf("precondition: lastPromptTokens should be 0, got %d", stash)
	}

	s.maybeAutoCompact(context.Background(), "run_test")
	if r.chatCalls != 1 {
		t.Fatalf("char estimate over threshold should trigger compaction via fallback, got %d calls", r.chatCalls)
	}
}

// reportingRouter streams a plain answer, reports a fixed LARGE-tier prompt_tokens via
// FlushMeter, and returns a summary from Chat — so an end-to-end Send populates
// lastPromptTokens from real usage and the NEXT Send can gate compaction on it even
// when the char estimate is tiny.
type reportingRouter struct {
	largePromptTokens int
	summary           string
	chatCalls         int
}

func (r *reportingRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	if onToken != nil {
		onToken("hi")
	}
	return models.ChatResult{Content: "hi"}, nil
}
func (r *reportingRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.chatCalls++
	return models.ChatResult{Content: r.summary}, nil
}
func (r *reportingRouter) ModelFor(domain.ModelTier) string { return "glm-5p2" }
func (r *reportingRouter) FlushMeter() []models.TierUsage {
	return []models.TierUsage{{
		Tier:         string(domain.ModelLarge),
		Model:        "glm-5p2",
		PromptTokens: r.largePromptTokens,
		TotalTokens:  r.largePromptTokens,
	}}
}

// TestSendStashedTokensGateNextRoundCompaction is the end-to-end proof: round 1's
// emitUsage stashes the provider-reported prompt_tokens (over threshold), and round 2's
// pre-turn auto-compact gates on that stashed figure — compacting BEFORE streaming even
// though the char history is tiny. This closes the gap that the seeded-field unit tests
// leave open (they don't flow through emitUsage → next-round maybeAutoCompact).
func TestSendStashedTokensGateNextRoundCompaction(t *testing.T) {
	r := &reportingRouter{largePromptTokens: domain.AutoCompactTokenThreshold + 10_000, summary: "GATED_SUMMARY"}
	s, _ := compactSession(t, r)
	ctx := context.Background()

	// Round 1: tiny char history, but the provider reports a prompt over the threshold
	// (tool schemas included). The pre-turn check saw lastPromptTokens == 0, so no
	// compaction fires this round — but emitUsage stashes the real figure.
	if _, err := s.Send(ctx, "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.chatCalls != 0 {
		t.Fatalf("first round must not compact (estimate tiny, no stash yet), got %d summary calls", r.chatCalls)
	}
	s.mu.Lock()
	stashed := s.lastPromptTokens
	s.mu.Unlock()
	if stashed != domain.AutoCompactTokenThreshold+10_000 {
		t.Fatalf("emitUsage should stash the real large-tier figure, got %d", stashed)
	}

	// Round 2: the pre-turn auto-compact reads the stashed real figure (> threshold) and
	// compacts before streaming, even though the char estimate is still tiny.
	if _, err := s.Send(ctx, "again", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.chatCalls != 1 {
		t.Fatalf("second round should compact on the stashed real figure, got %d summary calls", r.chatCalls)
	}
}

// TestCompactLockedZerosLastPromptTokens proves compaction clears the stashed real
// figure — otherwise the pre-compaction ~60K+ value would make the next check see the
// threshold exceeded and re-compact the freshly-shrunk history immediately.
func TestCompactLockedZerosLastPromptTokens(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("history")
	s.mu.Lock()
	s.lastPromptTokens = domain.AutoCompactTokenThreshold + 5_000
	s.compactLocked("summary")
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 0 {
		t.Fatalf("compactLocked must zero lastPromptTokens, got %d", got)
	}
}

// TestClearLockedZerosLastPromptTokens proves a clear drops the stashed real figure
// along with the history it described.
func TestClearLockedZerosLastPromptTokens(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("history")
	s.mu.Lock()
	s.lastPromptTokens = domain.AutoCompactTokenThreshold + 5_000
	s.clearLocked()
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 0 {
		t.Fatalf("clearLocked must zero lastPromptTokens, got %d", got)
	}
}

// TestTruncateLockedZerosLastPromptTokens proves the lossy fallback drops the stashed
// real figure so the next check measures the shrunk tail, not the stale over-ceiling
// figure.
func TestTruncateLockedZerosLastPromptTokens(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	for i := 0; i < 10; i++ {
		s.InjectNote(fmt.Sprintf("note-%d", i))
	}
	s.mu.Lock()
	s.lastPromptTokens = domain.AutoCompactHardTruncationThreshold + 5_000
	s.truncateLocked(domain.AutoCompactHardTruncationKeepMessages)
	got := s.lastPromptTokens
	s.mu.Unlock()
	if got != 0 {
		t.Fatalf("truncateLocked must zero lastPromptTokens, got %d", got)
	}
}

// --- verbatim recent tail on the healthy compaction path (issue #252) ---

// TestKeepValidTailHelper unit-tests the shared recency-tail selector directly: the
// non-positive guards, the keepN cap, the no-alias copy, orphan-result + incomplete-call
// cleaning, the token-budget head-shed, and a valid tool pair surviving intact.
func TestKeepValidTailHelper(t *testing.T) {
	mk := models.TextMessage

	// Non-positive guards return nil.
	if got := keepValidTail(nil, 5, 1000); got != nil {
		t.Fatalf("empty input should return nil, got %v", got)
	}
	if got := keepValidTail([]models.ChatMessage{mk("user", "a")}, 0, 1000); got != nil {
		t.Fatalf("keepN=0 should return nil, got %v", got)
	}
	if got := keepValidTail([]models.ChatMessage{mk("user", "a")}, 5, 0); got != nil {
		t.Fatalf("budget=0 should return nil, got %v", got)
	}

	// Caps to the last keepN.
	in := []models.ChatMessage{mk("user", "a"), mk("user", "b"), mk("user", "c")}
	got := keepValidTail(in, 2, 1000)
	if len(got) != 2 || got[0].StringContent != "b" || got[1].StringContent != "c" {
		t.Fatalf("cap to last 2 failed: %+v", got)
	}
	// The result is a copy — mutating it must not touch the input backing array.
	got[0].StringContent = "MUTATED"
	if in[1].StringContent != "b" {
		t.Fatal("keepValidTail must copy — mutating the result changed the input")
	}

	// An orphan tool result (id never declared by a preceding assistant) is dropped.
	orphan := []models.ChatMessage{
		mk("user", "hi"),
		{Role: "tool", ToolCallID: "call_x", StringContent: "orphan result"},
	}
	for _, m := range keepValidTail(orphan, 16, 1000) {
		if m.Role == "tool" {
			t.Fatalf("orphan tool result should be dropped: %+v", orphan)
		}
	}

	// An incomplete trailing tool call (declared, never answered) is cut.
	incomplete := []models.ChatMessage{
		mk("user", "hi"),
		{Role: "assistant", ToolCalls: []models.ToolCallRequest{{
			ID: "call_y", Type: "function",
			Function: models.ToolCallFunction{Name: "f", Arguments: "{}"},
		}}},
	}
	for _, m := range keepValidTail(incomplete, 16, 1000) {
		if len(m.ToolCalls) > 0 {
			t.Fatalf("incomplete trailing tool call should be cut: %+v", incomplete)
		}
	}

	// A single message larger than the budget sheds to empty.
	big := []models.ChatMessage{mk("user", strings.Repeat("x", 8000))} // ~2000 tokens
	if got := keepValidTail(big, 16, 1000); len(got) != 0 {
		t.Fatalf("oversized lone message should shed to empty, got %d", len(got))
	}

	// A valid tool pair survives intact (the result keeps its declared parent).
	pair := []models.ChatMessage{
		{Role: "assistant", ToolCalls: []models.ToolCallRequest{{
			ID: "call_z", Type: "function",
			Function: models.ToolCallFunction{Name: "f", Arguments: "{}"},
		}}},
		{Role: "tool", ToolCallID: "call_z", StringContent: "ok"},
	}
	if got := keepValidTail(pair, 16, 1000); len(got) != 2 {
		t.Fatalf("valid tool pair should survive, got %d: %+v", len(got), got)
	}
}

// TestAutoCompactKeepsVerbatimTail proves the healthy path now rebuilds to controls +
// summary note + the most-recent working messages verbatim (an oversized old note is shed
// by the tail budget), instead of collapsing to summary-only.
func TestAutoCompactKeepsVerbatimTail(t *testing.T) {
	r := &chatCountRouter{summary: `{"goal":"TAIL_SUMMARY"}`}
	s, _ := compactSession(t, r)
	// One big OLD note trips the soft threshold (shed by the tail budget); the recent
	// small notes are the verbatim tail to keep.
	s.InjectNote("OLD_BIG" + strings.Repeat("x", 260_000))
	s.InjectNote("RECENT_A")
	s.InjectNote("RECENT_B")
	s.InjectNote("RECENT_C")

	s.maybeAutoCompact(context.Background(), "run_test")
	if r.chatCalls != 1 {
		t.Fatalf("expected one summary call, got %d", r.chatCalls)
	}
	msgs := s.Messages()
	if len(msgs) != domain.ControlMessageCount+1+3 {
		t.Fatalf("messages = %d, want %d (controls + summary + 3 tail)", len(msgs), domain.ControlMessageCount+1+3)
	}
	// Controls remain byte-stable.
	if msgs[0].Role != "system" || !strings.Contains(msgs[1].StringContent, "# Runtime context") ||
		!strings.Contains(msgs[2].StringContent, "# Loaded skills") {
		t.Fatal("control messages must remain byte-stable")
	}
	// Checkpoint note sits immediately after the controls.
	if msgs[3].Role != "user" || !strings.Contains(msgs[3].StringContent, "checkpoint") ||
		!strings.Contains(msgs[3].StringContent, "TAIL_SUMMARY") {
		t.Fatalf("msg[3] = %q, want the checkpoint note", msgs[3].StringContent)
	}
	// The recent tail follows the summary, verbatim and in original order.
	if !strings.Contains(msgs[4].StringContent, "RECENT_A") ||
		!strings.Contains(msgs[5].StringContent, "RECENT_B") ||
		!strings.Contains(msgs[6].StringContent, "RECENT_C") {
		t.Fatalf("recent tail not preserved verbatim: %+v", msgs[4:])
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "OLD_BIG") {
			t.Fatal("the oversized old note should have been shed from the tail")
		}
	}
}

// TestAutoCompactTailCleanedOrphans proves the verbatim tail is a VALID model history:
// an orphan tool result is dropped while a complete tool pair survives, on the healthy
// path (the oversized old note is shed by the budget).
func TestAutoCompactTailCleanedOrphans(t *testing.T) {
	r := &chatCountRouter{summary: "ORPHAN_SUMMARY"}
	s, _ := compactSession(t, r)
	s.mu.Lock()
	s.messages = append(s.messages,
		// A big OLD note to trip the soft threshold (shed by the tail budget).
		models.TextMessage("user", "BIG_OLD"+strings.Repeat("x", 260_000)),
		// A valid recent tool pair — must survive intact.
		models.ChatMessage{Role: "assistant", ToolCalls: []models.ToolCallRequest{{
			ID: "call_1", Type: "function",
			Function: models.ToolCallFunction{Name: "terminal.list", Arguments: "{}"},
		}}},
		models.ChatMessage{Role: "tool", ToolCallID: "call_1", StringContent: "VALID_RESULT"},
		// An orphan tool result whose id was never declared — must be dropped.
		models.ChatMessage{Role: "tool", ToolCallID: "call_orphan", StringContent: "ORPHAN_RESULT"},
	)
	s.mu.Unlock()

	s.maybeAutoCompact(context.Background(), "run_test")
	if r.chatCalls != 1 {
		t.Fatalf("expected one summary call, got %d", r.chatCalls)
	}
	msgs := s.Messages()
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "ORPHAN_RESULT") {
			t.Fatal("orphan tool result must be dropped from the tail")
		}
		if strings.Contains(m.StringContent, "BIG_OLD") {
			t.Fatal("the oversized old note should have been shed")
		}
	}
	var sawCall, sawResult bool
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_1" {
				sawCall = true
			}
		}
		if m.Role == "tool" && m.ToolCallID == "call_1" && strings.Contains(m.StringContent, "VALID_RESULT") {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("valid tool pair should survive in the tail (call=%v result=%v): %+v", sawCall, sawResult, msgs)
	}
}

// TestAutoCompactTailTokenBudgetCaps proves a tail larger than the budget is shed from the
// head (oldest first) until it fits, even when it is within keepN.
func TestAutoCompactTailTokenBudgetCaps(t *testing.T) {
	r := &chatCountRouter{summary: "BUDGET_SUMMARY"}
	s, _ := compactSession(t, r)
	const noteChars = 8000 // ~2000 tokens each
	const total = 40       // ~80k tokens total → trips the 60k soft threshold
	for i := 0; i < total; i++ {
		s.InjectNote(fmt.Sprintf("NOTE_%03d", i) + strings.Repeat("y", noteChars))
	}
	s.maybeAutoCompact(context.Background(), "run_test")
	if r.chatCalls != 1 {
		t.Fatalf("expected one summary call, got %d", r.chatCalls)
	}
	tail := s.Messages()[domain.ControlMessageCount+1:] // after controls + summary note
	if len(tail) == 0 || len(tail) >= domain.AutoCompactVerbatimTailMessages {
		t.Fatalf("tail should be shed below keepN by the budget, got %d", len(tail))
	}
	if est := estimateMessagesTokens(tail); est >= domain.AutoCompactVerbatimTailTokenBudget {
		t.Fatalf("retained tail %d tokens exceeds budget %d", est, domain.AutoCompactVerbatimTailTokenBudget)
	}
	if !strings.Contains(tail[len(tail)-1].StringContent, fmt.Sprintf("NOTE_%03d", total-1)) {
		t.Fatalf("most-recent note should survive, got %q", tail[len(tail)-1].StringContent)
	}
	for _, m := range tail {
		if strings.Contains(m.StringContent, "NOTE_000") {
			t.Fatal("oldest note should have been shed by the budget")
		}
	}
}

// TestCompactManualKeepsNoTail is the regression guard for the shared compactLocked: the
// manual /compact path must still collapse to controls + summary only, NOT keep a verbatim
// tail (which is healthy-auto-path-only behaviour).
func TestCompactManualKeepsNoTail(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	// Recent small notes the AUTO path would keep as a verbatim tail.
	s.InjectNote("MANUAL_RECENT_A")
	s.InjectNote("MANUAL_RECENT_B")
	if err := s.Compact("manual summary"); err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if len(msgs) != domain.ControlMessageCount+1 {
		t.Fatalf("manual compact must produce controls + summary only, got %d", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "MANUAL_RECENT_") {
			t.Fatal("manual /compact must NOT keep a verbatim tail")
		}
	}
}

// TestAutoCompactRehydratesSummaryPlusTail proves the persisted layout (marker → summary
// note → verbatim tail) rehydrates to exactly the summary note + tail, with the shed old
// note gone.
func TestAutoCompactRehydratesSummaryPlusTail(t *testing.T) {
	r := &chatCountRouter{summary: `{"goal":"REHYDRATE_SUMMARY"}`}
	s, store := compactSession(t, r)
	s.InjectNote("OLD_BIG" + strings.Repeat("x", 260_000))
	s.InjectNote("TAIL_KEEP_A")
	s.InjectNote("TAIL_KEEP_B")
	s.maybeAutoCompact(context.Background(), "run_test")

	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("expected a resume from the compaction marker")
	}
	if len(res.RestoredMessages) != 3 {
		t.Fatalf("rehydrated %d working messages, want 3 (summary + 2 tail)", len(res.RestoredMessages))
	}
	if !strings.Contains(res.RestoredMessages[0].StringContent, "REHYDRATE_SUMMARY") {
		t.Fatalf("first restored should be the summary note, got %q", res.RestoredMessages[0].StringContent)
	}
	if !strings.Contains(res.RestoredMessages[1].StringContent, "TAIL_KEEP_A") ||
		!strings.Contains(res.RestoredMessages[2].StringContent, "TAIL_KEEP_B") {
		t.Fatalf("verbatim tail not rehydrated: %+v", res.RestoredMessages)
	}
	for _, m := range res.RestoredMessages {
		if strings.Contains(m.StringContent, "OLD_BIG") {
			t.Fatal("shed old note must not rehydrate")
		}
	}
}

// promptCaptureRouter records the system message of each Chat (summary) call so a test can
// assert the auto-summary prompt instructs the model to preserve load-bearing IDs.
type promptCaptureRouter struct {
	mu        sync.Mutex
	systemMsg string
	summary   string
}

func (r *promptCaptureRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *promptCaptureRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.mu.Lock()
	if len(opts.Messages) > 0 {
		r.systemMsg = opts.Messages[0].StringContent
	}
	r.mu.Unlock()
	return models.ChatResult{Content: r.summary}, nil
}
func (r *promptCaptureRouter) ModelFor(domain.ModelTier) string { return "deepseek-v4-flash" }
func (r *promptCaptureRouter) FlushMeter() []models.TierUsage   { return nil }
func (r *promptCaptureRouter) system() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.systemMsg
}

// TestAutoCompactSummaryPromptPreservesIDs proves the auto-compaction checkpoint system
// prompt tells the model to keep every load-bearing identifier verbatim (using the
// canonical domain prefixes — issue #256), so a mid-run compaction doesn't strand the
// orchestrator's live references.
func TestAutoCompactSummaryPromptPreservesIDs(t *testing.T) {
	r := &promptCaptureRouter{summary: "S"}
	s, _ := compactSession(t, r)
	s.InjectNote("keep-small")
	s.InjectNote("GIANT" + strings.Repeat("x", 260_000))
	s.maybeAutoCompact(context.Background(), "run_test")

	got := r.system()
	for _, want := range []string{"term_*", "wch_*", "wfr_*", "agt_*", "tmr_*", "grt_*", "branch", "grant"} {
		if !strings.Contains(got, want) {
			t.Fatalf("checkpoint prompt missing %q: %q", want, got)
		}
	}
}

// TestKeepValidTailLargeBatchSplitBoundary is the regression guard for the keepN cut
// landing mid tool-batch: a single assistant round with keepN tool calls (1 assistant +
// keepN results) must NOT collapse to an empty tail — the window start backs up over the
// leading results to include the declaring assistant, keeping the whole valid round.
func TestKeepValidTailLargeBatchSplitBoundary(t *testing.T) {
	const keepN = 16
	calls := make([]models.ToolCallRequest, keepN)
	msgs := make([]models.ChatMessage, 0, keepN+1)
	asst := models.ChatMessage{Role: "assistant"}
	for i := 0; i < keepN; i++ {
		id := fmt.Sprintf("call_%02d", i)
		calls[i] = models.ToolCallRequest{ID: id, Type: "function", Function: models.ToolCallFunction{Name: "f", Arguments: "{}"}}
	}
	asst.ToolCalls = calls
	msgs = append(msgs, asst)
	for i := 0; i < keepN; i++ {
		msgs = append(msgs, models.ChatMessage{Role: "tool", ToolCallID: fmt.Sprintf("call_%02d", i), StringContent: fmt.Sprintf("res_%02d", i)})
	}
	// len(msgs) == keepN+1 > keepN, so the naive cut would start at index 1 (a tool result)
	// and orphan all results. The backup must pull the assistant (index 0) into the window.
	got := keepValidTail(msgs, keepN, 1_000_000)
	if len(got) != keepN+1 {
		t.Fatalf("a single %d-call batch must survive whole, got %d of %d messages", keepN, len(got), keepN+1)
	}
	if got[0].Role != "assistant" || len(got[0].ToolCalls) != keepN {
		t.Fatalf("declaring assistant should lead the tail, got %+v", got[0])
	}
}

// TestAutoCompactTailExactSixteenMessages proves the verbatim tail is capped at exactly
// keepN of the most-recent working messages (oldest beyond keepN dropped), independent of
// the budget shed.
func TestAutoCompactTailExactSixteenMessages(t *testing.T) {
	r := &chatCountRouter{summary: "EXACT_SUMMARY"}
	s, _ := compactSession(t, r)
	const keepN = domain.AutoCompactVerbatimTailMessages
	// A big OLD note trips the soft threshold and is excluded by the keepN cut; the small
	// recent notes fit the budget, so the tail is bounded only by keepN.
	s.InjectNote("BIG_OLD" + strings.Repeat("x", 260_000))
	const total = 20
	for i := 0; i < total; i++ {
		s.InjectNote(fmt.Sprintf("NOTE_%03d", i))
	}
	s.maybeAutoCompact(context.Background(), "run_test")

	tail := s.Messages()[domain.ControlMessageCount+1:] // after controls + summary note
	if len(tail) != keepN {
		t.Fatalf("tail = %d messages, want exactly keepN=%d", len(tail), keepN)
	}
	// Tail is the most-recent keepN notes (NOTE_004..NOTE_019), in order.
	if !strings.Contains(tail[0].StringContent, fmt.Sprintf("NOTE_%03d", total-keepN)) {
		t.Fatalf("first tail = %q, want NOTE_%03d", tail[0].StringContent, total-keepN)
	}
	if !strings.Contains(tail[keepN-1].StringContent, fmt.Sprintf("NOTE_%03d", total-1)) {
		t.Fatalf("last tail = %q, want NOTE_%03d", tail[keepN-1].StringContent, total-1)
	}
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "NOTE_003") || strings.Contains(m.StringContent, "BIG_OLD") {
			t.Fatal("notes older than the keepN window must be gone")
		}
	}
}

// TestAutoCompactDoesNotRetrip proves the rebuilt history (controls + summary + tail) lands
// back under the soft threshold and a second pre-turn check does NOT re-compact — the tail
// budget plus the zeroed lastPromptTokens keep it from looping.
func TestAutoCompactDoesNotRetrip(t *testing.T) {
	r := &chatCountRouter{summary: "NORETRIP_SUMMARY"}
	s, _ := compactSession(t, r)
	s.InjectNote("OLD_BIG" + strings.Repeat("x", 260_000))
	s.InjectNote("RECENT_A")
	s.InjectNote("RECENT_B")

	s.maybeAutoCompact(context.Background(), "run_test")
	if r.chatCalls != 1 {
		t.Fatalf("first pass should compact once, got %d", r.chatCalls)
	}
	s.maybeAutoCompact(context.Background(), "run_test")
	if r.chatCalls != 1 {
		t.Fatalf("second pass must not re-compact the freshly-shrunk history, got %d", r.chatCalls)
	}
}

// TestAutoCompactRehydratesToolTailExact proves a verbatim tool pair in the tail survives
// the persist→rehydrate round-trip with its ToolCalls / ToolCallID intact (not just text).
func TestAutoCompactRehydratesToolTailExact(t *testing.T) {
	r := &chatCountRouter{summary: "TOOLTAIL_SUMMARY"}
	s, store := compactSession(t, r)
	s.mu.Lock()
	s.messages = append(s.messages,
		models.TextMessage("user", "BIG_OLD"+strings.Repeat("x", 260_000)), // shed by budget
		models.ChatMessage{Role: "assistant", ToolCalls: []models.ToolCallRequest{{
			ID: "call_keep", Type: "function",
			Function: models.ToolCallFunction{Name: "terminal.read", Arguments: `{"terminalId":"term_abc"}`},
		}}},
		models.ChatMessage{Role: "tool", ToolCallID: "call_keep", StringContent: "tail output"},
	)
	s.mu.Unlock()

	s.maybeAutoCompact(context.Background(), "run_test")

	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("expected a resume from the compaction marker")
	}
	var sawCall, sawResult bool
	for _, m := range res.RestoredMessages {
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_keep" && strings.Contains(tc.Function.Arguments, "term_abc") {
				sawCall = true
			}
		}
		if m.Role == "tool" && m.ToolCallID == "call_keep" {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("rehydrated tail lost the tool pair (call=%v result=%v): %+v", sawCall, sawResult, res.RestoredMessages)
	}
}
