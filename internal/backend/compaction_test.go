package backend

import (
	"encoding/json"
	"strings"
	"testing"
)

func intPtr(v int) *int { return &v }

func spanOf(start, end int) StreamCompactionSpan {
	return StreamCompactionSpan{StartIndex: intPtr(start), EndIndex: intPtr(end)}
}

// A minimal well-formed stream: meta, then whatever the caller adds, then done.
func compactionStream(middle string) string {
	return "event: meta\ndata: {\"state\":\"st_1\"}\n\n" +
		middle +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
}

const validCompactionEvent = `event: compaction
data: {"turn_id":"turn_abc","replaces":{"start_index":0,"end_index":4},"block":{"role":"user","name":"daintree_compaction","content":"Frozen state."}}

`

func TestParseRespondStream_CompactionBeforeDoneReachesResult(t *testing.T) {
	res, err := parseRespondStream(newTestBufReader(compactionStream(validCompactionEvent)), StreamCallbacks{})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if res.Compaction == nil {
		t.Fatal("expected a compaction block on the result")
	}
	if res.Compaction.TurnID != "turn_abc" {
		t.Errorf("turn id = %q", res.Compaction.TurnID)
	}
	if start, end, ok := res.Compaction.Replaces.Bounds(); !ok || start != 0 || end != 4 {
		t.Errorf("span = (%d,%d,%v)", start, end, ok)
	}
	if b := res.Compaction.Block; b.Role != "user" || b.Name != ContextCompactionBlockName || b.Content != "Frozen state." {
		t.Errorf("block = %+v", b)
	}
}

// The absent case is the overwhelmingly common one and must stay indistinguishable
// from today: no event, no block, no error.
func TestParseRespondStream_NoCompactionLeavesResultNil(t *testing.T) {
	res, err := parseRespondStream(newTestBufReader(compactionStream("")), StreamCallbacks{})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if res.Compaction != nil {
		t.Fatalf("expected no compaction, got %+v", res.Compaction)
	}
}

// Best-effort means the ANSWER always wins. A block this parser cannot read costs the
// turn its prompt savings and nothing else — the reply is already generated, and
// failing the stream over an optional optimisation would be the worst possible trade.
func TestParseRespondStream_MalformedCompactionDoesNotFailTheTurn(t *testing.T) {
	stream := compactionStream("event: compaction\ndata: {not json}\n\n" +
		"event: delta\ndata: {\"content\":\"hi\"}\n\n")
	res, err := parseRespondStream(newTestBufReader(stream), StreamCallbacks{})
	if err != nil {
		t.Fatalf("a malformed compaction must not fail the stream: %v", err)
	}
	if res.Compaction != nil {
		t.Error("a malformed compaction must not produce a block")
	}
	if res.Message.Content != "hi" {
		t.Errorf("content = %q, want the answer preserved", res.Message.Content)
	}
}

// at_most_once is part of the contract. A second block is evidence that client and
// server disagree about what the first one replaced, and the only safe reading of that
// is neither — while still delivering the answer.
func TestParseRespondStream_DuplicateCompactionInvalidatesBoth(t *testing.T) {
	res, err := parseRespondStream(newTestBufReader(compactionStream(validCompactionEvent+validCompactionEvent)), StreamCallbacks{})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if res.Compaction != nil {
		t.Fatalf("two blocks must invalidate each other, got %+v", res.Compaction)
	}
}

// The commit barrier. A compaction rides just ahead of `done`, so a stream that never
// reached `done` must not be able to rewrite the caller's history — the caller
// discards the whole result on error, but the field must be empty regardless so no
// future refactor can leak it.
func TestParseRespondStream_CompactionWithoutDoneIsNotReleased(t *testing.T) {
	stream := "event: meta\ndata: {\"state\":\"st_1\"}\n\n" + validCompactionEvent
	res, err := parseRespondStream(newTestBufReader(stream), StreamCallbacks{})
	if err == nil {
		t.Fatal("a stream ending before done must be an error")
	}
	if res.Compaction != nil {
		t.Fatalf("an uncommitted stream must release no block, got %+v", res.Compaction)
	}
}

// `done` is terminal by contract. Anything after it is never read, which is precisely
// why the backend puts compaction BEFORE it — this pins that the client half of that
// bargain holds.
func TestParseRespondStream_CompactionAfterDoneIsIgnored(t *testing.T) {
	stream := "event: meta\ndata: {\"state\":\"st_1\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n" + validCompactionEvent
	res, err := parseRespondStream(newTestBufReader(stream), StreamCallbacks{})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if res.Compaction != nil {
		t.Fatal("an event after the terminal done must not be read")
	}
}

// 256 KiB is the PROTOCOL maximum (the wire model's code-point cap and the ceiling on
// the configurable byte cap), not what a deployment serves by default. It must fit
// comfortably inside the SSE bounds — the point where a wrong constant would silently
// disable the feature for exactly the long conversations it exists to serve.
func TestParseRespondStream_MaxSizedCompactionBlockFits(t *testing.T) {
	content := strings.Repeat("x", 262_144)
	payload, err := json.Marshal(StreamCompaction{
		TurnID:   "turn_abc",
		Replaces: spanOf(0, 2),
		Block:    StreamCompactionBlock{Role: "user", Name: ContextCompactionBlockName, Content: content},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := compactionStream("event: compaction\ndata: " + string(payload) + "\n\n")
	res, perr := parseRespondStream(newTestBufReader(stream), StreamCallbacks{})
	if perr != nil {
		t.Fatalf("a max-sized block must parse: %v", perr)
	}
	if res.Compaction == nil || len(res.Compaction.Block.Content) != len(content) {
		t.Fatal("max-sized block did not survive the parse")
	}
}

// The issue's dedicated regression, at the stream boundary rather than in a renderer:
// a marker the backend failed to strip must reach neither the live token callback nor
// the assembled content — the two things every CLI surface is built from.
func TestParseRespondStream_LeakedDeclarationMarkerNeverReachesTheCaller(t *testing.T) {
	// Fragmented mid-marker on purpose: this is how a leak would actually arrive.
	stream := compactionStream(
		"event: delta\ndata: {\"content\":\"[[DAIN\"}\n\n" +
			"event: delta\ndata: {\"content\":\"TREE:FINAL]]\\n\"}\n\n" +
			"event: delta\ndata: {\"content\":\"The worktree is ready.\"}\n\n")

	var streamed strings.Builder
	res, err := parseRespondStream(newTestBufReader(stream), StreamCallbacks{
		OnContent: func(tok string) { streamed.WriteString(tok) },
	})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	const want = "The worktree is ready."
	if res.Message.Content != want {
		t.Errorf("assembled content = %q, want %q", res.Message.Content, want)
	}
	if streamed.String() != want {
		t.Errorf("streamed content = %q, want %q", streamed.String(), want)
	}
	if strings.Contains(res.Message.Content+streamed.String(), "DAINTREE") {
		t.Error("a leaked marker reached the caller")
	}
}

// The same path must be inert for ordinary prose — including a reply that legitimately
// opens with a bracket, which is the case a careless prefix guard would eat.
func TestParseRespondStream_OrdinaryProseIsUntouched(t *testing.T) {
	stream := compactionStream(
		"event: delta\ndata: {\"content\":\"[wor\"}\n\n" +
			"event: delta\ndata: {\"content\":\"ktree] ready\"}\n\n")
	var streamed strings.Builder
	res, err := parseRespondStream(newTestBufReader(stream), StreamCallbacks{
		OnContent: func(tok string) { streamed.WriteString(tok) },
	})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	const want = "[worktree] ready"
	if res.Message.Content != want || streamed.String() != want {
		t.Errorf("content=%q streamed=%q, want %q", res.Message.Content, streamed.String(), want)
	}
}

// ---- capability contract ----

// The full, correct descriptor as the backend actually serves it.
func validCompactionCaps() ContextCompactionCaps {
	return ContextCompactionCaps{
		Enabled:          true,
		StreamEvent:      "compaction",
		Delivery:         "before_done",
		AtMostOnce:       true,
		StreamingOnly:    true,
		BestEffort:       true,
		AppendOnly:       true,
		BlockMessageName: ContextCompactionBlockName,
		Span: ContextCompactionSpanCaps{
			Collection:           "input.messages",
			IndexBase:            intPtr(0),
			EndExclusive:         true,
			ExcludesCurrentReply: true,
		},
		TurnIDMatchRequired: true,
		// The backend's DEFAULT (cap_compaction_block_bytes, config.py). 256 KiB is the
		// configurable ceiling, exercised separately by the parser size tests — the
		// oracle fixture should be what a deployment actually serves.
		MaxBlockContentBytes: 65_536,
	}
}

func TestContextCompactionCaps_ReplayCompatibleRejectsEveryDeviation(t *testing.T) {
	ok := validCompactionCaps()
	if !ok.ReplayCompatible() {
		t.Fatal("the exact advertised contract must be compatible")
	}

	// Each mutation is an assumption the splice arithmetic depends on. A backend that
	// revised one of them ships blocks this client would apply WRONGLY — silently, and
	// to the user's own conversation — so every one of them must close the gate.
	deviations := map[string]func(*ContextCompactionCaps){
		"disabled":              func(c *ContextCompactionCaps) { c.Enabled = false },
		"other event name":      func(c *ContextCompactionCaps) { c.StreamEvent = "compact" },
		"delivery after done":   func(c *ContextCompactionCaps) { c.Delivery = "after_done" },
		"more than once":        func(c *ContextCompactionCaps) { c.AtMostOnce = false },
		"non-streaming too":     func(c *ContextCompactionCaps) { c.StreamingOnly = false },
		"not best effort":       func(c *ContextCompactionCaps) { c.BestEffort = false },
		"not append only":       func(c *ContextCompactionCaps) { c.AppendOnly = false },
		"other block name":      func(c *ContextCompactionCaps) { c.BlockMessageName = "summary" },
		"other collection":      func(c *ContextCompactionCaps) { c.Span.Collection = "history" },
		"one-based indices":     func(c *ContextCompactionCaps) { c.Span.IndexBase = intPtr(1) },
		"inclusive end":         func(c *ContextCompactionCaps) { c.Span.EndExclusive = false },
		"reply inside the span": func(c *ContextCompactionCaps) { c.Span.ExcludesCurrentReply = false },
		"no turn id match":      func(c *ContextCompactionCaps) { c.TurnIDMatchRequired = false },
		"no byte cap":           func(c *ContextCompactionCaps) { c.MaxBlockContentBytes = 0 },
	}
	for name, mutate := range deviations {
		caps := validCompactionCaps()
		mutate(&caps)
		if caps.ReplayCompatible() {
			t.Errorf("%s: must NOT be replay-compatible", name)
		}
	}

	var nilCaps *ContextCompactionCaps
	if nilCaps.ReplayCompatible() {
		t.Error("a nil capability block must not be replay-compatible")
	}
}

// nil (the deployment predates the feature) and enabled:false (it advertises the
// contract with no compactor wired — every real deployment today) are different
// answers, and the decoder must keep them apart even though the CLI withholds
// compaction for both.
func TestCapabilities_ContextCompactionAbsentVersusDisabled(t *testing.T) {
	var absent Capabilities
	if err := json.Unmarshal([]byte(`{"server_version":"1"}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.ContextCompaction != nil {
		t.Error("an older backend must decode to a nil block")
	}

	var disabled Capabilities
	if err := json.Unmarshal([]byte(`{"context_compaction":{"enabled":false,"stream_event":"compaction"}}`), &disabled); err != nil {
		t.Fatal(err)
	}
	if disabled.ContextCompaction == nil {
		t.Fatal("an advertised block must decode non-nil even when disabled")
	}
	if disabled.ContextCompaction.Enabled || disabled.ContextCompaction.ReplayCompatible() {
		t.Error("enabled:false must not open the gate")
	}
}

// The block is served at the TOP level, a sibling of `respond` — not inside it. A
// client that looked in the wrong place would find nil forever and the feature would
// never switch on, with no error anywhere to explain why.
func TestCapabilities_ContextCompactionDecodesFromTheTopLevel(t *testing.T) {
	body := `{
	  "respond": {"stream_events": ["meta","status","delta","compaction","done","error"]},
	  "context_compaction": {
	    "enabled": true, "stream_event": "compaction", "delivery": "before_done",
	    "at_most_once": true, "streaming_only": true, "best_effort": true,
	    "append_only": true, "block_message_name": "daintree_compaction",
	    "span": {"collection":"input.messages","index_base":0,"end_exclusive":true,"excludes_current_reply":true},
	    "turn_id_match_required": true, "max_block_content_bytes": 65536
	  }
	}`
	var caps Capabilities
	if err := json.Unmarshal([]byte(body), &caps); err != nil {
		t.Fatal(err)
	}
	if !caps.ContextCompaction.ReplayCompatible() {
		t.Fatalf("the served shape must be replay-compatible, got %+v", caps.ContextCompaction)
	}
	var sawCompaction bool
	for _, ev := range caps.Respond.StreamEvents {
		if ev == "compaction" {
			sawCompaction = true
		}
	}
	if !sawCompaction {
		t.Error("respond.stream_events must advertise the compaction event")
	}
}
