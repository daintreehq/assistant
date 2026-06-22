package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// --- fakes (satisfy the consumer-defined interfaces structurally) ---

type fakeStore struct {
	audits        []domain.AuditRecord
	grant         *domain.AutomationGrantRecord
	consumeCalled bool
}

func (s *fakeStore) InsertAudit(_ context.Context, rec domain.AuditRecord) (string, error) {
	s.audits = append(s.audits, rec)
	return rec.ID, nil
}
func (s *fakeStore) ConsumeGrant(_ context.Context, _ string, _ domain.AutomationGrantActorType,
	_ string, _ domain.RiskClass, _ int64) (*domain.AutomationGrantRecord, error) {
	s.consumeCalled = true
	return s.grant, nil
}

type fakeQueue struct{ published []domain.QueuePublishArgs }

func (q *fakeQueue) Publish(_ context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	q.published = append(q.published, args)
	return domain.QueueEvent{}, nil
}

func lastAudit(s *fakeStore) domain.AuditRecord { return s.audits[len(s.audits)-1] }

func baseCtx(store *fakeStore, q *fakeQueue, tier domain.Tier, actor domain.ToolActor) *ToolContext {
	return &ToolContext{
		Config: config.AppConfig{Tier: tier},
		DB:     store,
		Queue:  q,
		Actor:  actor,
	}
}

// echoTool returns ok and echoes its decoded args; used to exercise the pipeline.
func echoTool(name string, risk domain.RiskClass) *Tool {
	type args struct {
		X int `json:"x"`
	}
	return &Tool{
		Name: name, Risk: risk,
		Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"x":{"type":"integer"}}}`),
		Decode: StrictDecoder(func() any { return &args{} }),
		Handle: func(_ context.Context, a json.RawMessage, _ *ToolContext) ToolResult {
			return Ok("ok", json.RawMessage(a))
		},
	}
}

func TestDispatchUnknownTool(t *testing.T) {
	r := NewRegistry()
	s := &fakeStore{}
	res := r.Dispatch(context.Background(), "nope", nil, baseCtx(s, nil, domain.TierSystem, domain.ActorMain))
	if res.Ok || res.Error.Code != "UNKNOWN_TOOL" || res.Error.Recoverable {
		t.Fatalf("want UNKNOWN_TOOL non-recoverable, got %+v", res.Error)
	}
	if lastAudit(s).Outcome != outcomeError {
		t.Fatalf("unknown tool audited as %s", lastAudit(s).Outcome)
	}
}

func TestDispatchInvalidArgsRejectsUnknownFields(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("x.echo", domain.RiskRead))
	s := &fakeStore{}
	// Unknown field "y" must be rejected (strict decode), BEFORE any tier gate.
	res := r.Dispatch(context.Background(), "x.echo", json.RawMessage(`{"x":1,"y":2}`),
		baseCtx(s, nil, domain.TierSystem, domain.ActorMain))
	if res.Ok || res.Error.Code != "INVALID_ARGS" {
		t.Fatalf("want INVALID_ARGS, got %+v", res.Error)
	}
	if !res.Error.Recoverable {
		t.Fatal("INVALID_ARGS must be recoverable")
	}
}

func TestDispatchArgsValidatedBeforeTierGate(t *testing.T) {
	r := NewRegistry()
	// A git tool requires system tier; with supervisor tier + bad args we must see
	// INVALID_ARGS, not TIER_DENIED (§14.2 ordering).
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"bad":1}`),
		baseCtx(s, nil, domain.TierSupervisor, domain.ActorMain))
	if res.Error.Code != "INVALID_ARGS" {
		t.Fatalf("want INVALID_ARGS before tier gate, got %s", res.Error.Code)
	}
}

func TestDispatchTierDenied(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`),
		baseCtx(s, nil, domain.TierSupervisor, domain.ActorMain))
	if res.Ok || res.Error.Code != "TIER_DENIED" || res.Error.Recoverable {
		t.Fatalf("want TIER_DENIED non-recoverable, got %+v", res.Error)
	}
	if lastAudit(s).Outcome != outcomeDenied {
		t.Fatalf("tier-denied audited as %s", lastAudit(s).Outcome)
	}
}

func TestDispatchMainConfirmDeclined(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
	ctx.Confirm = func(_ context.Context, _ ConfirmRequest) (bool, error) { return false, nil }
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if res.Error.Code != "USER_DECLINED" {
		t.Fatalf("want USER_DECLINED, got %s", res.Error.Code)
	}
	if lastAudit(s).Outcome != outcomeDenied {
		t.Fatalf("declined audited as %s", lastAudit(s).Outcome)
	}
}

func TestDispatchConfirmThrowIsDecline(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
	// A returned error must be treated as a decline, never approval (§14.7).
	ctx.Confirm = func(_ context.Context, _ ConfirmRequest) (bool, error) { return true, errors.New("boom") }
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if res.Error.Code != "USER_DECLINED" {
		t.Fatalf("errored confirm must decline, got %s", res.Error.Code)
	}
}

func TestDispatchMainConfirmApproved(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
	ctx.Confirm = func(_ context.Context, _ ConfirmRequest) (bool, error) { return true, nil }
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if !res.Ok {
		t.Fatalf("approved call should succeed, got %+v", res.Error)
	}
	if lastAudit(s).Outcome != outcomeOK {
		t.Fatalf("approved audited as %s", lastAudit(s).Outcome)
	}
	if res.AuditID == "" {
		t.Fatal("audit id should be stamped onto the result")
	}
}

func TestDispatchAutoApproveSkipsPrompt(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
	ctx.Config.AutoApprove = true
	confirmCalled := false
	ctx.Confirm = func(_ context.Context, _ ConfirmRequest) (bool, error) { confirmCalled = true; return false, nil }
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if !res.Ok {
		t.Fatalf("autoApprove should run, got %+v", res.Error)
	}
	if confirmCalled {
		t.Fatal("autoApprove must skip the confirm prompt")
	}
}

func TestDispatchNonInteractiveBlockedNoGrant(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	q := &fakeQueue{}
	// Watcher actor with NO actorId → cannot even attempt grant → CONFIRMATION_REQUIRED.
	ctx := baseCtx(s, q, domain.TierSystem, domain.ActorWatcher)
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if res.Error.Code != "CONFIRMATION_REQUIRED" || res.Error.Recoverable {
		t.Fatalf("want CONFIRMATION_REQUIRED non-recoverable, got %+v", res.Error)
	}
	if s.consumeCalled {
		t.Fatal("grant consume must NOT be attempted without an actorId")
	}
	if len(q.published) != 1 {
		t.Fatalf("a denial event should be published, got %d", len(q.published))
	}
	if q.published[0].DedupeKey != "denied:watcher:g.echo" {
		t.Fatalf("tick-free dedupeKey wrong: %q", q.published[0].DedupeKey)
	}
}

func TestDispatchNonInteractiveGrantOK(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{grant: &domain.AutomationGrantRecord{ID: "grt_abc", Source: domain.GrantSourceLocal}}
	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorWatcher)
	ctx.ActorID = "wch_1"
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if !res.Ok {
		t.Fatalf("grant should authorize the call, got %+v", res.Error)
	}
	a := lastAudit(s)
	if a.Outcome != outcomeGrantOK {
		t.Fatalf("want grant_ok outcome, got %s", a.Outcome)
	}
	if a.GrantID == nil || *a.GrantID != "grt_abc" || a.GrantSource == nil {
		t.Fatalf("grant provenance must be stamped on a grant_ok row: %+v", a)
	}
}

func TestDispatchGrantFailureAuditedError(t *testing.T) {
	r := NewRegistry()
	// Tool that fails: grant-authorized failure must audit as error, not grant_ok.
	failing := &Tool{Name: "g.fail", Risk: domain.RiskGit,
		Handle: func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult {
			return Fail("BOOM", "nope")
		}}
	_ = r.Register(failing)
	s := &fakeStore{grant: &domain.AutomationGrantRecord{ID: "grt_x", Source: domain.GrantSourceLocal}}
	ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorWatcher)
	ctx.ActorID = "wch_1"
	r.Dispatch(context.Background(), "g.fail", nil, ctx)
	a := lastAudit(s)
	if a.Outcome != outcomeError {
		t.Fatalf("grant-authorized failure must audit as error, got %s", a.Outcome)
	}
	if a.GrantSource != nil || a.GrantID != nil {
		t.Fatal("a non-grant_ok row must not carry grant provenance")
	}
}

func TestDispatchPanicRecovered(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&Tool{Name: "p.panic", Risk: domain.RiskRead,
		Handle: func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult {
			panic("kaboom")
		}})
	s := &fakeStore{}
	res := r.Dispatch(context.Background(), "p.panic", nil, baseCtx(s, nil, domain.TierSystem, domain.ActorMain))
	if res.Ok || res.Error.Code != "TOOL_THREW" {
		t.Fatalf("panic should convert to TOOL_THREW, got %+v", res.Error)
	}
	if !res.Error.Recoverable {
		t.Fatal("TOOL_THREW is recoverable")
	}
}
