package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/daemon"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// grantTestTool is a confirm-required (project risk) tool that just succeeds, used
// to prove a timer-scoped grant authorizes an otherwise-denied autonomous call.
func grantTestTool() *tools.Tool {
	return &tools.Tool{
		Name:   "test.grantable",
		Risk:   domain.RiskProject, // in AlwaysConfirm → needs a grant for a timer actor
		Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`),
		Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			return tools.Ok("did the thing", nil)
		},
	}
}

// TestTimerScopedGrantSucceedsThroughDaemonAdapter is the end-to-end proof for
// Finding 2: a timer that fires a confirm-required call_safe_tool succeeds ONLY
// because the firing timer's id is threaded as the dispatch actorId, letting the
// scoped automation grant be matched and consumed. Without the threading the
// dispatch saw actorId="" and always returned CONFIRMATION_REQUIRED.
func TestTimerScopedGrantSucceedsThroughDaemonAdapter(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if err := a.Registry.Register(grantTestTool()); err != nil {
		t.Fatalf("register test tool: %v", err)
	}

	const timerID = "tmr_scoped"
	riskJSON := `["project"]`
	// A live grant scoped to THIS timer authorizing the project risk class.
	if _, err := a.Store.InsertGrant(domain.AutomationGrantRecord{
		ActorID:                timerID,
		ActorType:              domain.AutomationGrantActorType(domain.ActorTimer),
		AllowedRiskClassesJson: &riskJSON,
		ExpiresAt:              domain.NowMS() + 600_000,
		MaxUses:                5,
	}); err != nil {
		t.Fatalf("insert grant: %v", err)
	}

	adapter := daemonRegistryAdapter{app: a}

	// With the correct timer id threaded, the grant is consumed and the call runs.
	res, err := adapter.Dispatch(context.Background(), domain.ActorTimer, timerID, "test.grantable", "{}")
	if err != nil {
		t.Fatalf("adapter dispatch: %v", err)
	}
	if !res.Ok {
		t.Fatalf("a timer-scoped grant must authorize the call, got %+v", res.Error)
	}

	// Control: a different actor id has no matching grant → still denied. This
	// confirms the grant match is genuinely keyed on the threaded id, not a no-op.
	denied, _ := adapter.Dispatch(context.Background(), domain.ActorTimer, "tmr_other", "test.grantable", "{}")
	if denied.Ok || denied.Error.Code != "CONFIRMATION_REQUIRED" {
		t.Fatalf("an unrelated timer id must be denied (CONFIRMATION_REQUIRED), got %+v", denied)
	}
}

// TestOneShotTimerSpendsItsGrantThroughTheRealScheduler is the cross-layer proof for
// issue #333. The test above dispatches through the adapter directly, which skips
// fireTimer entirely — so it stayed green while the headline workflow the tool's own
// description recommends (one one-shot timer whose call_safe_tool payload runs a
// confirm-required tool, plus a timer-scoped grant) could not succeed at all: the
// terminal claim revoked the grant before the dispatch that needed it. This drives a
// REAL scheduler over the REAL store and registry, so the ordering is covered
// end-to-end rather than at the seam below it.
func TestOneShotTimerSpendsItsGrantThroughTheRealScheduler(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	ran := 0
	tool := grantTestTool()
	tool.Handle = func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
		ran++
		return tools.Ok("did the thing", nil)
	}
	if err := a.Registry.Register(tool); err != nil {
		t.Fatalf("register test tool: %v", err)
	}

	const timerID = "tmr_oneshot"
	riskJSON := `["project"]`
	if _, err := a.Store.InsertGrant(domain.AutomationGrantRecord{
		ActorID:                timerID,
		ActorType:              domain.AutomationGrantActorType(domain.ActorTimer),
		AllowedRiskClassesJson: &riskJSON,
		ExpiresAt:              domain.NowMS() + 600_000,
		MaxUses:                5,
	}); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	// No repeat block ⇒ TERMINAL on the very first fire, which is exactly the case
	// that used to lose its grant.
	if _, err := a.Store.InsertTimer(domain.TimerRecord{
		ID: timerID, Title: "spawn at fire time", FireAt: 1, Status: "scheduled",
		PayloadType: "call_safe_tool",
		PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"test.grantable","args":{}}}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}

	s := daemon.NewScheduler(daemon.SchedulerDeps{
		Store:    a.Store,
		Queue:    daemonQueueAdapter{q: a.Queue},
		Registry: daemonRegistryAdapter{app: a},
	})
	s.Tick(context.Background(), 1_000)

	if ran != 1 {
		t.Fatalf("the confirm-required payload must run exactly once on a grant-backed one-shot fire, ran %d times", ran)
	}
	// And the authority must not outlive the timer: a fired one-shot never fires
	// again, so leaving a live grant behind would be a standing permission nobody
	// can spend or see.
	live, err := a.Store.ListGrants(timerID, domain.NowMS())
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("a terminal fire must revoke the timer's grants afterwards, %d still live", len(live))
	}
}
