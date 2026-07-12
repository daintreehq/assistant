package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// TestValidateCheckpointReinjectsMissingIDs is the issue's core guarantee: an ID present
// in the discarded transcript but absent from the model's checkpoint is re-injected into
// PreservedIDs so it is never silently lost.
func TestValidateCheckpointReinjectsMissingIDs(t *testing.T) {
	transcript := `assistant: [tool call terminal.read {"terminalId":"term_a1b2"}]
assistant: spawned wch_12345678 and grt_cafef00d and agt_00112233 and tmr_99887766 and wfr_deadbeef`
	cp := backend.CheckpointOutput{Goal: "do the thing"}
	validateCheckpoint(&cp, transcript)
	for _, want := range []string{"term_a1b2", "wch_12345678", "grt_cafef00d", "agt_00112233", "tmr_99887766", "wfr_deadbeef"} {
		found := false
		for _, got := range cp.PreservedIDs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ID %q not preserved; PreservedIDs = %+v", want, cp.PreservedIDs)
		}
	}
}

// TestValidateCheckpointPreservesTerminalUUIDs pins the real Daintree MCP terminal-id
// shape (terminal-<uuid>): a full UUID id dropped by the model is re-injected, while a
// model-truncated prefix (terminal-5284bfef — matches no terminal) is NOT treated as a
// preservable ID of its own.
func TestValidateCheckpointPreservesTerminalUUIDs(t *testing.T) {
	transcript := `assistant: [tool call terminal.read {"terminalId":"terminal-5284bfef-3d11-424c-90cb-136f24046295"}]
assistant: also mentioned the truncated terminal-5284bfef by mistake`
	var cp backend.CheckpointOutput
	validateCheckpoint(&cp, transcript)
	if len(cp.PreservedIDs) != 1 || cp.PreservedIDs[0] != "terminal-5284bfef-3d11-424c-90cb-136f24046295" {
		t.Fatalf("want exactly the full terminal UUID preserved, got %+v", cp.PreservedIDs)
	}
}

// TestValidateCheckpointPrefixCollision is the regression guard for the substring bug:
// a short ID (term_1) must NOT be judged "already present" merely because a longer ID
// (term_10) the model captured contains it as a prefix. Daintree-MCP terminal IDs are
// short and numeric, so term_1 vs term_10 is a real-world collision.
func TestValidateCheckpointPrefixCollision(t *testing.T) {
	transcript := "ran on term_1, then later on term_10"
	cp := backend.CheckpointOutput{ActiveTerminals: []string{"term_10 — running tests"}}
	validateCheckpoint(&cp, transcript)
	// term_10 is placed → not re-injected; term_1 is genuinely absent → must be preserved.
	if len(cp.PreservedIDs) != 1 || cp.PreservedIDs[0] != "term_1" {
		t.Fatalf("term_1 must be preserved despite term_10 being present, got %+v", cp.PreservedIDs)
	}
}

// TestValidateCheckpointKeepsGrantInNamedField pins the P0 approvals/grants behaviour: a
// grant token the model correctly placed in approvals_grants is recognized as preserved
// (not duplicated into PreservedIDs).
func TestValidateCheckpointKeepsGrantInNamedField(t *testing.T) {
	transcript := `granted grt_cafef00d for git ops`
	cp := backend.CheckpointOutput{ApprovalsGrants: []string{"grt_cafef00d — git ops"}}
	validateCheckpoint(&cp, transcript)
	if len(cp.PreservedIDs) != 0 {
		t.Fatalf("a grant placed in approvals_grants must not be re-injected, got %+v", cp.PreservedIDs)
	}
}

// TestValidateCheckpointKeepsPlacedIDs proves an ID the model already placed in a named
// field is NOT duplicated into PreservedIDs.
func TestValidateCheckpointKeepsPlacedIDs(t *testing.T) {
	transcript := `assistant: [tool call terminal.read {"terminalId":"term_a1b2"}]`
	cp := backend.CheckpointOutput{ActiveTerminals: []string{"term_a1b2 — running tests"}}
	validateCheckpoint(&cp, transcript)
	if len(cp.PreservedIDs) != 0 {
		t.Fatalf("an already-placed ID must not be re-injected; PreservedIDs = %+v", cp.PreservedIDs)
	}
}

// TestValidateCheckpointNoIDsNoChange proves a transcript with no load-bearing IDs leaves
// the checkpoint untouched.
func TestValidateCheckpointNoIDsNoChange(t *testing.T) {
	cp := backend.CheckpointOutput{Goal: "g"}
	validateCheckpoint(&cp, "user: hello\nassistant: nothing to see here")
	if cp.PreservedIDs != nil {
		t.Fatalf("no IDs in transcript should leave PreservedIDs nil, got %+v", cp.PreservedIDs)
	}
}

// TestValidateCheckpointDedupes proves a repeated ID is preserved exactly once.
func TestValidateCheckpointDedupes(t *testing.T) {
	transcript := "term_a1b2 ... term_a1b2 ... term_a1b2"
	var cp backend.CheckpointOutput
	validateCheckpoint(&cp, transcript)
	if len(cp.PreservedIDs) != 1 || cp.PreservedIDs[0] != "term_a1b2" {
		t.Fatalf("repeated ID should preserve once, got %+v", cp.PreservedIDs)
	}
}

// TestFlattenTranscript pins the SHARED flattener both compaction paths feed the
// backend's transcript tasks: tool-call names + argument JSON folded in (argument-only
// IDs must reach the ID-preservation pass), tool results prefixed, system rows
// (deliberately) skipped, empty tool-call rows stubbed.
func TestFlattenTranscript(t *testing.T) {
	call := models.TextMessage("assistant", "spawning now")
	call.ToolCalls = []models.ToolCallRequest{{
		Type: "function",
		Function: models.ToolCallFunction{
			Name:      "terminal.read",
			Arguments: `{"terminalId":"terminal-5284bfef-3d11-424c-90cb-136f24046295"}`,
		},
	}}
	msgs := []models.ChatMessage{
		models.TextMessage("system", "CONTROL_ROW"),
		models.TextMessage("user", "please read the terminal"),
		call,
		models.TextMessage("tool", "tail contents"),
		{Role: "tool"}, // empty tool result → stubbed
	}
	got := FlattenTranscript(msgs)
	for _, want := range []string{
		"user: please read the terminal",
		"[tool call terminal.read {\"terminalId\":\"terminal-5284bfef-3d11-424c-90cb-136f24046295\"}]",
		"tool: [tool result] tail contents",
		"tool: [tool result]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("flattened transcript missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "CONTROL_ROW") {
		t.Fatalf("system rows must be skipped:\n%s", got)
	}
}

// TestAppendTranscriptBreadcrumb pins the compaction escape hatch: with an artifact id
// the note gains a pageable artifact.read pointer; without one the summary is unchanged.
func TestAppendTranscriptBreadcrumb(t *testing.T) {
	if got := AppendTranscriptBreadcrumb("SUMMARY", ""); got != "SUMMARY" {
		t.Fatalf("empty id must leave the summary unchanged, got %q", got)
	}
	got := AppendTranscriptBreadcrumb("SUMMARY", "artifact_ab12cd34")
	if !strings.Contains(got, "SUMMARY") ||
		!strings.Contains(got, "artifact_ab12cd34") ||
		!strings.Contains(got, `artifact.read {"artifactId":"artifact_ab12cd34"}`) {
		t.Fatalf("breadcrumb missing pieces: %q", got)
	}
}

// TestRenderCheckpointEmptyIsBraces proves an empty checkpoint renders to a non-empty,
// valid JSON object — never the empty string, which the caller would treat as a failed
// compaction. The typed backend.CheckpointOutput has no omitempty tags, so an empty
// value marshals to an object with explicit null/"" fields rather than a bare "{}"; the
// load-bearing contract is only "non-empty, parseable JSON the model can read back".
func TestRenderCheckpointEmptyIsBraces(t *testing.T) {
	got := renderCheckpoint(backend.CheckpointOutput{})
	if strings.TrimSpace(got) == "" {
		t.Fatal("empty checkpoint render must never be the empty string")
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("empty checkpoint render must be valid JSON, got %q", got)
	}
	if first := strings.TrimSpace(got); first == "" || first[0] != '{' {
		t.Fatalf("empty checkpoint render must be a JSON object, got %q", got)
	}
}

// TestRenderCheckpointContainsFields proves the rendered JSON carries the populated fields
// (so the model can read its own state back on rehydration).
func TestRenderCheckpointContainsFields(t *testing.T) {
	cp := backend.CheckpointOutput{
		Goal:           "ship",
		UserDirectives: []string{"never close terminals uninvited"},
		PreservedIDs:   []string{"term_a1b2"},
	}
	got := renderCheckpoint(cp)
	if !strings.Contains(got, "ship") || !strings.Contains(got, "term_a1b2") {
		t.Fatalf("render dropped fields: %q", got)
	}
	// user_directives is load-bearing (a dropped directive means the assistant starts
	// violating an explicit user instruction after compaction) — pin tag + render.
	if !strings.Contains(got, "user_directives") ||
		!strings.Contains(got, "never close terminals uninvited") {
		t.Fatalf("render dropped user_directives: %q", got)
	}
	if !strings.Contains(got, "\n") {
		t.Fatalf("render should be indented (pretty) JSON: %q", got)
	}
}

// TestBuildCheckpointParsesReply proves a well-formed model reply yields a populated
// checkpoint and no error.
func TestBuildCheckpointParsesReply(t *testing.T) {
	r := &jsonChatRouter{content: `{"goal":"ship it","next_actions":["open PR"]}`}
	cp, err := BuildCheckpoint(context.Background(), backendFromRouter{r: r}, "transcript with no ids")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp.Goal != "ship it" || len(cp.NextActions) != 1 {
		t.Fatalf("checkpoint not parsed: %+v", cp)
	}
}

// TestBuildCheckpointModelErrorExtractsIDs proves a model OUTAGE still preserves IDs from
// the transcript (and surfaces the error so the caller can run its bounded-growth
// fallback): the checkpoint is sparse but never loses a load-bearing identifier.
func TestBuildCheckpointModelErrorExtractsIDs(t *testing.T) {
	r := &errChatRouter{err: errors.New("model down")}
	cp, err := BuildCheckpoint(context.Background(), backendFromRouter{r: r}, "using term_a1b2 now")
	if err == nil {
		t.Fatal("a model error must be surfaced to the caller")
	}
	if len(cp.PreservedIDs) != 1 || cp.PreservedIDs[0] != "term_a1b2" {
		t.Fatalf("IDs must be mined even on model error, got %+v", cp.PreservedIDs)
	}
}

// TestBuildCheckpointProseReplyExtractsIDs proves the key improvement: when the model
// replies (no error) but returns PROSE instead of JSON, we still compact — the parse
// degrades to a zero value and validateCheckpoint mines the IDs into PreservedIDs.
func TestBuildCheckpointProseReplyExtractsIDs(t *testing.T) {
	r := &jsonChatRouter{content: "Here is a prose summary; we used term_a1b2."}
	cp, err := BuildCheckpoint(context.Background(), backendFromRouter{r: r}, "we used term_a1b2.")
	if err != nil {
		t.Fatalf("a prose reply is not an error: %v", err)
	}
	if cp.Goal != "" {
		t.Fatalf("prose must not populate goal, got %q", cp.Goal)
	}
	if len(cp.PreservedIDs) != 1 || cp.PreservedIDs[0] != "term_a1b2" {
		t.Fatalf("prose reply should still preserve IDs, got %+v", cp.PreservedIDs)
	}
}
