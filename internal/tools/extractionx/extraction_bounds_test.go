package extractionx

import (
	"encoding/json"
	"testing"
)

// Finding 4: the shared extract/poll numeric knobs must enforce their Zod bounds
// so a negative maxAttempts can't degenerate the poll cap and an oversized
// tailBytes/maxTokens/pollIntervalMs can't be silently honored. Validate() is
// promoted onto the extract tool's args via the embedded baseArgs.
func TestExtractRejectsOutOfBoundsNumerics(t *testing.T) {
	tool := newExtractTool(Deps{})
	for _, bad := range []string{
		`{"terminalIds":["t"],"maxAttempts":0}`,
		`{"terminalIds":["t"],"maxAttempts":-1}`,
		`{"terminalIds":["t"],"maxAttempts":121}`,
		`{"terminalIds":["t"],"tailBytes":0}`,
		`{"terminalIds":["t"],"tailBytes":-5}`,
		`{"terminalIds":["t"],"tailBytes":100001}`,
		`{"terminalIds":["t"],"maxTokens":0}`,
		`{"terminalIds":["t"],"maxTokens":2001}`,
		`{"terminalIds":["t"],"pollIntervalMs":-1}`,
		`{"terminalIds":["t"],"pollIntervalMs":60001}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("out-of-bounds extract numeric should be rejected: %s", bad)
		}
	}
	// In-range values still decode.
	if _, err := tool.Decode(json.RawMessage(
		`{"terminalIds":["t"],"maxAttempts":30,"tailBytes":12000,"maxTokens":1024,"pollIntervalMs":2000}`)); err != nil {
		t.Errorf("valid extract numerics should decode: %v", err)
	}
}
