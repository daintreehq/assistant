package agenttaskx

import (
	"encoding/json"
	"testing"
)

// Finding 1: agentTask.spawnForEdits advertises required title/taskPrompt and a
// mode enum (edit|explore) that strict decoding didn't enforce — an empty title
// or taskPrompt would reach spawn with a blank prompt, and an arbitrary mode would
// silently become edit-mode. Validate() must reject all of these as INVALID_ARGS.
func TestSpawnForEditsRejectsRequiredAndEnumGaps(t *testing.T) {
	tool := newSpawnForEditsTool(Deps{})
	for name, bad := range map[string]string{
		"missing title":      `{"taskPrompt":"do the thing"}`,
		"empty title":        `{"title":"","taskPrompt":"do the thing"}`,
		"blank title":        `{"title":"   ","taskPrompt":"do the thing"}`,
		"missing taskPrompt": `{"title":"T"}`,
		"empty taskPrompt":   `{"title":"T","taskPrompt":""}`,
		"blank taskPrompt":   `{"title":"T","taskPrompt":"  "}`,
		"bad mode":           `{"title":"T","taskPrompt":"p","mode":"refactor"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("%s should be rejected: %s", name, bad)
		}
	}
	// Valid edit/explore/default-mode calls still decode.
	for _, good := range []string{
		`{"title":"T","taskPrompt":"p"}`,
		`{"title":"T","taskPrompt":"p","mode":"edit"}`,
		`{"title":"T","taskPrompt":"p","mode":"explore"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(good)); err != nil {
			t.Errorf("valid spawn args should decode: %s — %v", good, err)
		}
	}
}
