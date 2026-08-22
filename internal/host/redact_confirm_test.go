package host

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/redact"
)

// A confirm request is built straight from the dispatch arguments and never passes
// through the EventSink's source-side sanitization that ordinary tool:started events
// get. The structural summarizer only collapses values by shape and LENGTH, so a
// short secret used to cross the wire verbatim on approval:requested — the one event
// a human is asked to read and act on.
//
// These pin credential masking at the confirm boundary itself, so it cannot be
// reintroduced by a change that only looks at the structural pass.
func TestRedactArgsMasksCredentialShapes(t *testing.T) {
	cases := []struct {
		name   string
		args   string
		secret string
	}{
		{"sensitive json key", `{"password":"hunter2hunter2"}`, "hunter2hunter2"},
		{"token key", `{"token":"abc123abc123abc"}`, "abc123abc123abc"},
		{"api key", `{"api_key":"sk-live-9f8e7d6c5b4a"}`, "sk-live-9f8e7d6c5b4a"},
		{"env assignment", `{"command":"export TOKEN=s3cr3tvalue123"}`, "s3cr3tvalue123"},
		{"url userinfo", `{"remote":"https://user:p4ssw0rdxyz@example.com/r.git"}`, "p4ssw0rdxyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactArgs(tc.args)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("redactArgs leaked %q\n  in: %s\n out: %s", tc.secret, tc.args, got)
			}
		})
	}
}

// A secret registered at runtime (the MCP bearer, an API key resolved from config) is
// a CERTAINTY rather than a shape guess, and must be removed even when it sits under
// an innocuous key and is short enough to survive the length collapse.
func TestRedactArgsMasksRegisteredSecrets(t *testing.T) {
	t.Cleanup(redact.ResetSecretsForTest)
	redact.ResetSecretsForTest()
	const bearer = "dnt_live_7a91c3e5"
	redact.RegisterSecret(bearer)

	got := redactArgs(`{"note":"the value is ` + bearer + `"}`)
	if strings.Contains(got, bearer) {
		t.Fatalf("redactArgs leaked a registered secret: %s", got)
	}
}

// Redaction must not eat ordinary arguments — a summary that masks everything is as
// useless as one that masks nothing, because the human cannot tell what they approved.
func TestRedactArgsKeepsOrdinaryValues(t *testing.T) {
	got := redactArgs(`{"worktreeId":"wt_42","branch":"feature/native-host"}`)
	for _, want := range []string{"wt_42", "feature/native-host"} {
		if !strings.Contains(got, want) {
			t.Fatalf("redactArgs dropped an ordinary value %q: %s", want, got)
		}
	}
}
