package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/redact"
)

// Audit rows are the most DURABLE copy of a tool call in the system: they outlive the
// conversation, they are queryable, and `audit.export` hands them back to the model as
// JSON or CSV — so a credential in an argument would be re-read into a later prompt.
// Redacting in safeJSON covers both the args and the result column, since both go
// through it.
func TestSafeJSONRedactsCredentialsBeforeTheyArePersisted(t *testing.T) {
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	const key = "sk-or-v1-auditplantedkey0123456789abc"
	const opaque = "an-opaque-mcp-token-matching-no-pattern"
	redact.RegisterSecret(opaque)

	got := safeJSON(map[string]any{
		"terminalId": "terminal-2b9f4c8e",
		"command":    "export OPENROUTER_KEY=" + key,
		// Under a NEUTRAL key and with no recognisable shape, so only RegisterSecret can
		// catch it. Placed under "Authorization" originally, this passed whether or not
		// exact registration worked — the key matcher was doing all the work.
		"note": "connecting with " + opaque,
	})

	if strings.Contains(got, key) {
		t.Errorf("a shape-matched key reached the audit row: %s", got)
	}
	if strings.Contains(got, opaque) {
		t.Errorf("a registered secret reached the audit row: %s", got)
	}
	// The row still has to be worth keeping.
	if !strings.Contains(got, "terminal-2b9f4c8e") {
		t.Errorf("redaction destroyed the identifier the row exists to record: %s", got)
	}
	// And it must still be valid JSON, or audit.export produces garbage.
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Errorf("redacted audit JSON no longer parses: %v\n%s", err, got)
	}
}

// safeJSON must never throw — the audit path is a side-channel that must not be able to
// break a tool call.
func TestSafeJSONNeverThrowsOnUnserializableInput(t *testing.T) {
	got := safeJSON(map[string]any{"fn": func() {}})
	if got != `"<unserializable>"` {
		t.Errorf("want the unserializable sentinel, got %s", got)
	}
}
