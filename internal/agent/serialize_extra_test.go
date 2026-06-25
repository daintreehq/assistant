package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Additional serializeToolResult truncation edges not already covered by
// serialize_test.go.

func TestSerializeSmallPayloadNoStub(t *testing.T) {
	s := SerializeToolResult(domain.Ok("tiny", "hello"), nil)
	var parsed struct {
		Ok      bool   `json:"ok"`
		Summary string `json:"summary"`
		Result  any    `json:"result"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if !parsed.Ok || parsed.Summary != "tiny" || parsed.Result != "hello" {
		t.Fatalf("small payload mangled: %s", s)
	}
	if strings.Contains(s, "truncated") {
		t.Fatalf("small payload should not be a truncation stub: %s", s)
	}
}

func TestSerializeOversizedSummaryStaysWithinInlineLimit(t *testing.T) {
	// A huge summary with a tiny result still overflows; the stub must cap the
	// echoed summary to TruncationSummaryChars and stay under the inline limit.
	res := domain.Ok(strings.Repeat("S", 8000), "tiny")
	s := SerializeToolResult(res, nil)
	if len(s) >= domain.MaxToolResultChars {
		t.Fatalf("stub len %d should stay under inline limit %d", len(s), domain.MaxToolResultChars)
	}
	var stub truncationStub
	if err := json.Unmarshal([]byte(s), &stub); err != nil {
		t.Fatalf("stub not valid JSON: %v", err)
	}
	if !stub.Result.Truncated {
		t.Fatal("expected truncated:true")
	}
	if len([]rune(stub.Summary)) > domain.TruncationSummaryChars {
		t.Fatalf("summary not capped: %d runes > %d", len([]rune(stub.Summary)), domain.TruncationSummaryChars)
	}
}

func TestSerializeFailedOversizedSurfacesErrorClass(t *testing.T) {
	res := domain.Fail("DB_ERROR", "boom", domain.Unrecoverable())
	res.Result = strings.Repeat("x", 9000)
	s := SerializeToolResult(res, nil)
	var stub truncationStub
	if err := json.Unmarshal([]byte(s), &stub); err != nil {
		t.Fatalf("stub not valid JSON: %v", err)
	}
	if stub.Ok {
		t.Fatal("failed result must keep ok:false")
	}
	if stub.Result.ErrorCode != "DB_ERROR" {
		t.Fatalf("errorCode = %q want DB_ERROR", stub.Result.ErrorCode)
	}
	if stub.Result.Recoverable == nil || *stub.Result.Recoverable {
		t.Fatal("recoverable should be false")
	}
}

func TestSerializeRepagedSliceDoesNotReOverflow(t *testing.T) {
	// Escape-heavy content doubles under each JSON encode. Storing it, then re-paging
	// a slice back through SerializeToolResult must stay valid + inline — not produce
	// a second nested stub (escape amplification).
	store := NewArtifactStore("", nil)
	heavy := strings.Repeat("\\", 8000)
	stub := SerializeToolResult(domain.Ok("heavy", heavy), store)
	var first truncationStub
	if err := json.Unmarshal([]byte(stub), &first); err != nil {
		t.Fatalf("first stub not valid JSON: %v", err)
	}
	full, ok := store.Get(first.Result.ArtifactID)
	if !ok {
		t.Fatal("artifact not stored")
	}
	// Simulate artifact.read returning a bounded slice, then re-serializing it.
	slice := sliceChars(full, 3500)
	reSerialized := SerializeToolResult(domain.Ok("page", map[string]any{
		"artifactId": first.Result.ArtifactID,
		"content":    slice,
		"offset":     0,
		"eof":        false,
	}), nil)
	if len(reSerialized) > domain.MaxToolResultChars {
		t.Fatalf("re-paged slice re-overflowed: len %d", len(reSerialized))
	}
	var parsed struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(reSerialized), &parsed); err != nil {
		t.Fatalf("re-paged not valid JSON: %v", err)
	}
	if _, truncated := parsed.Result["truncated"]; truncated {
		t.Fatal("re-paged slice should stay inline, not become a nested truncation stub")
	}
	if parsed.Result["content"] != slice {
		t.Fatal("re-paged content should be the real slice")
	}
}
