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
