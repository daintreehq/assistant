package host

import "testing"

// The presentation table is the activity tree's whole legibility story, so these pin
// the three properties a host depends on: a known tool resolves to a verb, an unknown
// one resolves to NOTHING (so the host falls back to the raw id rather than showing an
// invented label), and the target comes out of the raw args in the documented shapes.

func TestPresentToolVerbKnownAndUnknown(t *testing.T) {
	if got, _ := presentToolVerb("fs.read"); got != "Read" {
		t.Fatalf("fs.read verb = %q, want Read", got)
	}
	// Empty, NOT the tool name: BatchedCall.Verb being empty is the signal the host
	// keys off to render the raw id. Falling back to the name here would make every
	// unknown tool look like a known one.
	if got, keys := presentToolVerb("totally.unknown"); got != "" || keys != nil {
		t.Fatalf("unknown tool = (%q, %v), want (\"\", nil)", got, keys)
	}
}

func TestPresentToolActiveVerbOnlyForBlockingTools(t *testing.T) {
	if got := presentToolActiveVerb("agentTask.spawnForEdits"); got != "Delegating" {
		t.Fatalf("active verb = %q, want Delegating", got)
	}
	// "" means "the settled label already reads correctly while it runs".
	if got := presentToolActiveVerb("fs.read"); got != "" {
		t.Fatalf("fs.read active verb = %q, want empty", got)
	}
}

func TestPresentToolTargetShapes(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"plain string arg", "fs.search", `{"query":"needle"}`, "needle"},
		{"literal mode", "context.snapshot", `{}`, "workspace context"},
		{"id array joins", "terminal.extract", `{"terminalIds":["a","b"]}`, "a, b"},
		{"numeric arg has no trailing zero", "forge.getPR", `{"prNumber":42}`, "42"},
		// The mcpwrap opaque-args wrappers carry NO top-level target: their whole
		// payload is one `arguments` object. Keying only on the top level rendered
		// every one of these blank.
		{"arguments envelope, string", "git.getProjectPulse", `{"arguments":{"worktreeId":"wt_9"}}`, "wt_9"},
		{"arguments envelope, number", "forge.getIssue", `{"arguments":{"issueNumber":42}}`, "42"},
		{"empty envelope has no target", "git.getProjectPulse", `{"arguments":{}}`, ""},
		{"omitted envelope has no target", "git.getProjectPulse", `{}`, ""},
		// project.runCheck's target is the runner it was told to run; the schema is
		// projectId/runnerId/cwd/timeoutMs and has no `command`, `name` or `id`.
		{"runCheck names its runner", "project.runCheck", `{"projectId":"p1","runnerId":"test"}`, "test"},
		{"missing key falls through to the next", "terminal.rename", `{"terminalId":"t1"}`, "t1"},
		{"no target keys", "watcher.list", `{"anything":1}`, ""},
		{"unknown tool", "totally.unknown", `{"path":"x"}`, ""},
		{"malformed args do not panic", "fs.search", `{not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := presentToolTarget(tc.tool, tc.args); got != tc.want {
				t.Fatalf("target = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPresentToolTargetIsBounded(t *testing.T) {
	long := ""
	for range 200 {
		long += "x"
	}
	got := presentToolTarget("fs.search", `{"query":"`+long+`"}`)
	if len([]rune(got)) > 49 { // 48 + the ellipsis
		t.Fatalf("target not bounded: %d runes", len([]rune(got)))
	}
}
