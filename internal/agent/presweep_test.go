package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// toolMsg builds a tool-role message with the given tool-call id and serialized body.
func toolMsg(id, content string) models.ChatMessage {
	return models.ChatMessage{Role: "tool", ToolCallID: id, StringContent: content}
}

// withControls prepends ControlMessageCount placeholder control messages so the working
// slice the sweep operates on (msgs[ControlMessageCount:]) lines up with production.
func withControls(work ...models.ChatMessage) []models.ChatMessage {
	msgs := make([]models.ChatMessage, 0, domain.ControlMessageCount+len(work))
	for i := 0; i < domain.ControlMessageCount; i++ {
		msgs = append(msgs, models.TextMessage("system", "control"))
	}
	return append(msgs, work...)
}

// stubBody marshals an overflow truncation stub with the given fields — the exact shape
// SerializeToolResult emits, so the sweep's decode/collapse path is exercised faithfully.
func stubBody(t *testing.T, artifactID, preview string, totalChars int) string {
	t.Helper()
	stub := truncationStub{
		Ok:      true,
		Summary: "tool ran",
		Result: truncationResult{
			Truncated:  true,
			ArtifactID: artifactID,
			TotalChars: totalChars,
			TotalBytes: totalChars,
			Preview:    preview,
			Note:       "Output truncated to a preview; call artifact.read to page the full result.",
		},
	}
	b, err := json.Marshal(stub)
	if err != nil {
		t.Fatalf("marshal stub: %v", err)
	}
	return string(b)
}

func decodeStub(t *testing.T, body string) truncationStub {
	t.Helper()
	var stub truncationStub
	if err := json.Unmarshal([]byte(body), &stub); err != nil {
		t.Fatalf("decode stub %q: %v", body, err)
	}
	return stub
}

// --- dedup ---

func TestRunPreSweepDedupKeepsMostRecent(t *testing.T) {
	big := "RESULT" + strings.Repeat("x", 4000)
	msgs := withControls(
		toolMsg("call_1", big),
		toolMsg("call_2", big),
		toolMsg("call_3", big),
	)

	n := runPreSweep(msgs)
	if n != 2 {
		t.Fatalf("modified = %d, want 2 (two earlier copies collapsed)", n)
	}
	work := msgs[domain.ControlMessageCount:]
	wantRef := "[duplicate of call_3]"
	if work[0].StringContent != wantRef || work[1].StringContent != wantRef {
		t.Fatalf("earlier copies = %q, %q; want both %q", work[0].StringContent, work[1].StringContent, wantRef)
	}
	if work[2].StringContent != big {
		t.Fatal("most-recent copy must retain its verbatim body")
	}
	// ToolCallIDs are preserved (the wire pairing must stay valid).
	if work[0].ToolCallID != "call_1" || work[2].ToolCallID != "call_3" {
		t.Fatalf("ToolCallIDs altered: %q ... %q", work[0].ToolCallID, work[2].ToolCallID)
	}

	// Idempotent: a second sweep over the already-collapsed history is a no-op.
	if again := runPreSweep(msgs); again != 0 {
		t.Fatalf("second sweep modified = %d, want 0 (idempotent)", again)
	}
}

func TestRunPreSweepDedupLeavesUniqueResults(t *testing.T) {
	bodyA := "alpha" + strings.Repeat("x", 4000)
	bodyB := "beta" + strings.Repeat("y", 4000)
	msgs := withControls(toolMsg("call_1", bodyA), toolMsg("call_2", bodyB))
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (all bodies distinct)", n)
	}
	// A no-op count must mean a literal no-op: bodies AND ToolCallIDs untouched.
	work := msgs[domain.ControlMessageCount:]
	if work[0].StringContent != bodyA || work[1].StringContent != bodyB {
		t.Fatal("distinct bodies must be left byte-for-byte unchanged")
	}
	if work[0].ToolCallID != "call_1" || work[1].ToolCallID != "call_2" {
		t.Fatal("ToolCallIDs must be left unchanged")
	}
}

// TestRunPreSweepDedupMultipleDistinctClasses proves the survivor map keys per distinct
// body: two interleaved duplicate classes each keep their OWN most-recent survivor and
// never cross-contaminate references.
func TestRunPreSweepDedupMultipleDistinctClasses(t *testing.T) {
	bodyA := "AAAA" + strings.Repeat("a", 4000)
	bodyB := "BBBB" + strings.Repeat("b", 4000)
	msgs := withControls(
		toolMsg("a1", bodyA),
		toolMsg("b1", bodyB),
		toolMsg("a2", bodyA), // survivor of class A
		toolMsg("b2", bodyB), // survivor of class B
	)
	if n := runPreSweep(msgs); n != 2 {
		t.Fatalf("modified = %d, want 2 (one earlier copy per class)", n)
	}
	work := msgs[domain.ControlMessageCount:]
	if work[0].StringContent != "[duplicate of a2]" {
		t.Fatalf("class-A earlier copy = %q, want ref to a2", work[0].StringContent)
	}
	if work[1].StringContent != "[duplicate of b2]" {
		t.Fatalf("class-B earlier copy = %q, want ref to b2", work[1].StringContent)
	}
	if work[2].StringContent != bodyA || work[3].StringContent != bodyB {
		t.Fatal("each class must retain its own most-recent survivor verbatim")
	}
}

func TestRunPreSweepDedupSkipsShortBodies(t *testing.T) {
	// Two identical tiny bodies: the "[duplicate of call_2]" ref is LONGER than "ok", so
	// the reductive guard leaves both intact rather than growing context.
	msgs := withControls(toolMsg("call_1", "ok"), toolMsg("call_2", "ok"))
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (ref would not shrink the body)", n)
	}
	if msgs[domain.ControlMessageCount].StringContent != "ok" {
		t.Fatal("tiny duplicate body must be left verbatim")
	}
}

func TestRunPreSweepDedupSkipsEmptyToolCallID(t *testing.T) {
	big := strings.Repeat("z", 4000)
	msgs := withControls(toolMsg("", big), toolMsg("", big))
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (no ToolCallID anchor to reference)", n)
	}
}

func TestRunPreSweepIgnoresNonToolMessages(t *testing.T) {
	big := strings.Repeat("p", 4000)
	msgs := withControls(
		models.ChatMessage{Role: "assistant", StringContent: big},
		models.ChatMessage{Role: "user", StringContent: big},
	)
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (non-tool roles are never swept)", n)
	}
}

func TestRunPreSweepSkipsControlMessages(t *testing.T) {
	// With no working message after the control prefix there is nothing to sweep — the
	// `len(msgs) <= ControlMessageCount` guard returns 0 before any pass. The backend now
	// owns the system prefix (ControlMessageCount == 0), so a history of exactly the
	// control prefix is empty; the guard is verified forward-compatibly against whatever
	// ControlMessageCount is, plus the empty-slice case.
	if n := runPreSweep(nil); n != 0 {
		t.Fatalf("modified = %d, want 0 (empty history)", n)
	}
	controlsOnly := withControls() // exactly ControlMessageCount control messages, none working
	before := append([]models.ChatMessage(nil), controlsOnly...)
	if n := runPreSweep(controlsOnly); n != 0 {
		t.Fatalf("modified = %d, want 0 (control region is off-limits)", n)
	}
	for i := range controlsOnly {
		if controlsOnly[i].StringContent != before[i].StringContent {
			t.Fatal("control-region messages must never be rewritten")
		}
	}
}

// --- stub collapse ---

func TestRunPreSweepCollapsesStubPreview(t *testing.T) {
	preview := strings.Repeat("a", domain.TruncationPreviewChars)
	body := stubBody(t, "artifact_abc123", preview, 50_000)
	msgs := withControls(toolMsg("call_1", body))

	n := runPreSweep(msgs)
	if n != 1 {
		t.Fatalf("modified = %d, want 1 (one stub preview stripped)", n)
	}
	got := decodeStub(t, msgs[domain.ControlMessageCount].StringContent)
	if got.Result.Preview != "" {
		t.Fatalf("preview = %q, want empty after collapse", got.Result.Preview)
	}
	// Load-bearing fields survive intact.
	if got.Result.ArtifactID != "artifact_abc123" {
		t.Fatalf("artifactId = %q, want preserved", got.Result.ArtifactID)
	}
	if !got.Result.Truncated || got.Result.TotalChars != 50_000 || got.Result.TotalBytes != 50_000 {
		t.Fatalf("totals/flags altered: %+v", got.Result)
	}
	if !got.Ok || got.Summary != "tool ran" {
		t.Fatalf("outer envelope altered: ok=%v summary=%q", got.Ok, got.Summary)
	}
	if !strings.Contains(got.Result.Note, "artifact_abc123") {
		t.Fatalf("collapsed note must still name the artifactId, got %q", got.Result.Note)
	}
	if charLen(msgs[domain.ControlMessageCount].StringContent) >= charLen(body) {
		t.Fatal("collapsed stub must be strictly smaller than the original")
	}

	// Idempotent: re-running finds an empty Preview and leaves it alone.
	if again := runPreSweep(msgs); again != 0 {
		t.Fatalf("second sweep modified = %d, want 0 (idempotent)", again)
	}
}

func TestRunPreSweepCollapsePreservesErrorFields(t *testing.T) {
	// A failed-tool overflow stub carries ErrorCode + a Recoverable pointer alongside the
	// artifact. Collapsing the preview must preserve every load-bearing field; only the
	// preview (and its note) may change.
	recoverable := true
	stub := truncationStub{
		Ok:      false,
		Summary: "tool failed",
		Result: truncationResult{
			Truncated:   true,
			ArtifactID:  "artifact_err",
			ErrorCode:   "MCP_RATE_LIMITED",
			Recoverable: &recoverable,
			TotalChars:  42_000,
			TotalBytes:  42_000,
			Preview:     strings.Repeat("e", domain.TruncationPreviewChars),
			Note:        "truncated",
		},
	}
	raw, err := json.Marshal(stub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msgs := withControls(toolMsg("call_1", string(raw)))

	if n := runPreSweep(msgs); n != 1 {
		t.Fatalf("modified = %d, want 1", n)
	}
	got := decodeStub(t, msgs[domain.ControlMessageCount].StringContent)
	if got.Result.Preview != "" {
		t.Fatalf("preview = %q, want empty", got.Result.Preview)
	}
	if got.Result.ErrorCode != "MCP_RATE_LIMITED" {
		t.Fatalf("errorCode = %q, want preserved", got.Result.ErrorCode)
	}
	if got.Result.Recoverable == nil || *got.Result.Recoverable != true {
		t.Fatalf("recoverable = %v, want preserved (true)", got.Result.Recoverable)
	}
	if got.Result.ArtifactID != "artifact_err" || got.Result.TotalChars != 42_000 {
		t.Fatalf("artifactId/totals altered: %+v", got.Result)
	}
	if got.Ok || got.Summary != "tool failed" {
		t.Fatalf("outer envelope altered: ok=%v summary=%q", got.Ok, got.Summary)
	}
}

func TestRunPreSweepStubWithoutArtifactIDUntouched(t *testing.T) {
	// An overflow that could not be archived (no artifactId) must keep its preview — it
	// is the only remaining copy of the truncated content.
	body := stubBody(t, "", strings.Repeat("a", domain.TruncationPreviewChars), 50_000)
	msgs := withControls(toolMsg("call_1", body))
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (no artifact twin to fall back on)", n)
	}
	if msgs[domain.ControlMessageCount].StringContent != body {
		t.Fatal("artifact-less stub must be left verbatim")
	}
}

func TestRunPreSweepAlreadyCollapsedStubUntouched(t *testing.T) {
	body := stubBody(t, "artifact_abc123", "", 50_000) // Preview already empty
	msgs := withControls(toolMsg("call_1", body))
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (already collapsed)", n)
	}
}

func TestRunPreSweepNonStubToolUntouched(t *testing.T) {
	// A normal full-payload tool result (no "truncated":true token) is passed through.
	body := `{"ok":true,"summary":"done","result":{"items":[1,2,3]}}`
	msgs := withControls(toolMsg("call_1", body))
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (non-stub result)", n)
	}
	if msgs[domain.ControlMessageCount].StringContent != body {
		t.Fatal("full-payload result must be left verbatim")
	}
}

func TestRunPreSweepMalformedStubUntouched(t *testing.T) {
	// Carries the "truncated":true token (passes the cheap pre-filter) but is not valid
	// JSON, so the decode fails and the body is left alone.
	body := `garbage "truncated":true not json {`
	msgs := withControls(toolMsg("call_1", body))
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (unparseable body)", n)
	}
	if msgs[domain.ControlMessageCount].StringContent != body {
		t.Fatal("malformed body must be left verbatim")
	}
}

// --- combined / aggregate ---

func TestRunPreSweepCombinedCountsBothPasses(t *testing.T) {
	big := "DUP" + strings.Repeat("x", 4000)
	preview := strings.Repeat("a", domain.TruncationPreviewChars)
	msgs := withControls(
		toolMsg("call_1", big), // → ref (dedup)
		toolMsg("call_2", big), // survivor
		toolMsg("call_3", stubBody(t, "artifact_x", preview, 50_000)), // → preview stripped
	)
	n := runPreSweep(msgs)
	if n != 2 {
		t.Fatalf("modified = %d, want 2 (1 dedup + 1 stub collapse)", n)
	}
}

func TestRunPreSweepReducesEstimate(t *testing.T) {
	big := strings.Repeat("x", 20_000)
	preview := strings.Repeat("a", domain.TruncationPreviewChars)
	msgs := withControls(
		toolMsg("call_1", big),
		toolMsg("call_2", big),
		toolMsg("call_3", big),
		toolMsg("call_4", stubBody(t, "artifact_x", preview, 80_000)),
	)
	before := estimateMessagesTokens(msgs)
	if runPreSweep(msgs) == 0 {
		t.Fatal("expected the sweep to rewrite at least one message")
	}
	after := estimateMessagesTokens(msgs)
	if after >= before {
		t.Fatalf("estimate did not drop: before=%d after=%d", before, after)
	}
}

func TestRunPreSweepEmptyAndShortHistoryNoOp(t *testing.T) {
	if n := runPreSweep(nil); n != 0 {
		t.Fatalf("nil history modified = %d, want 0", n)
	}
	// Exactly ControlMessageCount messages: no working slice, nothing to do.
	controls := withControls()
	if n := runPreSweep(controls); n != 0 {
		t.Fatalf("controls-only history modified = %d, want 0", n)
	}
}
