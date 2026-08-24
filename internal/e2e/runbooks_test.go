package e2e

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// runbooks_test.go drives `--list-runbooks` and `--runbook` through the REAL binary against the
// fake backend. The unit tests prove each layer; this proves the argv surface, the route,
// the capability read and the wire field are actually connected to each other — which is
// where a feature threaded through five packages usually breaks.

// runRunbooksCLI invokes the built binary against a fake backend with an isolated state dir.
func runRunbooksCLI(t *testing.T, backendURL string, args ...string) (string, string, int) {
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

// The listing is what makes --runbook usable at all: you cannot name an id you cannot
// enumerate. It must work with no project lease and no MCP.
func TestBinaryListRunbooks(t *testing.T) {
	be := newFakeBackend(t)

	stdout, stderr, code := runRunbooksCLI(t, be.baseURL(), "--list-runbooks")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	for _, want := range []string{"daintree.foundation", "Multi-agent orchestration"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("listing is missing %q:\n%s", want, stdout)
		}
	}

	// --json must put ONE parseable document on stdout, because feeding ids into --runbook
	// from a script is the whole reason the JSON form exists.
	stdout, stderr, code = runRunbooksCLI(t, be.baseURL(), "--list-runbooks", "--json")
	if code != 0 {
		t.Fatalf("json exit = %d, want 0 (stderr %q)", code, stderr)
	}
	var doc struct {
		CatalogRevision string `json:"catalogRevision"`
		Runbooks        []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"runbooks"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, stdout)
	}
	if len(doc.Runbooks) != 2 {
		t.Fatalf("runbooks = %+v, want the fake's two entries", doc.Runbooks)
	}
	if doc.CatalogRevision != "sha256:test" {
		t.Fatalf("catalogRevision = %q, want sha256:test", doc.CatalogRevision)
	}
}

// The end-to-end proof that a pin actually reaches the backend: the fake records every
// /respond body, so this asserts on what the SERVER saw rather than on what the client
// believes it sent.
func TestBinaryPinnedRunbookReachesTheWire(t *testing.T) {
	be := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})

	stdout, stderr, code := runRunbooksCLI(t, be.baseURL(),
		"--json", "--runbook", "daintree.foundation", "hello")
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
	raw, _ := sel["pinned_runbook_ids"].([]any)
	if len(raw) != 1 || raw[0] != "daintree.foundation" {
		t.Fatalf("selection.pinned_runbook_ids = %v, want [daintree.foundation]", raw)
	}
}

// The pre-#54 safety property, proved on the wire rather than in a struct: an unpinned
// run must not mention the field at all. A backend validating with extra="forbid" would
// 422 the whole turn over a stray key, so this is the regression that would break every
// session against an older deployment at once.
func TestBinaryUnpinnedRunOmitsThePinField(t *testing.T) {
	be := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})

	if _, stderr, code := runRunbooksCLI(t, be.baseURL(), "--json", "hello"); code != 0 {
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
	if _, present := sel["pinned_runbook_ids"]; present {
		t.Fatalf("an unpinned run put the field on the wire: %+v", sel)
	}
}

// The fatal preflight, end to end and asserted on the SERVER: a mistyped id must cost
// zero turns. This is the test that catches the preflight being skipped, or moved after
// the first /respond — at which point --runbook would still "work" in every other test
// while quietly having spent a turn to discover the typo.
func TestBinaryUnknownPinnedRunbookSpendsNoTurn(t *testing.T) {
	be := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})

	// Human mode first: the message belongs on stderr, and stdout — the ANSWER channel —
	// must stay empty so a caller capturing it gets nothing rather than an error rendered
	// as the reply.
	stdout, stderr, code := runRunbooksCLI(t, be.baseURL(), "--runbook", "daintree.foundatoin", "hello")
	if code == 0 {
		t.Fatalf("a mistyped --runbook must fail the launch (stdout %q)", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("stdout must stay empty on a failed human run, got %q", stdout)
	}
	// The near miss is the whole point: "unknown id" alone leaves you re-reading the
	// backend's source, which is what --list-runbooks and this check exist to end.
	if !strings.Contains(stderr, "daintree.foundation") {
		t.Fatalf("stderr does not offer the near miss: %q", stderr)
	}

	// Under --json the SAME failure rides the JSONL stream instead, because stdout is the
	// only channel a scripted caller reads. The message must survive the change of
	// channel — a machine-readable failure that dropped the near miss would be strictly
	// worse than the human one.
	stdout, _, code = runRunbooksCLI(t, be.baseURL(), "--json", "--runbook", "daintree.foundatoin", "hello")
	if code == 0 {
		t.Fatal("a mistyped --runbook must fail the launch under --json too")
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

// The round trip, in one invocation: the pin goes out on the wire AND the committed
// decision comes back on stdout naming it. Both halves already have a test —
// TestBinaryPinnedRunbookReachesTheWire watches the request, TestBinaryJSONOneShot watches
// the stream for the backend's own unforced selection — and both would keep passing if
// the two ends came apart, because neither one ever pins a runbook and then reads what the
// transcript said about it. That correlation is what every future runbook test stands on:
// a harness asserts "the runbook under development was active" by reading runbook:decision
// after naming it with --runbook, and nothing else proves those are the same runbook.
func TestBinaryPinnedRunbookRoundTripsThroughJSON(t *testing.T) {
	const (
		runbookID    = "daintree.foundation"
		runbookTitle = "Foundation"
	)
	// The fake does not honour pins — it replays a script — so the round's runbooks block
	// stands in for a backend that did. That is the right seam: what is under test is the
	// CLI's two ends, not the backend's obedience, which its own suite owns.
	be := newFakeBackend(t, sseRound{
		contentTokens: []string{"ok"},
		runbooks: runbooksBlock(false,
			[]string{runbookID, runbookTitle},
			[]string{runbookID, runbookTitle}),
	})

	// runRunbooksCLI passes argv through verbatim, so --json is the caller's job; without it
	// the answer channel carries prose and there is no decision to read.
	stdout, stderr, code := runRunbooksCLI(t, be.baseURL(),
		"--json", "--runbook", runbookID, "hello")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stdout %q stderr %q)", code, stdout, stderr)
	}

	// Outbound: asserted on what the SERVER received, same shape as the wire-only test.
	req := be.request(0)
	if req == nil {
		t.Fatal("the backend saw no /respond request")
	}
	sel, _ := req["selection"].(map[string]any)
	if sel == nil {
		t.Fatalf("no selection block on the wire: %+v", req)
	}
	pinned, _ := sel["pinned_runbook_ids"].([]any)
	if len(pinned) != 1 || pinned[0] != runbookID {
		t.Fatalf("selection.pinned_runbook_ids = %v, want [%s]", pinned, runbookID)
	}

	// Inbound: the committed decision on the real --json stream. Parsed as generic JSON so
	// the emitted casing stays observable — a typed struct would silently accept either.
	var decisions []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("stdout is not JSONL (%v): %q", err, line)
		}
		if raw["type"] == "runbook:decision" {
			decisions = append(decisions, raw)
		}
	}
	if len(decisions) != 1 {
		t.Fatalf("runbook:decision count = %d, want one for the single scripted round:\n%s",
			len(decisions), stdout)
	}

	// active is the authoritative committed set — newlyLoaded is only the delta, and a
	// retained runbook never appears in it, so a harness that read the delta would miss
	// the pin on every round after the first.
	active, _ := decisions[0]["active"].([]any)
	if len(active) != 1 {
		t.Fatalf("runbook:decision active = %#v, want the pinned runbook", decisions[0]["active"])
	}
	entry, _ := active[0].(map[string]any)
	if entry["id"] != runbookID || entry["title"] != runbookTitle {
		t.Fatalf("active[0] = %#v, want id %q title %q", entry, runbookID, runbookTitle)
	}
}
