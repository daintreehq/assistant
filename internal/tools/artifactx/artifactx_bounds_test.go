package artifactx

import (
	"encoding/json"
	"testing"
)

// Finding 4: a negative limit on artifact.read previously made end = offset+limit <
// offset, so runes[offset:end] panicked with an invalid slice range. It must be
// rejected as INVALID_ARGS at decode (Zod limit.min(1)), and offset must be min(0).
func TestArtifactReadRejectsOutOfBoundsArgs(t *testing.T) {
	tool := Tools(Deps{})[0]
	for _, bad := range []string{
		`{"artifactId":"x","limit":-1}`,
		`{"artifactId":"x","limit":0}`,
		`{"artifactId":"x","limit":999999}`,
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
