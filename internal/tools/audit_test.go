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
//
// The structural walker also made this STRICTLY better: an unserializable member used to
// collapse the whole record to "<unserializable>", losing every argument beside it. Now
// only the offending field is replaced, and it is replaced by its TYPE rather than by the
// pointer address %v would print — an address helps nobody and leaks a memory-layout
// detail into a durable row.
func TestSafeJSONHandlesUnserializableMembersWithoutLosingTheRecord(t *testing.T) {
	got := safeJSON(map[string]any{"fn": func() {}, "terminalId": "terminal-2b9f4c8e", "lines": 200})

	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("audit JSON must stay parseable: %v\n%s", err, got)
	}
	if back["terminalId"] != "terminal-2b9f4c8e" || back["lines"] != float64(200) {
		t.Errorf("serializable fields beside an unserializable one were lost: %s", got)
	}
	fn, _ := back["fn"].(string)
	if !strings.HasPrefix(fn, "<unserializable ") {
		t.Errorf("the unserializable member should be marked, got %q", fn)
	}
	if strings.Contains(got, "0x") {
		t.Errorf("a pointer address reached the audit row: %s", got)
	}
}

// A top-level value that cannot be marshaled at all still yields valid JSON.
func TestSafeJSONNeverThrows(t *testing.T) {
	got := safeJSON(func() {})
	var back any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Errorf("safeJSON produced unparseable output: %v (%s)", err, got)
	}
}
