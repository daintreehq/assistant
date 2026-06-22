package artifactx

import (
	"encoding/json"
	"strings"
	"testing"
)

type fakeStore map[string]string

func (f fakeStore) Get(id string) (string, bool) { v, ok := f[id]; return v, ok }

func readResult(t *testing.T, deps Deps, args string) map[string]any {
	t.Helper()
	res := handle(deps, json.RawMessage(args))
	if !res.Ok {
		t.Fatalf("expected ok, got fail: %+v", res.Error)
	}
	m, _ := res.Result.(map[string]any)
	return m
}

func TestArtifactUnavailableAndNotFound(t *testing.T) {
	// No store → ARTIFACT_UNAVAILABLE, unrecoverable.
	res := handle(Deps{}, json.RawMessage(`{"artifactId":"x"}`))
	if res.Ok || res.Error.Code != codeArtifactUnavailable || res.Error.Recoverable {
		t.Fatalf("want unrecoverable ARTIFACT_UNAVAILABLE, got %+v", res.Error)
	}
	// Missing id → ARTIFACT_NOT_FOUND.
	res = handle(Deps{Store: fakeStore{}}, json.RawMessage(`{"artifactId":"nope"}`))
	if res.Ok || res.Error.Code != codeArtifactNotFound {
		t.Fatalf("want ARTIFACT_NOT_FOUND, got %+v", res.Error)
	}
}

func TestArtifactPagingAndClamp(t *testing.T) {
	full := strings.Repeat("a", 5000)
	deps := Deps{Store: fakeStore{"art1": full}}

	// First page: default limit 3500.
	m := readResult(t, deps, `{"artifactId":"art1"}`)
	if m["totalChars"].(int) != 5000 {
		t.Errorf("totalChars = %v", m["totalChars"])
	}
	if len(m["content"].(string)) != 3500 {
		t.Errorf("first page len = %d, want 3500", len(m["content"].(string)))
	}
	if m["eof"].(bool) {
		t.Error("eof should be false on first page")
	}
	if m["nextOffset"].(int) != 3500 {
		t.Errorf("nextOffset = %v", m["nextOffset"])
	}

	// Second page from nextOffset.
	m = readResult(t, deps, `{"artifactId":"art1","offset":3500}`)
	if len(m["content"].(string)) != 1500 || !m["eof"].(bool) {
		t.Errorf("second page len=%d eof=%v", len(m["content"].(string)), m["eof"])
	}

	// Past-the-end offset clamps to total, empty content, eof.
	m = readResult(t, deps, `{"artifactId":"art1","offset":99999}`)
	if m["content"].(string) != "" || !m["eof"].(bool) || m["offset"].(int) != 5000 {
		t.Errorf("clamp failed: %+v", m)
	}

	// Limit above the ceiling clamps to 3500.
	m = readResult(t, deps, `{"artifactId":"art1","limit":9000}`)
	if m["limit"].(int) != maxReadChars {
		t.Errorf("limit not clamped: %v", m["limit"])
	}
}

func TestArtifactRuneSlicing(t *testing.T) {
	// Multi-byte runes: indices must be character-based, not byte-based.
	full := strings.Repeat("é", 10) // 10 runes, 20 bytes
	deps := Deps{Store: fakeStore{"u": full}}
	m := readResult(t, deps, `{"artifactId":"u","limit":4}`)
	if m["totalChars"].(int) != 10 {
		t.Errorf("totalChars = %v, want 10 (runes)", m["totalChars"])
	}
	if got := m["content"].(string); got != "éééé" {
		t.Errorf("content = %q, want éééé", got)
	}
}
