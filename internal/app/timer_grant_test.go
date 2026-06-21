package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
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
