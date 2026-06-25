package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
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
	msgs := withControls(
		toolMsg("call_1", "alpha"+strings.Repeat("x", 4000)),
		toolMsg("call_2", "beta"+strings.Repeat("y", 4000)),
	)
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (all bodies distinct)", n)
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
	// Identical "tool" bodies living entirely WITHIN the control region (no working
	// message after them) must be untouched — the guard returns before any pass.
	big := strings.Repeat("c", 4000)
	msgs := []models.ChatMessage{
		models.TextMessage("system", "control"),
		toolMsg("call_1", big),
		toolMsg("call_2", big),
	}
	if n := runPreSweep(msgs); n != 0 {
		t.Fatalf("modified = %d, want 0 (control region is off-limits)", n)
	}
	if msgs[1].StringContent != big || msgs[2].StringContent != big {
		t.Fatal("control-region messages must never be rewritten")
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
