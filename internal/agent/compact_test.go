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
	if msgs[3].Role != "user" || !strings.Contains(msgs[3].StringContent, "compacted summary") ||
		!strings.Contains(msgs[3].StringContent, "goals: X") {
		t.Fatalf("msg[3] = %+v want a compacted summary note", msgs[3])
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "first") {
			t.Fatal("old turns must be gone from context")
		}
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
		if strings.Contains(m.StringContent, "alpha") || strings.Contains(m.StringContent, "compacted summary") {
			t.Fatal("clear must drop turns and any summary note")
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
		if strings.Contains(m.StringContent, "compacted summary") || strings.Contains(m.StringContent, "goals: X") {
			t.Fatal("clear must drop a prior compaction summary")
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
	r := &chatCountRouter{summary: "AUTO_SUMMARY"}
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
		if strings.Contains(m.StringContent, "compacted summary") && strings.Contains(m.StringContent, "AUTO_SUMMARY") {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("history should be replaced with the compacted summary note")
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
	saved := s.distillCompact(context.Background(), "user: did X\nassistant: ok")
	if saved != 1 {
		t.Fatalf("saved=%d want 1 (fact B already exists, only fact A is novel)", saved)
	}
	if len(mem.inserted) != 1 || mem.inserted[0].Content != "fact A" {
		t.Fatalf("unexpected inserts: %+v", mem.inserted)
	}
	if mem.inserted[0].Source != domain.MemoryCompact {
		t.Fatalf("source = %q, want compact", mem.inserted[0].Source)
	}
}

func TestDistillCompactNilStoreSkipsModelCall(t *testing.T) {
	r := &jsonChatRouter{content: `["x"]`}
	s, _ := compactSession(t, r) // no MemoryStore wired
	if saved := s.distillCompact(context.Background(), "transcript"); saved != 0 {
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
	if saved := s.distillCompact(context.Background(), "transcript"); saved != 0 {
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
	if saved := s.distillCompact(context.Background(), "   "); saved != 0 {
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
	r := &seqChatRouter{replies: []string{"AUTO_SUMMARY", `["distilled durable fact"]`}}
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
		return models.ChatResult{Content: "AUTO_SUMMARY"}, nil
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
		s.maybeAutoCompact(ctx)
		if got := len(s.Messages()); got != initialLen {
			t.Fatalf("failure %d truncated history too early: %d messages (want %d)", i+1, got, initialLen)
		}
	}
	// The threshold'th failure crosses the line → lossy truncation.
	s.maybeAutoCompact(ctx)
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
	r := &flakyRouter{failFirst: 2, summary: "RECOVERED_SUMMARY"}
	s, _ := compactSession(t, r) // no MemoryStore ⇒ success path spawns no distill goroutine
	// Two notes (a lone note trips the "no real history" guard). Over the SOFT
	// threshold but under the hard ceiling, so failures climb the counter without
	// ever truncating.
	s.InjectNote("keep-small")
	s.InjectNote(strings.Repeat("x", (domain.AutoCompactTokenThreshold+20_000)*domain.CharsPerToken))
	ctx := context.Background()

	s.maybeAutoCompact(ctx)
	s.maybeAutoCompact(ctx)
	if s.compactFailures != 2 {
		t.Fatalf("compactFailures = %d after two outages, want 2", s.compactFailures)
	}
	s.maybeAutoCompact(ctx) // succeeds → compacts + resets
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

	s.maybeAutoCompact(ctx)
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
