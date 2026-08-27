package agenttaskx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// worktreeRoster builds a worktree.list structuredContent payload. Each triple is
// id|path|branch; id is the value Daintree keys worktrees by (a path).
func worktreeRoster(entries ...[3]string) MCPCallResult {
	arr := make([]any, 0, len(entries))
	for _, e := range entries {
		arr = append(arr, map[string]any{"id": e[0], "path": e[1], "branch": e[2]})
	}
	return MCPCallResult{StructuredContent: map[string]any{"worktrees": arr}}
}

// TestListWorktreesTimeoutIsCancel asserts the worktree read bounds itself with a CANCEL
// (not a deadline): on timeout the ctx error the MCP layer observes is context.Canceled (so
// mcp.Client does NOT degrade the connection on a slow read), and the caller falls open to
// nil. A transport error also fails open.
func TestListWorktreesTimeoutIsCancel(t *testing.T) {
	defer func(prev time.Duration) { worktreeRosterTimeout = prev }(worktreeRosterTimeout)
	worktreeRosterTimeout = 20 * time.Millisecond

	b := &blockingMCP{}
	if got := listWorktrees(context.Background(), b); got != nil {
		t.Fatalf("a timed-out worktree read must fail open to nil, got %v", got)
	}
	if !errors.Is(b.ctxErr, context.Canceled) {
		t.Fatalf("worktree timeout must surface as context.Canceled (so mcp.Client does not degrade), got %v", b.ctxErr)
	}
	if got := listWorktrees(context.Background(), errMCP{}); got != nil {
		t.Fatalf("a transport error must fail open to nil, got %v", got)
	}
}

func TestListWorktreesParsesBothSources(t *testing.T) {
	// structuredContent path.
	sc := &scriptMCP{connected: true, worktreeList: worktreeRoster(
		[3]string{"/p/app", "/p/app", "main"},
		[3]string{"/p/app-wt", "/p/app-wt", "feature-x"},
	)}
	if got := listWorktrees(context.Background(), sc); len(got) != 2 || got[0].branch != "main" || got[1].id != "/p/app-wt" {
		t.Fatalf("structuredContent parse = %v", got)
	}
	// Text-body fallback (Daintree returns results in text).
	tx := &scriptMCP{connected: true, worktreeList: MCPCallResult{
		Text: `{"worktrees":[{"id":"/p/app","path":"/p/app","branch":"main"}]}`,
	}}
	if got := listWorktrees(context.Background(), tx); len(got) != 1 || got[0].branch != "main" {
		t.Fatalf("text parse = %v", got)
	}
	// UNION: a worktree present only in structuredContent and another present only in the
	// text body must BOTH survive (a regression that ignored text whenever structuredContent
	// existed would drop /p/app-wt).
	union := &scriptMCP{connected: true, worktreeList: MCPCallResult{
		StructuredContent: map[string]any{"worktrees": []any{map[string]any{"id": "/p/app", "branch": "main"}}},
		Text:              `{"worktrees":[{"id":"/p/app-wt","branch":"feature-x"}]}`,
	}}
	if got := listWorktrees(context.Background(), union); len(got) != 2 || got[0].id != "/p/app" || got[1].id != "/p/app-wt" {
		t.Fatalf("union of both sources = %v, want both worktrees", got)
	}
	// Same id in both sources dedupes to one row AND backfills the field the first source
	// left blank (structured has no branch; text supplies it).
	merge := &scriptMCP{connected: true, worktreeList: MCPCallResult{
		StructuredContent: map[string]any{"worktrees": []any{map[string]any{"id": "/p/app", "path": "/p/app"}}},
		Text:              `{"worktrees":[{"id":"/p/app","branch":"main"}]}`,
	}}
	got := listWorktrees(context.Background(), merge)
	if len(got) != 1 || got[0].branch != "main" || got[0].path != "/p/app" {
		t.Fatalf("merge by id = %v, want one row with branch and path filled", got)
	}
}

func TestResolveWorktreeID(t *testing.T) {
	roster := &scriptMCP{connected: true, worktreeList: worktreeRoster(
		[3]string{"/p/app", "/p/app", "main"},
		[3]string{"/p/app-wt", "/p/app-wt", "feature-x"},
	)}

	// Omitted ⇒ ok, empty (Daintree picks the active worktree), no read needed.
	noRead := &scriptMCP{connected: true}
	if resolved, ok, _, _ := resolveWorktreeID(context.Background(), noRead, "  "); !ok || resolved != "" {
		t.Fatalf("blank worktreeId must resolve ok to empty, got (%q, %v)", resolved, ok)
	}
	if noRead.called("worktree.list") {
		t.Fatal("a blank worktreeId must not trigger a worktree.list read")
	}
	// Exact id passes through unchanged.
	if resolved, ok, _, _ := resolveWorktreeID(context.Background(), roster, "/p/app-wt"); !ok || resolved != "/p/app-wt" {
		t.Fatalf("exact id = (%q, %v), want (/p/app-wt, true)", resolved, ok)
	}
	// Case-insensitive id/path still resolves to the canonical id.
	if resolved, ok, _, _ := resolveWorktreeID(context.Background(), roster, "/P/APP-WT"); !ok || resolved != "/p/app-wt" {
		t.Fatalf("case-varied id = (%q, %v), want (/p/app-wt, true)", resolved, ok)
	}
	// THE fix: a branch name maps to that worktree's canonical id.
	if resolved, ok, _, _ := resolveWorktreeID(context.Background(), roster, "main"); !ok || resolved != "/p/app" {
		t.Fatalf("branch \"main\" must map to its worktree id, got (%q, %v)", resolved, ok)
	}
	if resolved, ok, _, _ := resolveWorktreeID(context.Background(), roster, "feature-x"); !ok || resolved != "/p/app-wt" {
		t.Fatalf("branch \"feature-x\" must map to /p/app-wt, got (%q, %v)", resolved, ok)
	}
	// A genuinely unknown value is rejected with the available list + a near-miss.
	resolved, ok, available, suggestion := resolveWorktreeID(context.Background(), roster, "mian")
	if ok {
		t.Fatal("an unknown worktree must not resolve ok")
	}
	if resolved != "" || len(available) != 2 || suggestion != "/p/app" {
		t.Fatalf("reject = (%q, available %d, suggestion %q), want (\"\", 2, /p/app)", resolved, len(available), suggestion)
	}
	// Unreadable roster ⇒ fail open (proceed with the original value).
	if resolved, ok, _, _ := resolveWorktreeID(context.Background(), &scriptMCP{connected: true}, "anything"); !ok || resolved != "anything" {
		t.Fatalf("an unreadable roster must fail open, got (%q, %v)", resolved, ok)
	}
}

// An exact id wins even when it coincidentally equals ANOTHER worktree's branch — the id
// pass runs to completion before the branch pass, so the value is never hijacked.
func TestResolveWorktreeIDExactIdBeatsBranchCollision(t *testing.T) {
	roster := &scriptMCP{connected: true, worktreeList: worktreeRoster(
		[3]string{"release", "/p/release-wt", "v2"}, // id "release"
		[3]string{"/p/app", "/p/app", "release"},    // branch "release"
	)}
	if resolved, ok, _, _ := resolveWorktreeID(context.Background(), roster, "release"); !ok || resolved != "release" {
		t.Fatalf("an exact id must win over a branch collision, got (%q, %v)", resolved, ok)
	}
}

// An ambiguous branch (two worktrees claim it) is NOT auto-mapped — it falls through to the
// reject so the model picks the exact id, never a silent spawn in the wrong worktree.
func TestResolveWorktreeIDRejectsAmbiguousBranch(t *testing.T) {
	roster := &scriptMCP{connected: true, worktreeList: worktreeRoster(
		[3]string{"/p/a", "/p/a", "shared"},
		[3]string{"/p/b", "/p/b", "shared"},
	)}
	if _, ok, _, _ := resolveWorktreeID(context.Background(), roster, "shared"); ok {
		t.Fatal("an ambiguous branch must not auto-map to a single worktree")
	}
}

// IsError fails open even with a non-empty payload, proving IsError (not just an empty body)
// short-circuits the read.
func TestListWorktreesFailsOpenOnIsError(t *testing.T) {
	m := &scriptMCP{connected: true, worktreeList: MCPCallResult{
		IsError: true,
		Text:    `{"worktrees":[{"id":"/p/app","branch":"main"}]}`,
	}}
	if got := listWorktrees(context.Background(), m); got != nil {
		t.Fatalf("an IsError result must fail open to nil even with a payload, got %v", got)
	}
}

func TestClosestWorktreeID(t *testing.T) {
	cands := []worktreeInfo{
		{id: "/p/app", branch: "main"},
		{id: "/p/app-wt", branch: "feature-x"},
	}
	// Distance 2 from "main" == threshold (max(2, len/3)) ⇒ inclusive, suggests the id.
	if got := closestWorktreeID("mian", cands); got != "/p/app" {
		t.Errorf("mian -> %q, want /p/app", got)
	}
	// Just over the threshold ⇒ no suggestion.
	if got := closestWorktreeID("zzzzzzzz", cands); got != "" {
		t.Errorf("unrelated -> %q, want empty", got)
	}
}

// The spawn gate rejects an unresolvable worktreeId BEFORE any agent.launch or saga write —
// the regression where worktreeId "no-such-branch" launched nothing and returned a silent
// ambiguous success. The Fail carries the available roster + suggestion for the model.
func TestSpawnRejectsUnknownWorktree(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_5"),
		worktreeList: worktreeRoster(
			[3]string{"/p/app", "/p/app", "main"},
			[3]string{"/p/app-wt", "/p/app-wt", "feature-x"},
		)}
	st := newSagaStore()
	res := spawnMain(context.Background(), Deps{MCP: mcp, DB: st}, &spawnArgs{
		AgentID: "claude", Mode: "explore", Title: "explore it", TaskPrompt: "go", WorktreeID: "no-such-branch",
	})
	if res.Ok || res.Error.Code != codeUnknownWorktree {
		t.Fatalf("want UNKNOWN_WORKTREE, got %+v", res)
	}
	if mcp.launchCount() != 0 {
		t.Fatal("must not call agent.launch for an unresolvable worktree")
	}
	if len(st.launches) != 0 {
		t.Fatal("must not write a saga record for an unresolvable worktree")
	}
	details, _ := res.Error.Details.(map[string]any)
	if rows, _ := details["availableWorktrees"].([]map[string]any); len(rows) != 2 {
		t.Fatalf("reject details must list the available worktrees, got %+v", details["availableWorktrees"])
	}
}

// A branch name passed as worktreeId is NORMALIZED to the worktree's canonical id, and that
// canonical id flows into BOTH the launch args and the agent prompt — what would previously
// have silently failed now just works, consistently.
func TestSpawnNormalizesBranchWorktreeId(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_7"),
		worktreeList: worktreeRoster([3]string{"/p/app", "/p/app", "main"})}
	st := newSagaStore()
	res := spawnMain(context.Background(), Deps{MCP: mcp, DB: st}, &spawnArgs{
		AgentID: "claude", Mode: "explore", Title: "explore it", TaskPrompt: "go", WorktreeID: "main",
	})
	if !res.Ok {
		t.Fatalf("a branch-name worktreeId must normalize and spawn, got %+v", res.Error)
	}
	args := mcp.lastLaunchArgs()
	if args["worktreeId"] != "/p/app" {
		t.Fatalf("launch forwarded worktreeId %v, want the canonical id /p/app", args["worktreeId"])
	}
	// The prompt must reference the RESOLVED id, not the raw branch the caller typed.
	if prompt, _ := args["prompt"].(string); !strings.Contains(prompt, "Work in worktree: /p/app") || strings.Contains(prompt, "Work in worktree: main") {
		t.Fatalf("prompt should embed the canonical worktree id, got %q", prompt)
	}
}

// An omitted (or whitespace-only) worktreeId with NO binding available used to be
// forwarded as-is, letting Daintree pick its live active worktree. That fallback is
// gone: Daintree now REFUSES an agent-dispatched launch that names no worktree, so
// forwarding it buys a guaranteed refusal with a worse message. Fail locally instead,
// launching nothing and writing no saga row.
func TestSpawnUnpinnedOmittedWorktreeFailsLocally(t *testing.T) {
	for _, wt := range []string{"", "   "} {
		mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
		st := newSagaStore()
		res := spawnUnpinned(context.Background(), Deps{MCP: mcp, DB: st}, &spawnArgs{
			AgentID: "claude", Mode: "explore", Title: "t", TaskPrompt: "go", WorktreeID: wt,
		}, domain.ActorMain)
		if res.Ok || res.Error.Code != codeUnknownWorktree {
			t.Fatalf("worktreeId %q with no binding should fail locally, got %+v", wt, res)
		}
		if mcp.launchCount() != 0 {
			t.Fatalf("worktreeId %q must not reach agent.launch at all", wt)
		}
		if len(st.launches) != 0 {
			t.Fatalf("worktreeId %q must not write a saga record", wt)
		}
		details, _ := res.Error.Details.(map[string]any)
		if unavailable, _ := details["turnWorktreeUnavailable"].(bool); !unavailable {
			t.Errorf("details should mark this as the turn having no worktree, got %+v", details)
		}
	}
}

// A durable timer can invoke agentTask.spawnForEdits at its firing time, which happens
// OUTSIDE any turn. The pin is a process-wide ambient value with no owner tag, so an
// off-turn dispatch that consumed it would silently borrow whichever worktree the last
// interactive turn bound — possibly one the user left hours ago. It must name its own.
func TestSpawnOffTurnActorDoesNotBorrowTheTurnPin(t *testing.T) {
	for _, actor := range []domain.ToolActor{domain.ActorTimer, domain.ActorWatcher, domain.ActorWorkflow} {
		mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1"),
			worktreeList: worktreeRoster([3]string{"/p/app", "/p/app", "main"})}
		st := newSagaStore()
		deps := Deps{MCP: mcp, DB: st, WorktreePin: fixedPin{id: "/p/app", path: "/p/app", branch: "main"}}
		res := spawn(context.Background(), deps, &spawnArgs{
			AgentID: "claude", Mode: "explore", Title: "t", TaskPrompt: "go",
		}, actor)

		if res.Ok || res.Error.Code != codeUnknownWorktree {
			t.Fatalf("%s: an off-turn spawn must not inherit the turn's worktree, got %+v", actor, res)
		}
		if mcp.launchCount() != 0 {
			t.Fatalf("%s: nothing may launch", actor)
		}
		details, _ := res.Error.Details.(map[string]any)
		if details["actor"] != string(actor) {
			t.Errorf("%s: details should name the actor, got %+v", actor, details)
		}
	}

	// The turn actors DO consume it — that is the whole point of the binding.
	for _, actor := range []domain.ToolActor{domain.ActorMain, domain.ActorWake} {
		mcp := &scriptMCP{connected: true, launchResult: launchOK("term_2"),
			worktreeList: worktreeRoster([3]string{"/p/app", "/p/app", "main"})}
		deps := Deps{MCP: mcp, DB: newSagaStore(), WorktreePin: fixedPin{id: "/p/app", path: "/p/app", branch: "main"}}
		res := spawn(context.Background(), deps, &spawnArgs{
			AgentID: "claude", Mode: "explore", Title: "t", TaskPrompt: "go",
		}, actor)
		if !res.Ok {
			t.Fatalf("%s runs inside a turn and must use the pin, got %+v", actor, res.Error)
		}
		if got := mcp.lastLaunchArgs()["worktreeId"]; got != "/p/app" {
			t.Fatalf("%s: launch forwarded %v, want the pinned /p/app", actor, got)
		}
	}
}

// fixedPin is a bound turn worktree.
type fixedPin struct{ id, path, branch string }

func (f fixedPin) ID() string                         { return f.id }
func (f fixedPin) Describe() (string, string, string) { return f.id, f.path, f.branch }

// The bug this closes: Daintree resolves an omitted worktreeId against its LIVE active
// selection at the instant the launch lands, so a human switching worktrees while a
// concurrent spawn cohort was in flight split that cohort across two worktrees. The
// turn's pin is substituted here so the launch args SAY where the turn meant, and the
// pinned id goes through the same validation an explicit one does.
func TestSpawnOmittedWorktreeUsesTheTurnPin(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_9"),
		worktreeList: worktreeRoster([3]string{"/p/app", "/p/app", "main"})}
	deps := Deps{MCP: mcp, DB: newSagaStore(), WorktreePin: fixedPin{id: "/p/app", path: "/p/app", branch: "main"}}
	res := spawnMain(context.Background(), deps, &spawnArgs{
		AgentID: "claude", Mode: "explore", Title: "t", TaskPrompt: "go",
	})
	if !res.Ok {
		t.Fatalf("a pinned spawn must succeed, got %+v", res.Error)
	}
	if got := mcp.lastLaunchArgs()["worktreeId"]; got != "/p/app" {
		t.Fatalf("launch forwarded worktreeId %v, want the pinned /p/app", got)
	}
	// The agent's own instructions must name the same worktree the launch targets.
	if prompt, _ := mcp.lastLaunchArgs()["prompt"].(string); !strings.Contains(prompt, "Work in worktree: /p/app") {
		t.Fatalf("prompt should embed the pinned worktree id, got %q", prompt)
	}
}

// Naming a worktree is how the model sends an agent somewhere OTHER than where the turn
// began, so an explicit id must beat the pin. Getting this backwards would make the pin
// a cage rather than a default.
func TestSpawnExplicitWorktreeBeatsTheTurnPin(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_10"),
		worktreeList: worktreeRoster(
			[3]string{"/p/app", "/p/app", "main"},
			[3]string{"/p/app-wt", "/p/app-wt", "feature-x"},
		)}
	deps := Deps{MCP: mcp, DB: newSagaStore(), WorktreePin: fixedPin{id: "/p/app", path: "/p/app", branch: "main"}}
	res := spawnMain(context.Background(), deps, &spawnArgs{
		AgentID: "claude", Mode: "explore", Title: "t", TaskPrompt: "go", WorktreeID: "feature-x",
	})
	if !res.Ok {
		t.Fatalf("an explicit worktree must spawn, got %+v", res.Error)
	}
	if got := mcp.lastLaunchArgs()["worktreeId"]; got != "/p/app-wt" {
		t.Fatalf("launch forwarded worktreeId %v, want the explicitly named /p/app-wt", got)
	}
}

// A worktree deleted or closed mid-turn is not the model's mistake and it cannot fix it
// by re-reading, so the refusal must say the TURN'S worktree is gone rather than blaming
// an id the model never passed. Nothing may launch into a substitute.
func TestSpawnRejectsAVanishedTurnPin(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_11"),
		worktreeList: worktreeRoster([3]string{"/p/other", "/p/other", "main"})}
	st := newSagaStore()
	deps := Deps{MCP: mcp, DB: st, WorktreePin: fixedPin{id: "/p/gone", path: "/p/gone", branch: "dead"}}
	res := spawnMain(context.Background(), deps, &spawnArgs{
		AgentID: "claude", Mode: "explore", Title: "t", TaskPrompt: "go",
	})
	if res.Ok || res.Error.Code != codeUnknownWorktree {
		t.Fatalf("want UNKNOWN_WORKTREE for a vanished pin, got %+v", res)
	}
	if mcp.launchCount() != 0 {
		t.Fatal("must not launch into a substitute worktree when the turn's own is gone")
	}
	if len(st.launches) != 0 {
		t.Fatal("must not write a saga record when the turn's worktree is gone")
	}
	details, _ := res.Error.Details.(map[string]any)
	if fromTurn, _ := details["fromTurnWorktree"].(bool); !fromTurn {
		t.Errorf("details must mark the id as the turn's, not the caller's: %+v", details)
	}
	if !strings.Contains(res.Error.Message, "turn started in") {
		t.Errorf("message should say the turn's worktree is gone, got %q", res.Error.Message)
	}
}
