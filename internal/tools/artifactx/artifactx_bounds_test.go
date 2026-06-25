package artifactx

import (
	"encoding/json"
	"testing"
)

// Finding 4: a negative limit on artifact.read previously made end = offset+limit <
// offset, so runes[offset:end] panicked with an invalid slice range. A limit below
// the floor (< 1) and a negative offset must still be rejected as INVALID_ARGS at
// decode (Zod limit.min(1), offset.min(0)).
func TestArtifactReadRejectsOutOfBoundsArgs(t *testing.T) {
	tool := Tools(Deps{})[0]
	for _, bad := range []string{
		`{"artifactId":"x","limit":-1}`,
		`{"artifactId":"x","limit":0}`,
		`{"artifactId":"x","offset":-5}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("out-of-bounds artifact.read args should be rejected: %s", bad)
		}
	}
	// Valid args still decode.
	if _, err := tool.Decode(json.RawMessage(`{"artifactId":"x","offset":0,"limit":100}`)); err != nil {
		t.Errorf("valid args should decode: %v", err)
	}
}

// A limit ABOVE the ceiling is NOT an error — the model routinely sets limit to the
// artifact's totalChars to "grab it all", and rejecting that wastes a tool round.
// Validate clamps it to maxReadChars and the canonical re-marshal carries the clamped
// value forward (so the handler reads at most one 3500-char page).
func TestArtifactReadClampsOverLimit(t *testing.T) {
	tool := Tools(Deps{})[0]
	canonical, err := tool.Decode(json.RawMessage(`{"artifactId":"x","limit":999999}`))
	if err != nil {
		t.Fatalf("an over-max limit should clamp, not reject: %v", err)
	}
	var got readArgs
	if err := json.Unmarshal(canonical, &got); err != nil {
		t.Fatalf("canonical args should decode: %v", err)
	}
	if got.Limit == nil || *got.Limit != maxReadChars {
		t.Errorf("over-max limit = %v, want clamped to %d", got.Limit, maxReadChars)
	}
}
