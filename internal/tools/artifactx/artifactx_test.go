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

func TestArtifactPagingSummaryFrontLoadsProgress(t *testing.T) {
	// Issue #312: the cockpit truncates an activity row's detail from the TAIL, so
	// the per-page offset/remaining must live in the head of the summary or every
	// page of a paged read renders identically. Pin the exact strings — the whole
	// point of the format is WHERE each field sits, which a looser assertion misses.
	const total = 56394
	deps := Deps{Store: fakeStore{"artifact_1091e529": strings.Repeat("a", total)}}

	cases := []struct {
		args string
		want string
	}{
		{`{"artifactId":"artifact_1091e529"}`,
			"offset 0: 3500/56394 chars, 52894 remaining — artifact_1091e529"},
		{`{"artifactId":"artifact_1091e529","offset":3500}`,
			"offset 3500: 3500/56394 chars, 49394 remaining — artifact_1091e529"},
		{`{"artifactId":"artifact_1091e529","offset":7000}`,
			"offset 7000: 3500/56394 chars, 45894 remaining — artifact_1091e529"},
		{`{"artifactId":"artifact_1091e529","offset":10500}`,
			"offset 10500: 3500/56394 chars, 42394 remaining — artifact_1091e529"},
		// Final partial page: the count shrinks and the tail says eof, not "0 remaining".
		{`{"artifactId":"artifact_1091e529","offset":56000}`,
			"offset 56000: 394/56394 chars, end of artifact — artifact_1091e529"},
	}
	for _, c := range cases {
		res := handle(deps, json.RawMessage(c.args))
		if !res.Ok {
			t.Fatalf("%s: expected ok, got %+v", c.args, res.Error)
		}
		if res.Summary != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.args, res.Summary, c.want)
		}
	}
}

func TestArtifactSummaryKeepsFullID(t *testing.T) {
	// The summary is model-visible (agent/serialize.go feeds it back with the result),
	// so the id must stay in its full, callable form — a bare hex suffix would invite
	// the model to echo an unusable id, the way a truncated terminal id once stranded
	// an awaitAll.
	deps := Deps{Store: fakeStore{"artifact_1091e529": strings.Repeat("a", 10)}}
	res := handle(deps, json.RawMessage(`{"artifactId":"artifact_1091e529"}`))
	if !strings.Contains(res.Summary, "artifact_1091e529") {
		t.Errorf("summary must carry the full artifact id, got %q", res.Summary)
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
