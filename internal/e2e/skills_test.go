package e2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// skills_test.go drives `--list-skills` and `--skill` through the REAL binary against the
// fake backend. The unit tests prove each layer; this proves the argv surface, the route,
// the capability read and the wire field are actually connected to each other — which is
// where a feature threaded through five packages usually breaks.

// runSkillsCLI invokes the built binary against a fake backend with an isolated state dir.
func runSkillsCLI(t *testing.T, backendURL string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+backendURL,
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run binary: %v (stderr: %s)", err, stderr.String())
		}
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// The listing is what makes --skill usable at all: you cannot name an id you cannot
// enumerate. It must work with no project lease and no MCP.
func TestBinaryListSkills(t *testing.T) {
	be := newFakeBackend(t)

	stdout, stderr, code := runSkillsCLI(t, be.baseURL(), "--list-skills")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	for _, want := range []string{"daintree.foundation", "Multi-agent orchestration"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("listing is missing %q:\n%s", want, stdout)
		}
	}

	// --json must put ONE parseable document on stdout, because feeding ids into --skill
	// from a script is the whole reason the JSON form exists.
	stdout, stderr, code = runSkillsCLI(t, be.baseURL(), "--list-skills", "--json")
	if code != 0 {
		t.Fatalf("json exit = %d, want 0 (stderr %q)", code, stderr)
	}
	var doc struct {
		CatalogRevision string `json:"catalogRevision"`
		Skills          []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, stdout)
	}
	if len(doc.Skills) != 2 {
		t.Fatalf("skills = %+v, want the fake's two entries", doc.Skills)
	}
	if doc.CatalogRevision != "sha256:test" {
		t.Fatalf("catalogRevision = %q, want sha256:test", doc.CatalogRevision)
	}
}

// The end-to-end proof that a pin actually reaches the backend: the fake records every
// /respond body, so this asserts on what the SERVER saw rather than on what the client
// believes it sent.
func TestBinaryPinnedSkillReachesTheWire(t *testing.T) {
	be := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})

	stdout, stderr, code := runSkillsCLI(t, be.baseURL(),
		"--json", "--skill", "daintree.foundation", "hello")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stdout %q stderr %q)", code, stdout, stderr)
	}

	req := be.request(0)
	if req == nil {
		t.Fatal("the backend saw no /respond request")
	}
	sel, _ := req["selection"].(map[string]any)
	if sel == nil {
		t.Fatalf("no selection block on the wire: %+v", req)
	}
	raw, _ := sel["pinned_skill_ids"].([]any)
	if len(raw) != 1 || raw[0] != "daintree.foundation" {
		t.Fatalf("selection.pinned_skill_ids = %v, want [daintree.foundation]", raw)
	}
}

// The pre-#54 safety property, proved on the wire rather than in a struct: an unpinned
// run must not mention the field at all. A backend validating with extra="forbid" would
// 422 the whole turn over a stray key, so this is the regression that would break every
// session against an older deployment at once.
func TestBinaryUnpinnedRunOmitsThePinField(t *testing.T) {
	be := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})

	if _, stderr, code := runSkillsCLI(t, be.baseURL(), "--json", "hello"); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	req := be.request(0)
	if req == nil {
		t.Fatal("the backend saw no /respond request")
	}
	sel, _ := req["selection"].(map[string]any)
	if sel == nil {
		t.Fatalf("no selection block on the wire: %+v", req)
	}
	if _, present := sel["pinned_skill_ids"]; present {
		t.Fatalf("an unpinned run put the field on the wire: %+v", sel)
	}
}

// The fatal preflight, end to end and asserted on the SERVER: a mistyped id must cost
// zero turns. This is the test that catches the preflight being skipped, or moved after
// the first /respond — at which point --skill would still "work" in every other test
// while quietly having spent a turn to discover the typo.
func TestBinaryUnknownPinnedSkillSpendsNoTurn(t *testing.T) {
	be := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})

	// Human mode first: the message belongs on stderr, and stdout — the ANSWER channel —
	// must stay empty so a caller capturing it gets nothing rather than an error rendered
	// as the reply.
	stdout, stderr, code := runSkillsCLI(t, be.baseURL(), "--skill", "daintree.foundatoin", "hello")
	if code == 0 {
		t.Fatalf("a mistyped --skill must fail the launch (stdout %q)", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("stdout must stay empty on a failed human run, got %q", stdout)
	}
	// The near miss is the whole point: "unknown id" alone leaves you re-reading the
	// backend's source, which is what --list-skills and this check exist to end.
	if !strings.Contains(stderr, "daintree.foundation") {
		t.Fatalf("stderr does not offer the near miss: %q", stderr)
	}

	// Under --json the SAME failure rides the JSONL stream instead, because stdout is the
	// only channel a scripted caller reads. The message must survive the change of
	// channel — a machine-readable failure that dropped the near miss would be strictly
	// worse than the human one.
	stdout, _, code = runSkillsCLI(t, be.baseURL(), "--json", "--skill", "daintree.foundatoin", "hello")
	if code == 0 {
		t.Fatal("a mistyped --skill must fail the launch under --json too")
	}
	var sawError bool
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("stdout is not JSONL (%v): %q", err, line)
		}
		if ev.Type == "error" && strings.Contains(ev.Message, "daintree.foundation") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("no JSONL error event carried the near miss:\n%s", stdout)
	}

	// The point of the whole test: neither run reached the backend. Checked LAST so it
	// covers both invocations at once.
	if n := be.callCount(); n != 0 {
		t.Fatalf("the backend served %d /respond request(s); an unknown pin must be caught before any turn", n)
	}
}
