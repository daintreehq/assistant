package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func TestSerializeSmallResultUntouched(t *testing.T) {
	res := domain.Ok("did the thing", map[string]any{"n": 1})
	s := SerializeToolResult(res, nil)
	var probe map[string]any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		t.Fatalf("small result not valid JSON: %v", err)
	}
	if probe["summary"] != "did the thing" {
		t.Fatalf("summary missing: %v", probe)
	}
}

func TestSerializeTruncatesToValidJSONWithArtifact(t *testing.T) {
	// Build a result whose serialization blows past the 8000-char cap.
	big := strings.Repeat("x", domain.MaxToolResultChars*2)
	res := domain.Ok("oversized", map[string]any{"blob": big})
	store := NewArtifactStore()

	s := SerializeToolResult(res, store)

	// Truncation stub MUST be valid JSON (issue #78 — never sliced mid-structure).
	var stub struct {
		Ok      bool   `json:"ok"`
		Summary string `json:"summary"`
		Result  struct {
			Truncated  bool   `json:"truncated"`
			ArtifactID string `json:"artifactId"`
			TotalChars int    `json:"totalChars"`
			TotalBytes int    `json:"totalBytes"`
			Preview    string `json:"preview"`
			Note       string `json:"note"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(s), &stub); err != nil {
		t.Fatalf("truncation stub not valid JSON: %v\n%s", err, s)
	}
	if !stub.Result.Truncated {
		t.Fatal("expected truncated:true")
	}
	if stub.Result.ArtifactID == "" {
		t.Fatal("expected an artifactId when a store is supplied")
	}
	if len([]rune(stub.Result.Preview)) != domain.TruncationPreviewChars {
		t.Fatalf("preview len = %d want %d", len([]rune(stub.Result.Preview)), domain.TruncationPreviewChars)
	}
	// The full result must be retrievable from the artifact store.
	full, ok := store.Get(stub.Result.ArtifactID)
	if !ok {
		t.Fatal("artifact not stored")
	}
	if len(full) != stub.Result.TotalBytes {
		t.Fatalf("artifact bytes %d != totalBytes %d", len(full), stub.Result.TotalBytes)
	}
}

func TestSerializeTruncatesWithoutStoreUnretrievable(t *testing.T) {
	big := strings.Repeat("y", domain.MaxToolResultChars*2)
	res := domain.Ok("oversized", map[string]any{"blob": big})
	s := SerializeToolResult(res, nil) // no store
	var stub map[string]any
	if err := json.Unmarshal([]byte(s), &stub); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	result := stub["result"].(map[string]any)
	if _, hasArtifact := result["artifactId"]; hasArtifact {
		t.Fatal("no artifactId expected without a store")
	}
	if !strings.Contains(result["note"].(string), "not retrievable") {
		t.Fatalf("expected unretrievable note, got %v", result["note"])
	}
}

func TestSerializeCapIsCharsNotBytes(t *testing.T) {
	// "の" is 3 bytes but 1 rune. A blob just under the CHAR cap whose BYTE length is
	// well over it must NOT truncate — the cap is in characters (utf8.RuneCount), not
	// bytes (len).
	runes := domain.MaxToolResultChars - 200 // leave headroom for the JSON envelope
	blob := strings.Repeat("の", runes)
	res := domain.Ok("multibyte", map[string]any{"blob": blob})

	s := SerializeToolResult(res, NewArtifactStore())

	// Byte length exceeds the cap (3x the runes); had the check used len(s) this
	// would have wrongly truncated.
	if len(s) <= domain.MaxToolResultChars {
		t.Fatalf("test setup: byte length %d should exceed the cap to exercise the bug", len(s))
	}
	if utf8.RuneCountInString(s) > domain.MaxToolResultChars {
		t.Fatalf("test setup: rune count %d should be within the cap", utf8.RuneCountInString(s))
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		t.Fatalf("result not valid JSON: %v", err)
	}
	if r, isStub := probe["result"].(map[string]any); isStub {
		if _, truncated := r["truncated"]; truncated {
			t.Fatal("multibyte result within the CHAR cap must NOT be truncated (cap is chars, not bytes)")
		}
	}
}

func TestSerializeTruncatesMultibyteOnRuneBoundary(t *testing.T) {
	// A multibyte blob that genuinely exceeds the CHAR cap must truncate, stay valid
	// JSON, and slice the preview on a rune boundary (never a split multibyte rune).
	blob := strings.Repeat("世界", domain.MaxToolResultChars) // ~2*cap runes
	res := domain.Ok("oversized multibyte", map[string]any{"blob": blob})
	store := NewArtifactStore()

	s := SerializeToolResult(res, store)

	if !utf8.ValidString(s) {
		t.Fatal("truncation stub must be valid UTF-8 (no split rune)")
	}
	var stub struct {
		Result struct {
			Truncated  bool   `json:"truncated"`
			TotalChars int    `json:"totalChars"`
			TotalBytes int    `json:"totalBytes"`
			Preview    string `json:"preview"`
			ArtifactID string `json:"artifactId"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(s), &stub); err != nil {
		t.Fatalf("truncation stub not valid JSON: %v", err)
	}
	if !stub.Result.Truncated {
		t.Fatal("expected truncated:true for an over-cap multibyte result")
	}
	if utf8.RuneCountInString(stub.Result.Preview) != domain.TruncationPreviewChars {
		t.Fatalf("preview rune count = %d want %d", utf8.RuneCountInString(stub.Result.Preview), domain.TruncationPreviewChars)
	}
	// totalChars (runes) and totalBytes (bytes) must differ for a multibyte blob, and
	// bytes must be the larger — proof they are tracked separately.
	if stub.Result.TotalBytes <= stub.Result.TotalChars {
		t.Fatalf("totalBytes %d should exceed totalChars %d for a multibyte result", stub.Result.TotalBytes, stub.Result.TotalChars)
	}
	full, ok := store.Get(stub.Result.ArtifactID)
	if !ok {
		t.Fatal("artifact not stored")
	}
	if len(full) != stub.Result.TotalBytes {
		t.Fatalf("artifact bytes %d != totalBytes %d", len(full), stub.Result.TotalBytes)
	}
	if utf8.RuneCountInString(full) != stub.Result.TotalChars {
		t.Fatalf("artifact runes %d != totalChars %d", utf8.RuneCountInString(full), stub.Result.TotalChars)
	}
}

func TestArtifactStoreEvictsOldest(t *testing.T) {
	store := NewArtifactStore()
	first := store.set("first")
	for i := 0; i < domain.MaxStoredArtifacts; i++ {
		store.set("filler")
	}
	if store.Len() != domain.MaxStoredArtifacts {
		t.Fatalf("store len = %d want %d (capped)", store.Len(), domain.MaxStoredArtifacts)
	}
	if _, ok := store.Get(first); ok {
		t.Fatal("oldest artifact should have been evicted")
	}
}
