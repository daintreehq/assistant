package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// Finding 1: a tool excluded by the per-turn projection (ActiveToolNames) must be
// rejected at dispatch even when invoked directly — defense in depth over the
// schema projection, so an autonomous wake / loop bug can't run an unoffered tool.
func TestDispatchRejectsToolNotInActiveAllowlist(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("x.echo", domain.RiskRead))
	_ = r.Register(echoTool("x.other", domain.RiskRead))
	s := &fakeStore{}

	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
	// Only x.other is offered this turn; x.echo was projection-excluded.
	ctx.ActiveToolNames = []string{"x.other"}

	res := r.Dispatch(context.Background(), "x.echo", json.RawMessage(`{"x":1}`), ctx)
	if res.Ok || res.Error.Code != codeNotOffered {
		t.Fatalf("want TOOL_NOT_OFFERED, got %+v", res.Error)
	}
	if res.Error.Recoverable {
		t.Fatal("TOOL_NOT_OFFERED must be non-recoverable")
	}
	if lastAudit(s).Outcome != outcomeDenied {
		t.Fatalf("not-offered tool audited as %s, want denied", lastAudit(s).Outcome)
	}
}

// An offered tool still runs, and a nil ActiveToolNames leaves dispatch unconstrained.
func TestDispatchAllowsOfferedTool(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("x.echo", domain.RiskRead))
	s := &fakeStore{}

	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
	ctx.ActiveToolNames = []string{"x.echo"}
	if res := r.Dispatch(context.Background(), "x.echo", json.RawMessage(`{"x":1}`), ctx); !res.Ok {
		t.Fatalf("offered tool should run, got %+v", res.Error)
	}

	// nil allowlist ⇒ unconstrained (every tool callable).
	ctx2 := baseCtx(&fakeStore{}, nil, domain.TierSystem, domain.ActorMain)
	if res := r.Dispatch(context.Background(), "x.echo", json.RawMessage(`{"x":1}`), ctx2); !res.Ok {
		t.Fatalf("nil allowlist should be unconstrained, got %+v", res.Error)
	}
}

// panickyDecodeTool panics inside Decode (BEFORE the handler), exercising the
// top-level dispatch panic firewall (Finding 3).
func panickyDecodeTool(name string) *Tool {
	return &Tool{
		Name: name, Risk: domain.RiskRead,
		Schema: json.RawMessage(`{"type":"object"}`),
		Decode: func(_ json.RawMessage) (json.RawMessage, error) {
			panic("boom in decode")
		},
		Handle: func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult {
			return Ok("ok", nil)
		},
	}
}

// Finding 3: a panic in Decode (not just the handler) is converted to TOOL_THREW
// and STILL audited — dispatch never panics to the caller and never skips audit.
func TestDispatchPanicInDecodeIsConvertedAndAudited(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(panickyDecodeTool("x.boom"))
	s := &fakeStore{}

	res := r.Dispatch(context.Background(), "x.boom", json.RawMessage(`{}`),
		baseCtx(s, nil, domain.TierSystem, domain.ActorMain))
	if res.Ok || res.Error.Code != codeToolThrew {
		t.Fatalf("want TOOL_THREW from a panicking decode, got %+v", res.Error)
	}
	if len(s.audits) == 0 {
		t.Fatal("a panicking decode must still write an audit row")
	}
	if lastAudit(s).Outcome != outcomeError {
		t.Fatalf("panic audited as %s, want error", lastAudit(s).Outcome)
	}
}

// panickyQueue panics on Publish, exercising the recover guard in publishDenial.
type panickyQueue struct{}

func (*panickyQueue) Publish(_ context.Context, _ domain.QueuePublishArgs) (domain.QueueEvent, error) {
	panic("boom in publish")
}

// publishDenial is best-effort: a panic from the attention queue must be
// contained and never break the tool call. The blocked actor still gets its
// CONFIRMATION_REQUIRED result; only the (failed) notification is lost.
func TestDispatchPublishDenialPanicIsContained(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}

	ctx := baseCtx(s, &panickyQueue{}, domain.TierSystem, domain.ActorWatcher)
	ctx.ActorID = "wch_1" // present → grant attempted (nil) → denial published → queue panics

	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if res.Error.Code != "CONFIRMATION_REQUIRED" || res.Error.Recoverable {
		t.Fatalf("a panicking denial publish must not change the result, got %+v", res.Error)
	}
	if lastAudit(s).Outcome != outcomeDenied {
		t.Fatalf("blocked call audited as %s, want denied", lastAudit(s).Outcome)
	}
}

// A panicking ReportProgress must not crash dispatch (reportProgress is now
// panic-safe). The tool still runs to completion.
func TestDispatchPanicInReportProgressIsContained(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("x.echo", domain.RiskRead))
	s := &fakeStore{}

	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
	ctx.ReportProgress = func(ToolProgress) { panic("boom in progress") }

	res := r.Dispatch(context.Background(), "x.echo", json.RawMessage(`{"x":1}`), ctx)
	if !res.Ok {
		t.Fatalf("a panicking ReportProgress must not fail the call, got %+v", res.Error)
	}
}
