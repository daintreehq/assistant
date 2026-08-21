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

// openCompactionGate is the capability provider for a backend advertising the exact contract.
func openCompactionGate() func() (backend.ContextCompactionCaps, bool) {
	return func() (backend.ContextCompactionCaps, bool) {
		return backend.ContextCompactionCaps{
			Enabled:             true,
			StreamEvent:         "compaction",
			Delivery:            "before_done",
			AtMostOnce:          true,
			StreamingOnly:       true,
			BestEffort:          true,
			AppendOnly:          true,
			BlockMessageName:    backend.ContextCompactionBlockName,
			Span:                backend.ContextCompactionSpanCaps{Collection: "input.messages", IndexBase: new(int), EndExclusive: true, ExcludesCurrentReply: true},
			TurnIDMatchRequired: true,
			// The backend's default cap (cap_compaction_block_bytes).
			MaxBlockContentBytes: 65_536,
		}, true
	}
}

// compactionBackend replies with plain text and, on the round named by `onRound`,
// hands back a compaction block the way a real committed stream would.
type compactionBackend struct {
	backendFromRouter
	mu      sync.Mutex
	reqs    []backend.RespondRequest
	onRound int
	block   *backend.StreamCompaction
	round   int
	// promptTokens, when set, makes the fake report a provider prompt-token figure the
	// way a real round does — the value the session stashes for the auto-compact gate.
	promptTokens int
	// keepBlockTurnID delivers the block with its own turn id instead of stamping the
	// request's, so a FOREIGN block can be pushed through the live loop.
	keepBlockTurnID bool
}

func (b *compactionBackend) RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	round := b.round
	b.round++
	b.mu.Unlock()
	res, err := b.backendFromRouter.RespondStream(ctx, req, cb)
	if err == nil && b.promptTokens > 0 {
		res.Usage.PromptTokens = b.promptTokens
	}
	if err == nil && round == b.onRound && b.block != nil {
		// Stamp the block with THIS request's turn id, the way the real backend does —
		// so the turn-id gate is genuinely exercised rather than bypassed by a fixture.
		block := *b.block
		if !b.keepBlockTurnID {
			block.TurnID = req.Session.TurnID
		}
		res.Compaction = &block
	}
	return res, err
}

func (b *compactionBackend) sent() []backend.RespondRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backend.RespondRequest(nil), b.reqs...)
}

// compactionTestSession builds a session over a recording store with the capability
// gate open, and seeds `history` as already-established conversation.
func compactionTestSession(t *testing.T, history []models.ChatMessage) (*Session, *recordingStore, *compactionBackend) {
	t.Helper()
	store := &recordingStore{}
	be := &compactionBackend{backendFromRouter: backendFromRouter{r: plainRouter()}, onRound: -1}
	s := NewSession(SessionDeps{
		Backend:                  be,
		Tools:                    &fakeTools{},
		Store:                    store,
		SessionID:                "ses_compaction",
		Events:                   NoopEventSink{},
		BackendContextCompaction: openCompactionGate(),
	})
	for _, m := range history {
		s.pushMessage(m)
	}
	return s, store, be
}

// seedHistory is a well-formed four-message conversation: two complete turns, so a
// span of [0,2) is a legal replacement and index 2 is a user boundary.
func seedHistory() []models.ChatMessage {
	return []models.ChatMessage{
		models.TextMessage("user", "u1"),
		models.TextMessage("assistant", "a1"),
		models.TextMessage("user", "u2"),
		models.TextMessage("assistant", "a2"),
	}
}

func validBlock(turnID string, start, end int) *backend.StreamCompaction {
	return &backend.StreamCompaction{
		TurnID:   turnID,
		Replaces: backend.StreamCompactionSpan{StartIndex: &start, EndIndex: &end},
		Block: backend.StreamCompactionBlock{
			Role:    "user",
			Name:    backend.ContextCompactionBlockName,
			Content: "Reconciled: u1/a1 established the worktree.",
		},
	}
}

func roles(msgs []models.ChatMessage) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		label := m.Role
		if isCompactionBlockMessage(m) {
			label = "BLOCK"
		}
		parts = append(parts, label+":"+m.ContentToText())
	}
	return strings.Join(parts, "|")
}

// The happy path: the block replaces its span and the tail survives verbatim.
func TestApplyServerCompactionSplicesTheSpan(t *testing.T) {
	s, store, _ := compactionTestSession(t, seedHistory())
	baselineRows := len(store.msgs)

	applied, reason := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1")
	if !applied {
		t.Fatalf("expected the block to apply, refused as %q", reason)
	}

	got := roles(s.Messages())
	want := "BLOCK:Reconciled: u1/a1 established the worktree.|user:u2|assistant:a2"
	if got != want {
		t.Fatalf("history = %s\nwant       %s", got, want)
	}

	// Durable form: a marker moves the rehydration boundary, then the block and the
	// tail land after it. The rows the block replaced stay in the table, behind the
	// marker — an append-only log, exactly as /compact leaves them.
	newRows := store.msgs[baselineRows:]
	if len(newRows) != 4 {
		t.Fatalf("expected marker + block + 2 tail rows, got %d", len(newRows))
	}
	if newRows[0].Role != "system" || !strings.HasPrefix(newRows[0].Content, compactionMarkerPrefix) {
		t.Errorf("first new row must be a boundary marker, got %+v", newRows[0])
	}
	if newRows[1].Name == nil || *newRows[1].Name != backend.ContextCompactionBlockName {
		t.Fatalf("the block row must persist its reserved name, got %+v", newRows[1].Name)
	}
	if len(store.msgs) <= baselineRows {
		t.Error("the replaced rows must not be deleted")
	}
}

// Every gate, one case each. All of them must fail OPEN — history untouched, no error,
// nothing a user could see — because server-side compaction is best-effort by contract
// and the turn that carried the block has already produced its answer.
func TestApplyServerCompactionRejectionsLeaveHistoryIntact(t *testing.T) {
	// A history with a tool transaction spanning the would-be boundary, used by the
	// tool-split case.
	toolHistory := []models.ChatMessage{
		models.TextMessage("user", "u1"),
		{Role: "assistant", ContentNull: true, ToolCalls: []models.ToolCallRequest{{ID: "tc_1", Type: "function"}}},
		models.TextMessage("user", "u2"),
		{Role: "tool", StringContent: "late result", ToolCallID: "tc_1"},
		models.TextMessage("user", "u3"),
	}

	cases := []struct {
		name    string
		history []models.ChatMessage
		block   *backend.StreamCompaction
		sentLen int
		turnID  string
		want    compactionRejectReason
	}{
		{"turn id mismatch", seedHistory(), validBlock("turn_other", 0, 2), 4, "turn_1", compactionRejectTurnID},
		{"empty turn id", seedHistory(), validBlock("", 0, 2), 4, "turn_1", compactionRejectTurnID},
		{"end past what was sent", seedHistory(), validBlock("turn_1", 0, 4), 4, "turn_1", compactionRejectSpanBounds},
		{"end beyond history", seedHistory(), validBlock("turn_1", 0, 9), 4, "turn_1", compactionRejectSpanBounds},
		{"negative start", seedHistory(), validBlock("turn_1", -1, 2), 4, "turn_1", compactionRejectSpanBounds},
		{"empty span", seedHistory(), validBlock("turn_1", 2, 2), 4, "turn_1", compactionRejectSpanBounds},
		{"inverted span", seedHistory(), validBlock("turn_1", 3, 1), 4, "turn_1", compactionRejectSpanBounds},
		{"start off a user boundary", seedHistory(), validBlock("turn_1", 1, 2), 4, "turn_1", compactionRejectSpanBoundry},
		{"end off a user boundary", seedHistory(), validBlock("turn_1", 0, 3), 4, "turn_1", compactionRejectSpanBoundry},
		{"splits a tool transaction", toolHistory, validBlock("turn_1", 0, 2), 5, "turn_1", compactionRejectToolSplit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, store, _ := compactionTestSession(t, tc.history)
			before := roles(s.Messages())
			rowsBefore := len(store.msgs)

			applied, reason := s.applyServerCompaction(tc.block, tc.sentLen, tc.turnID)
			if applied {
				t.Fatal("a refused block must not be applied")
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
			if got := roles(s.Messages()); got != before {
				t.Errorf("history changed on rejection:\n got %s\nwant %s", got, before)
			}
			if len(store.msgs) != rowsBefore {
				t.Error("a refused block must write nothing durable")
			}
		})
	}
}

// Block-shape gates, checked before any index arithmetic touches history.
func TestApplyServerCompactionRejectsMalformedBlocks(t *testing.T) {
	mutations := map[string]struct {
		mutate func(*backend.StreamCompaction)
		want   compactionRejectReason
	}{
		"assistant role": {func(c *backend.StreamCompaction) { c.Block.Role = "assistant" }, compactionRejectBlockShape},
		"system role":    {func(c *backend.StreamCompaction) { c.Block.Role = "system" }, compactionRejectBlockShape},
		"wrong name":     {func(c *backend.StreamCompaction) { c.Block.Name = "summary" }, compactionRejectBlockShape},
		"no name":        {func(c *backend.StreamCompaction) { c.Block.Name = "" }, compactionRejectBlockShape},
		"empty content":  {func(c *backend.StreamCompaction) { c.Block.Content = "" }, compactionRejectBlockShape},
		"blank content":  {func(c *backend.StreamCompaction) { c.Block.Content = "   \n\t" }, compactionRejectBlockShape},
		"invalid utf-8":  {func(c *backend.StreamCompaction) { c.Block.Content = string([]byte{0xff, 0xfe}) }, compactionRejectBlockShape},
		"over the byte cap": {func(c *backend.StreamCompaction) {
			c.Block.Content = strings.Repeat("x", 65_537)
		}, compactionRejectBlockSize},
	}
	for name, tc := range mutations {
		t.Run(name, func(t *testing.T) {
			s, _, _ := compactionTestSession(t, seedHistory())
			block := validBlock("turn_1", 0, 2)
			tc.mutate(block)
			applied, reason := s.applyServerCompaction(block, 4, "turn_1")
			if applied {
				t.Fatal("a malformed block must not be applied")
			}
			if reason != tc.want {
				t.Errorf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

// The gate governs ACCEPTANCE. A backend that does not advertise the exact contract —
// or one this client has not asked — behaves exactly as it does today.
func TestApplyServerCompactionRefusedWhenCapabilityClosed(t *testing.T) {
	for name, gate := range map[string]func() (backend.ContextCompactionCaps, bool){
		"nil provider": nil,
		"closed gate": func() (backend.ContextCompactionCaps, bool) {
			return backend.ContextCompactionCaps{}, false
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &recordingStore{}
			s := NewSession(SessionDeps{
				Backend: backendFromRouter{r: plainRouter()}, Tools: &fakeTools{},
				Store: store, SessionID: "ses_gate", Events: NoopEventSink{},
				BackendContextCompaction: gate,
			})
			for _, m := range seedHistory() {
				s.pushMessage(m)
			}
			applied, reason := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1")
			if applied {
				t.Fatal("a closed gate must refuse the block")
			}
			if reason != compactionRejectCapability {
				t.Errorf("reason = %q", reason)
			}
			if len(s.Messages()) != 4 {
				t.Error("history must be untouched")
			}
		})
	}
}

// A second block must never reach back over the first. Doing so would replace frozen
// state with a summary of a summary AND move the boundary the next request's selector
// reads — the one thing that makes the whole stateless design work.
func TestApplyServerCompactionRefusesToReachOverAFrozenBlock(t *testing.T) {
	s, _, _ := compactionTestSession(t, seedHistory())
	if applied, reason := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1"); !applied {
		t.Fatalf("first block refused as %q", reason)
	}
	// History is now [BLOCK, u2, a2]. A span starting at 0 would swallow the block.
	applied, reason := s.applyServerCompaction(validBlock("turn_2", 0, 1), 3, "turn_2")
	if applied {
		t.Fatal("a span covering an existing block must be refused")
	}
	if reason != compactionRejectSpanFrozen {
		t.Errorf("reason = %q, want %q", reason, compactionRejectSpanFrozen)
	}
}

// A second block that opens strictly after the first composes cleanly — the indices
// address the already-spliced array, so no coordinate mapping is ever needed.
func TestApplyServerCompactionComposesAfterAnEarlierBlock(t *testing.T) {
	s, store, _ := compactionTestSession(t, seedHistory())
	if applied, _ := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1"); !applied {
		t.Fatal("first block refused")
	}
	// [BLOCK, u2, a2] — add another complete turn so [1,3) is a legal second span.
	s.pushMessage(models.TextMessage("user", "u3"))
	s.pushMessage(models.TextMessage("assistant", "a3"))

	second := validBlock("turn_2", 1, 3)
	second.Block.Content = "Reconciled again."
	if applied, reason := s.applyServerCompaction(second, 5, "turn_2"); !applied {
		t.Fatalf("second block refused as %q", reason)
	}
	got := roles(s.Messages())
	want := "BLOCK:Reconciled: u1/a1 established the worktree.|BLOCK:Reconciled again.|user:u3|assistant:a3"
	if got != want {
		t.Fatalf("history = %s\nwant       %s", got, want)
	}

	// Live history is only half the claim. The FIRST block sits in the second write's
	// prefix, so a group that persisted only [marker, block, tail] would resume without
	// it — memory and disk telling two different stories about the same conversation.
	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("rehydration reported no history")
	}
	if resumed := roles(res.RestoredMessages); resumed != got {
		t.Fatalf("resumed = %s\nlive    = %s", resumed, got)
	}
}

// A stale prompt_tokens figure measured the PRE-splice history. Left set, the next
// auto-compact check would compact again immediately on top of the server's work.
func TestApplyServerCompactionClearsTheStalePromptTokenStash(t *testing.T) {
	s, _, _ := compactionTestSession(t, seedHistory())
	s.mu.Lock()
	s.lastPromptTokens = 900_000
	s.mu.Unlock()

	if applied, _ := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1"); !applied {
		t.Fatal("block refused")
	}
	s.mu.Lock()
	stash := s.lastPromptTokens
	s.mu.Unlock()
	if stash != 0 {
		t.Fatalf("lastPromptTokens = %d, want 0 so the next check re-measures", stash)
	}
}

// End to end through a real turn: the block rides the round's result, the turn loop
// applies it, and the very NEXT request carries block + tail instead of the replaced
// prefix — with the reserved name intact, which is the only thing telling the server
// that history is frozen.
func TestTurnLoopSendsTheCompactedPrefixOnTheNextRequest(t *testing.T) {
	s, _, be := compactionTestSession(t, seedHistory())
	// Round 0 of the next turn sends [u1,a1,u2,a2,u5]. Replacing [0,2) leaves the
	// second turn plus the live one, and index 2 is a user boundary.
	be.onRound = 0
	be.block = validBlock("", 0, 2)

	if _, err := s.Send(context.Background(), "u5", SendOptions{}); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if got := roles(s.Messages()); !strings.HasPrefix(got, "BLOCK:") {
		t.Fatalf("the loop did not apply the block; history = %s", got)
	}

	// The next turn is where the saving is actually taken.
	if _, err := s.Send(context.Background(), "u6", SendOptions{}); err != nil {
		t.Fatalf("follow-up turn failed: %v", err)
	}
	sent := be.sent()
	next := sent[len(sent)-1].Input.Messages
	if len(next) == 0 {
		t.Fatal("the follow-up request carried no messages")
	}
	if next[0].Role != "user" || next[0].Name != backend.ContextCompactionBlockName {
		t.Fatalf("the request must open with the reserved block, got role=%q name=%q", next[0].Role, next[0].Name)
	}
	// Assert the EXACT ordered sequence, not a count plus an absence. Uncompacted, the
	// follow-up would carry [u1,a1,u2,a2,u5,a5,u6]; the block stands in for the first
	// two. A count alone would let a compensating drop-and-duplicate, or a reordering,
	// through — and either would be a broken conversation, not a smaller one.
	wantRoles := []string{"user", "user", "assistant", "user", "assistant", "user"}
	if len(next) != len(wantRoles) {
		t.Fatalf("expected %d messages (block + u2,a2,u5,a5,u6), got %d", len(wantRoles), len(next))
	}
	for i, want := range wantRoles {
		if next[i].Role != want {
			t.Errorf("message %d role = %q, want %q", i, next[i].Role, want)
		}
	}
	for _, m := range next {
		if body := string(m.Content); body == `"u1"` || body == `"a1"` {
			t.Errorf("the replaced prefix was re-sent: %s", body)
		}
	}
}

// A block stamped with someone else's turn, delivered through the LIVE loop. The weak
// version of this test called applyServerCompaction directly, which would still have
// passed if the turn loop had handed the block its OWN turn id as the expected one —
// accepting every foreign block ever sent.
func TestTurnLoopIgnoresABlockStampedWithAnotherTurn(t *testing.T) {
	s, store, be := compactionTestSession(t, seedHistory())
	be.onRound = 0
	be.keepBlockTurnID = true
	be.block = validBlock("turn_from_another_session", 0, 2)

	if _, err := s.Send(context.Background(), "u5", SendOptions{}); err != nil {
		t.Fatalf("a foreign block must not break the turn: %v", err)
	}
	got := roles(s.Messages())
	if strings.Contains(got, "BLOCK:") {
		t.Fatalf("a foreign block was applied; history = %s", got)
	}
	if !strings.HasPrefix(got, "user:u1|assistant:a1|") {
		t.Errorf("the replaced prefix must be intact; history = %s", got)
	}
	for _, r := range store.msgs {
		if r.Name != nil {
			t.Errorf("a foreign block reached the durable log: %+v", r)
		}
	}
}

// The durable half, and the one the whole feature turns on: a block that does not
// survive a restart means the next request re-sends history the server already froze,
// and it is re-compacted forever. This exercises the real path — persist, list,
// rehydrate, rebuild a session, encode for the wire.
func TestServerCompactionSurvivesAStoreRehydrateRoundTrip(t *testing.T) {
	s, store, _ := compactionTestSession(t, nil)
	for _, m := range seedHistory() {
		s.pushMessage(m) // pushMessage persists
	}
	if applied, reason := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1"); !applied {
		t.Fatalf("block refused as %q", reason)
	}

	// A fresh process: everything comes back through the persisted rows alone.
	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("rehydration reported no history")
	}
	got := roles(res.RestoredMessages)
	want := "BLOCK:Reconciled: u1/a1 established the worktree.|user:u2|assistant:a2"
	if got != want {
		t.Fatalf("rehydrated = %s\nwant        %s", got, want)
	}

	// And the reserved name must reach the WIRE, not merely memory — the encoder used
	// to drop it, which would have made the whole feature a silent no-op after restart.
	wire, err := ToBackendMessages(res.RestoredMessages)
	if err != nil {
		t.Fatalf("encoding the resumed history failed: %v", err)
	}
	if wire[0].Name != backend.ContextCompactionBlockName {
		t.Fatalf("the block reached the wire without its name: %+v", wire[0])
	}
	for _, m := range wire[1:] {
		if m.Name != "" {
			t.Errorf("an ordinary message carried a name: %+v", m)
		}
	}
}

// The same durable path in the shape the feature actually runs in: a span that opens
// AFTER some untouched history, then a second compaction stacked on the first. Both
// splices carry a non-empty prefix, and the second one's prefix contains the earlier
// frozen block — which stands in for the whole first phase of the conversation.
//
// This is the steady state, not an edge case: the frozen-history gate REQUIRES every
// span after the first to open past the last block, so every compaction but the first
// has a prefix to lose. Pinning resumed == live is the assertion that matters; a
// hand-written "want" would have to be kept in step with the splice rules to stay
// meaningful, and the bug this guards is precisely the two diverging.
func TestServerCompactionRoundTripKeepsThePrefixAndTheEarlierBlock(t *testing.T) {
	s, store, _ := compactionTestSession(t, nil)
	for _, m := range seedHistory() {
		s.pushMessage(m) // pushMessage persists
	}
	s.pushMessage(models.TextMessage("user", "u3"))
	s.pushMessage(models.TextMessage("assistant", "a3"))

	// [u1,a1,u2,a2,u3,a3] — replace the SECOND turn only, so the splice keeps a prefix.
	if applied, reason := s.applyServerCompaction(validBlock("turn_1", 2, 4), 6, "turn_1"); !applied {
		t.Fatalf("first block refused as %q", reason)
	}
	// [u1,a1,BLOCK1,u3,a3] — one more complete turn makes [3,5) a legal second span.
	s.pushMessage(models.TextMessage("user", "u4"))
	s.pushMessage(models.TextMessage("assistant", "a4"))
	second := validBlock("turn_2", 3, 5)
	second.Block.Content = "Reconciled again."
	if applied, reason := s.applyServerCompaction(second, 7, "turn_2"); !applied {
		t.Fatalf("second block refused as %q", reason)
	}

	live := roles(s.Messages())
	want := "user:u1|assistant:a1|BLOCK:Reconciled: u1/a1 established the worktree.|" +
		"BLOCK:Reconciled again.|user:u4|assistant:a4"
	if live != want {
		t.Fatalf("live history = %s\nwant          %s", live, want)
	}

	// A fresh process, through the persisted rows alone.
	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("rehydration reported no history")
	}
	if resumed := roles(res.RestoredMessages); resumed != live {
		t.Fatalf("durable history diverged from live history across a restart\nresumed = %s\nlive    = %s", resumed, live)
	}
}

// The encoder gap that would have made everything above a no-op, pinned on its own.
func TestToBackendMessagesPreservesTheMessageName(t *testing.T) {
	in := []models.ChatMessage{
		{Role: "user", StringContent: "frozen", Name: backend.ContextCompactionBlockName},
		models.TextMessage("assistant", "ok"),
	}
	out, err := ToBackendMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Name != backend.ContextCompactionBlockName {
		t.Errorf("name dropped on the wire: %+v", out[0])
	}
	if out[1].Name != "" {
		t.Errorf("name invented on an ordinary message: %+v", out[1])
	}
}

// The tool-closure mirror, exercised directly across the cases that matter.
func TestCompactionSpanToolClosed(t *testing.T) {
	call := func(id string) models.ChatMessage {
		return models.ChatMessage{Role: "assistant", ContentNull: true,
			ToolCalls: []models.ToolCallRequest{{ID: id, Type: "function"}}}
	}
	result := func(id string) models.ChatMessage {
		return models.ChatMessage{Role: "tool", StringContent: "r", ToolCallID: id}
	}
	u := func(s string) models.ChatMessage { return models.TextMessage("user", s) }

	t.Run("closed pair inside", func(t *testing.T) {
		msgs := []models.ChatMessage{u("u1"), call("t1"), result("t1"), u("u2")}
		if !compactionSpanToolClosed(msgs, 0, 3) {
			t.Error("a fully-contained transaction must be closed")
		}
	})
	t.Run("call inside, result outside", func(t *testing.T) {
		msgs := []models.ChatMessage{u("u1"), call("t1"), u("u2"), result("t1")}
		if compactionSpanToolClosed(msgs, 0, 2) {
			t.Error("a call whose result lands outside must not be closed")
		}
	})
	t.Run("result inside, call outside", func(t *testing.T) {
		msgs := []models.ChatMessage{call("t1"), u("u1"), result("t1"), u("u2")}
		if compactionSpanToolClosed(msgs, 1, 3) {
			t.Error("an orphaned result inside the span must not be closed")
		}
	})
	t.Run("unidentifiable call refuses", func(t *testing.T) {
		msgs := []models.ChatMessage{u("u1"), call(""), u("u2")}
		if compactionSpanToolClosed(msgs, 0, 2) {
			t.Error("a call with no id cannot be shown safe")
		}
	})
	t.Run("no tools at all", func(t *testing.T) {
		if !compactionSpanToolClosed(seedHistory(), 0, 2) {
			t.Error("a span with no tool traffic is trivially closed")
		}
	})
}

// The ordering that makes the stash-clearing actually stick.
//
// emitBackendUsage stashes the round's reported prompt_tokens, and that figure measured
// the PRE-splice prompt. If the splice runs before it, the stash is immediately
// overwritten with a number describing history the block just replaced — and the next
// maybeAutoCompact compacts again, on top of the server's work, for no reason a log
// would explain. The apply therefore has to be the last writer, and this pins it.
func TestTurnLoopClearsTheStalePromptStashAfterCompacting(t *testing.T) {
	s, _, be := compactionTestSession(t, seedHistory())
	be.onRound = 0
	be.block = validBlock("", 0, 2)
	be.promptTokens = 900_000

	if _, err := s.Send(context.Background(), "u5", SendOptions{}); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if got := roles(s.Messages()); !strings.HasPrefix(got, "BLOCK:") {
		t.Fatalf("the loop did not apply the block; history = %s", got)
	}
	s.mu.Lock()
	stash := s.lastPromptTokens
	s.mu.Unlock()
	if stash != 0 {
		t.Fatalf("lastPromptTokens = %d after a compaction; the pre-splice figure must not survive it", stash)
	}
}

// The control: a turn that compacted NOTHING must still stash the round's figure, or
// the auto-compact gate loses the only real measurement it has and falls back to the
// char estimate on every turn.
func TestTurnLoopKeepsThePromptStashWhenNothingCompacted(t *testing.T) {
	s, _, be := compactionTestSession(t, seedHistory())
	be.onRound = -1
	be.promptTokens = 12_345

	if _, err := s.Send(context.Background(), "u5", SendOptions{}); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	s.mu.Lock()
	stash := s.lastPromptTokens
	s.mu.Unlock()
	if stash != 12_345 {
		t.Fatalf("lastPromptTokens = %d, want the round's reported figure preserved", stash)
	}
}

// nonAtomicStore records rows but cannot write a group as one unit — the shape of any
// store that has not opted into AtomicMessageStore.
type nonAtomicStore struct {
	msgs []domain.ConversationMessageRecord
}

func (s *nonAtomicStore) InsertMessage(rec domain.ConversationMessageRecord) (domain.ConversationMessageRecord, error) {
	s.msgs = append(s.msgs, rec)
	return rec, nil
}

// The compaction boundary must be durably safe or not written at all. A marker row that
// committed without the block and tail behind it would hide intact history and resume a
// conversation that was never written — while the live turn reported success, which is
// the worst way to lose a transcript.
func TestApplyServerCompactionDeclinesWhenTheBoundaryCannotBeWrittenAtomically(t *testing.T) {
	for name, tc := range map[string]struct {
		store   MessageStore
		rows    func() int
		wantErr compactionRejectReason
	}{
		"store cannot group":  {store: &nonAtomicStore{}, wantErr: compactionRejectPersist},
		"group write refused": {store: &recordingStore{failGroupAt: 2}, wantErr: compactionRejectPersist},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSession(SessionDeps{
				Backend: backendFromRouter{r: plainRouter()}, Tools: &fakeTools{},
				Store: tc.store, SessionID: "ses_atomic", Events: NoopEventSink{},
				BackendContextCompaction: openCompactionGate(),
			})
			for _, m := range seedHistory() {
				s.pushMessage(m)
			}
			before := roles(s.Messages())

			applied, reason := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1")
			if applied {
				t.Fatal("a boundary that cannot be written atomically must not be applied")
			}
			if reason != tc.wantErr {
				t.Errorf("reason = %q, want %q", reason, tc.wantErr)
			}
			// The live history must be untouched too: a splice in memory that the log
			// cannot back would diverge from what a resume replays.
			if got := roles(s.Messages()); got != before {
				t.Errorf("history changed despite the refusal:\n got %s\nwant %s", got, before)
			}
		})
	}
}

// A refused group leaves NO durable trace and no gap in the sequence — the next ordinary
// message must number where the refused block would have.
func TestRefusedCompactionLeavesNoDurableGap(t *testing.T) {
	store := &recordingStore{failGroupAt: 1}
	s := NewSession(SessionDeps{
		Backend: backendFromRouter{r: plainRouter()}, Tools: &fakeTools{},
		Store: store, SessionID: "ses_gap", Events: NoopEventSink{},
		BackendContextCompaction: openCompactionGate(),
	})
	for _, m := range seedHistory() {
		s.pushMessage(m) // pushMessage persists; a second write would double every row
	}
	seedRows := len(store.msgs)
	nextSeq := store.msgs[seedRows-1].Seq + 1

	if applied, _ := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1"); applied {
		t.Fatal("the block must have been refused")
	}
	if n := len(store.msgs); n != seedRows {
		t.Fatalf("a refused group wrote %d rows, want none beyond the seed", n-seedRows)
	}
	s.persistMessage(models.TextMessage("user", "after"))
	if got := store.msgs[len(store.msgs)-1].Seq; got != nextSeq {
		t.Errorf("next row seq = %d, want %d — a refused compaction must not burn sequence numbers", got, nextSeq)
	}
}

// A compaction arriving on a LATER round of a tool loop, which is where the prompt cost
// of a long turn actually accumulates. The retained tail then contains a live tool
// transaction, and it must survive the splice intact and in order.
func TestCompactionOnAToolRoundKeepsTheLiveTransactionIntact(t *testing.T) {
	s, store, _ := compactionTestSession(t, seedHistory())
	// The live turn so far: a user message, an assistant tool-call turn, its result.
	s.pushMessage(models.TextMessage("user", "u5"))
	s.pushMessage(models.ChatMessage{Role: "assistant", ContentNull: true,
		ToolCalls: []models.ToolCallRequest{{ID: "tc_1", Type: "function"}}})
	s.pushMessage(models.ChatMessage{Role: "tool", StringContent: "terminal listed", ToolCallID: "tc_1"})

	// [u1,a1,u2,a2,u5,asst(tc_1),tool] — replacing [0,2) keeps the whole live transaction.
	applied, reason := s.applyServerCompaction(validBlock("turn_1", 0, 2), 7, "turn_1")
	if !applied {
		t.Fatalf("a mid-turn block was refused as %q", reason)
	}
	got := s.Messages()
	if len(got) != 6 || !isCompactionBlockMessage(got[0]) {
		t.Fatalf("history = %s", roles(got))
	}
	if len(got[4].ToolCalls) != 1 || got[4].ToolCalls[0].ID != "tc_1" {
		t.Errorf("the assistant tool-call turn did not survive: %+v", got[4])
	}
	if got[5].Role != "tool" || got[5].ToolCallID != "tc_1" {
		t.Errorf("the tool result did not survive: %+v", got[5])
	}

	// And it must still rehydrate as a VALID history — an orphaned tool result here
	// would 400 the next request rather than merely lose context.
	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("rehydration reported no history")
	}
	if res.DroppedRows != 0 {
		t.Errorf("rehydration dropped %d rows — the tail was not persisted validly", res.DroppedRows)
	}
	if roles(res.RestoredMessages) != roles(got) {
		t.Errorf("resumed history diverged:\n got %s\nwant %s", roles(res.RestoredMessages), roles(got))
	}
	if _, err := ToBackendMessages(res.RestoredMessages); err != nil {
		t.Errorf("the resumed history is not encodable: %v", err)
	}
}

// Rejection must be OBSERVATIONALLY silent, not merely undocumented. Every rejection test
// above runs on a NoopEventSink, so they would all still pass if a future change started
// emitting a warning on every turn of a session the server keeps trying to compact.
func TestRejectedServerCompactionEmitsNothing(t *testing.T) {
	sink := &recordingSink{}
	store := &recordingStore{}
	s := NewSession(SessionDeps{
		Backend: backendFromRouter{r: plainRouter()}, Tools: &fakeTools{},
		Store: store, SessionID: "ses_silent", Events: sink,
		BackendContextCompaction: openCompactionGate(),
	})
	for _, m := range seedHistory() {
		s.pushMessage(m)
	}
	before := len(sink.log)

	// One of each rejection class.
	for _, block := range []*backend.StreamCompaction{
		validBlock("turn_other", 0, 2), // turn id
		validBlock("turn_1", 0, 9),     // bounds
		validBlock("turn_1", 1, 2),     // boundary
	} {
		if applied, _ := s.applyServerCompaction(block, 4, "turn_1"); applied {
			t.Fatal("expected a rejection")
		}
	}
	if got := len(sink.log); got != before {
		t.Errorf("a rejected compaction emitted %d event(s); it must be invisible", got-before)
	}
}

// Two compaction mechanisms can run over one session — the server's and the client's
// /compact — and both write a boundary marker. Resume must follow the NEWEST one.
func TestRehydrateFollowsTheNewestBoundaryAcrossBothMechanisms(t *testing.T) {
	s, store, _ := compactionTestSession(t, nil)
	for _, m := range seedHistory() {
		s.pushMessage(m) // pushMessage persists
	}
	if applied, reason := s.applyServerCompaction(validBlock("turn_1", 0, 2), 4, "turn_1"); !applied {
		t.Fatalf("server block refused as %q", reason)
	}
	// Now the CLIENT compacts on top of the server's block.
	if err := s.Compact("client-side summary"); err != nil {
		t.Fatalf("/compact failed: %v", err)
	}

	res, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("rehydration reported no history")
	}
	if roles(res.RestoredMessages) != roles(s.Messages()) {
		t.Errorf("resume did not follow the newest boundary:\n got %s\nwant %s",
			roles(res.RestoredMessages), roles(s.Messages()))
	}
}

// The byte cap is measured in UTF-8 BYTES, not runes — the unit the capability names and
// the unit prompt cost is actually paid in. An ASCII-only test cannot tell the two apart.
func TestApplyServerCompactionMeasuresTheByteCapInBytes(t *testing.T) {
	s, _, _ := compactionTestSession(t, seedHistory())
	// "é" is two bytes. 200 of them is 400 bytes — over a 300-byte cap, but only 200 runes.
	gate := func() (backend.ContextCompactionCaps, bool) {
		caps, _ := openCompactionGate()()
		caps.MaxBlockContentBytes = 300
		return caps, true
	}
	s.deps.BackendContextCompaction = gate

	block := validBlock("turn_1", 0, 2)
	block.Block.Content = strings.Repeat("é", 200)
	if applied, reason := s.applyServerCompaction(block, 4, "turn_1"); applied {
		t.Fatal("400 bytes must exceed a 300-byte cap even at 200 runes")
	} else if reason != compactionRejectBlockSize {
		t.Errorf("reason = %q", reason)
	}

	// 150 of them is 300 bytes — exactly at the cap, and allowed.
	block.Block.Content = strings.Repeat("é", 150)
	if applied, reason := s.applyServerCompaction(block, 4, "turn_1"); !applied {
		t.Errorf("a block exactly at the cap must be accepted, refused as %q", reason)
	}
}
