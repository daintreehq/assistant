package ui

import (
	"github.com/daintreehq/assistant/internal/redact"
	"strings"
	"testing"
)

// redact_test.go locks the display-redaction pass: credential values are masked before
// args render in the approval sheet / ^X expanded row, WITHOUT hiding load-bearing
// approval detail (commands, push targets, paths).

func TestRedactArgs_MasksTokenInShellCommand(t *testing.T) {
	in := `{"input":"export ANTHROPIC_API_KEY=sk-ant-abc123DEF456ghi789jkl"}`
	out := redactArgs(in)
	if strings.Contains(out, "sk-ant-abc123DEF456ghi789jkl") {
		t.Fatalf("token leaked through redaction: %q", out)
	}
	if !strings.Contains(out, redact.Mark) {
		t.Fatalf("expected a redaction mark: %q", out)
	}
	// The command context survives so the user can still judge the action.
	if !strings.Contains(out, "export ANTHROPIC_API_KEY=") {
		t.Fatalf("the command context must survive redaction: %q", out)
	}
}

func TestRedactArgs_MasksSensitiveKeyValue(t *testing.T) {
	in := `{"password":"hunter2secret","remote":"origin","branch":"main"}`
	out := redactArgs(in)
	if strings.Contains(out, "hunter2secret") {
		t.Fatalf("password value leaked: %q", out)
	}
	if !strings.Contains(out, "origin") || !strings.Contains(out, "main") {
		t.Fatalf("benign load-bearing fields must survive: %q", out)
	}
}

func TestRedactArgs_PreservesLoadBearingArgs(t *testing.T) {
	// No secret key, no secret shape → must pass through byte-for-byte (never hide a
	// push target / command that the user needs to see to approve).
	in := `{"remote":"origin","branch":"feat/x","input":"git push origin HEAD"}`
	if got := redactArgs(in); got != in {
		t.Fatalf("non-secret args must pass through unchanged:\n in=%q\nout=%q", in, got)
	}
}

func TestRedactArgs_MasksAuthorizationBearer(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dQw4w9WgXcQabcdef0123"
	in := `{"headers":{"Authorization":"Bearer ` + jwt + `"}}`
	out := redactArgs(in)
	if strings.Contains(out, jwt) {
		t.Fatalf("bearer/JWT leaked: %q", out)
	}
}

func TestRedactArgs_SafeOnEmptyAndPlainText(t *testing.T) {
	if redactArgs("") != "" {
		t.Fatal("empty must stay empty")
	}
	if got := redactArgs("just a plain sentence"); got != "just a plain sentence" {
		t.Fatalf("plain text with no secret shape must pass through: %q", got)
	}
}

func TestArgsBlockRedactsSecrets(t *testing.T) {
	out := stripAnsi(argsBlock(`{"token":"ghp_abcdefghijklmnopqrstuvwxyz0123"}`, 80, 4))
	if strings.Contains(out, "ghp_abcdefghijklmnopqrstuvwxyz0123") {
		t.Fatalf("approval args leaked a token: %q", out)
	}
	if !strings.Contains(out, redact.Mark) {
		t.Fatalf("approval args should show the redaction mark: %q", out)
	}
}

func TestCompactArgsRedactsSecrets(t *testing.T) {
	out := compactArgs(`{"authorization":"Bearer sk-verylongsecrettoken1234567"}`, 200)
	if strings.Contains(out, "sk-verylongsecrettoken1234567") {
		t.Fatalf("expanded (^X) args leaked a token: %q", out)
	}
}
