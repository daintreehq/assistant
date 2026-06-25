package commands

import "testing"

// TestRegistryNamesUnique guards the single-source-of-truth registry against a
// duplicate command name (which would make the help/palette ambiguous).
func TestRegistryNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range COMMAND_REGISTRY {
		if seen[c.Name] {
			t.Fatalf("duplicate command name %q", c.Name)
		}
		seen[c.Name] = true
	}
	// Spot-check the canonical set is present.
	for _, want := range []string{"status", "inbox", "tools", "doctor", "quit", "help"} {
		if !seen[want] {
			t.Fatalf("registry missing %q", want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	cmd, arg, rest := parseCommand("/audit export json tool=fs.read")
	if cmd != "audit" {
		t.Fatalf("cmd = %q", cmd)
	}
	if arg != "export json tool=fs.read" {
		t.Fatalf("arg = %q", arg)
	}
	if len(rest) != 3 || rest[0] != "export" {
		t.Fatalf("rest = %v", rest)
	}
	if c, _, _ := parseCommand("/"); c != "" {
		t.Fatalf("empty slash should yield empty cmd, got %q", c)
	}
}

func TestCanonicalAliases(t *testing.T) {
	cases := map[string]string{"?": "help", "exit": "quit", "q": "quit", "status": "status"}
	for in, want := range cases {
		if got := canonical(in); got != want {
			t.Fatalf("canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHelpLinesPadded(t *testing.T) {
	lines := HelpLines()
	if len(lines) != len(COMMAND_REGISTRY) {
		t.Fatalf("help lines = %d, registry = %d", len(lines), len(COMMAND_REGISTRY))
	}
	// Every line's syntax column is padded to at least helpPad runes before the help.
	for _, l := range lines {
		if len([]rune(l)) < helpPad {
			t.Fatalf("help line shorter than pad: %q", l)
		}
	}
}

func TestParseAuditExportArgs(t *testing.T) {
	res := ParseAuditExportArgs([]string{"csv", "tool=fs.read", "n=5"})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if res.Format != "csv" {
		t.Fatalf("format = %q", res.Format)
	}
	if res.Filters.ToolName == nil || *res.Filters.ToolName != "fs.read" {
		t.Fatalf("tool filter not parsed")
	}
	if res.Filters.Limit == nil || *res.Filters.Limit != 5 {
		t.Fatalf("limit filter not parsed")
	}
	bad := ParseAuditExportArgs([]string{"json", "garbage"})
	if bad.Error == "" {
		t.Fatalf("expected error for unrecognized arg")
	}
}

func TestFormatDoctorMarks(t *testing.T) {
	out := FormatDoctor([]DoctorCheck{
		{Label: "deepseek key", OK: true, Detail: "present"},
		{Label: "mcp url", OK: false, Detail: "(unset)", Fix: "set DAINTREE_MCP_URL"},
	})
	if want := "✓ "; out[:len(want)] != want {
		t.Fatalf("first line not a ✓ mark: %q", out)
	}
	if !contains(out, "✗ ") || !contains(out, "→ set DAINTREE_MCP_URL") {
		t.Fatalf("missing ✗ row or fix arrow: %q", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
