package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
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

func baseCtx(store *fakeStore, q Queue, tier domain.Tier, actor domain.ToolActor) *ToolContext {
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

// A near-miss tool name (a typo / hallucinated wire form) gets a "did you mean?"
// hint naming the closest registered tool, so the model can self-correct.
func TestDispatchUnknownToolSuggestsClosest(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("fs.read", domain.RiskRead))
	s := &fakeStore{}
	// One-character typo of the wire form: must point back at fs.read.
	res := r.Dispatch(context.Background(), "fs__raed", nil, baseCtx(s, nil, domain.TierSystem, domain.ActorMain))
	if res.Ok || res.Error.Code != "UNKNOWN_TOOL" {
		t.Fatalf("want UNKNOWN_TOOL, got %+v", res.Error)
	}
	if !strings.Contains(res.Error.Message, "fs.read") || !strings.Contains(res.Error.Message, "Did you mean") {
		t.Fatalf("expected a did-you-mean hint naming fs.read, got %q", res.Error.Message)
	}
	// A wildly different name has no close neighbor → no misleading suggestion.
	res2 := r.Dispatch(context.Background(), "zzzzzzzzzz.qqqqq", nil, baseCtx(s, nil, domain.TierSystem, domain.ActorMain))
	if strings.Contains(res2.Error.Message, "Did you mean") {
		t.Fatalf("a far-off name must not get a suggestion, got %q", res2.Error.Message)
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
	// INVALID_ARGS, not TIER_DENIED (validation precedes the tier gate).
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
	// A returned error must be treated as a decline, never approval.
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

// The confirm prompt carries the pre-computed NeedsTypedConfirm verdict so every
// surface enforces the typed-phrase requirement without re-deriving it: git/system
// → true, terminal → false.
func TestDispatchConfirmCarriesNeedsTypedConfirm(t *testing.T) {
	cases := []struct {
		risk domain.RiskClass
		want bool
	}{
		{domain.RiskGit, true},
		{domain.RiskSystem, true},
		{domain.RiskTerminal, false},
	}
	for _, c := range cases {
		r := NewRegistry()
		_ = r.Register(echoTool("c.echo", c.risk))
		s := &fakeStore{}
		ctx := baseCtx(s, nil, domain.TierSystem, domain.ActorMain)
		var got ConfirmRequest
		ctx.Confirm = func(_ context.Context, req ConfirmRequest) (bool, error) { got = req; return true, nil }
		res := r.Dispatch(context.Background(), "c.echo", json.RawMessage(`{"x":1}`), ctx)
		if !res.Ok {
			t.Fatalf("risk %s: approved call should succeed, got %+v", c.risk, res.Error)
		}
		if got.NeedsTypedConfirm != c.want {
			t.Errorf("risk %s: ConfirmRequest.NeedsTypedConfirm = %v, want %v", c.risk, got.NeedsTypedConfirm, c.want)
		}
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
	// A blocked autonomous action must cross the attention filter (SeverityBlocked),
	// not sit silently as info.
	if q.published[0].Severity != domain.SeverityBlocked {
		t.Fatalf("denial event should be SeverityBlocked, got %q", q.published[0].Severity)
	}
	// With no actorId there's nothing to scope a grant to, so no recommended action.
	if len(q.published[0].RecommendedActions) != 0 {
		t.Fatalf("no actorId → no recommended action, got %d", len(q.published[0].RecommendedActions))
	}
}

// A watcher/timer WITH an actorId but no matching grant is blocked AND handed a
// one-click grant.create recommendation, pre-filled with the exact scope to
// authorize just this tool — so the human can approve in a single step.
func TestDispatchNonInteractiveBlockedRecommendsGrant(t *testing.T) {
	cases := []struct {
		name      string
		actor     domain.ToolActor
		actorID   string
		actorType string
	}{
		{"watcher", domain.ActorWatcher, "wch_1", "watcher"},
		{"timer", domain.ActorTimer, "tmr_9", "timer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			_ = r.Register(echoTool("g.echo", domain.RiskGit))
			// grant nil → ConsumeGrant returns no grant → blocked path.
			s := &fakeStore{}
			q := &fakeQueue{}
			ctx := baseCtx(s, q, domain.TierSystem, tc.actor)
			ctx.ActorID = tc.actorID
			res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
			if res.Error.Code != "CONFIRMATION_REQUIRED" || res.Error.Recoverable {
				t.Fatalf("want CONFIRMATION_REQUIRED non-recoverable, got %+v", res.Error)
			}
			if !s.consumeCalled {
				t.Fatal("grant consume should be attempted when an actorId is present")
			}
			if len(q.published) != 1 {
				t.Fatalf("a denial event should be published, got %d", len(q.published))
			}
			ev := q.published[0]
			if ev.Severity != domain.SeverityBlocked {
				t.Fatalf("denial event should be SeverityBlocked, got %q", ev.Severity)
			}
			// The actorId segment keeps distinct actors from collapsing together.
			wantKey := "denied:" + string(tc.actor) + ":" + tc.actorID + ":g.echo"
			if ev.DedupeKey != wantKey {
				t.Fatalf("dedupeKey wrong: got %q want %q", ev.DedupeKey, wantKey)
			}
			if len(ev.RecommendedActions) != 1 {
				t.Fatalf("want exactly one recommended action, got %d", len(ev.RecommendedActions))
			}
			act := ev.RecommendedActions[0]
			if act.ToolName != "grant.create" {
				t.Fatalf("recommended action should target grant.create, got %q", act.ToolName)
			}
			if act.Risk != domain.RiskLocal {
				t.Fatalf("recommended action risk should match grant.create (local), got %q", act.Risk)
			}
			if !act.RequiresConfirmation {
				t.Fatal("minting a mutating grant must require confirmation")
			}
			args, ok := act.Args.(map[string]any)
			if !ok {
				t.Fatalf("recommended action args should be a map, got %T", act.Args)
			}
			if args["actorId"] != tc.actorID {
				t.Fatalf("args.actorId = %v, want %q", args["actorId"], tc.actorID)
			}
			if args["actorType"] != tc.actorType {
				t.Fatalf("args.actorType = %v, want %q", args["actorType"], tc.actorType)
			}
			tools, ok := args["allowedToolNames"].([]string)
			if !ok || len(tools) != 1 || tools[0] != "g.echo" {
				t.Fatalf("args.allowedToolNames should be [g.echo], got %v", args["allowedToolNames"])
			}
			if args["ttlMs"] != defaultGrantTTLMs {
				t.Fatalf("args.ttlMs = %v, want %d", args["ttlMs"], defaultGrantTTLMs)
			}
			if args["maxUses"] != defaultGrantMaxUses {
				t.Fatalf("args.maxUses = %v, want %d", args["maxUses"], defaultGrantMaxUses)
			}
		})
	}
}

// A watcher blocked on an UNGRANTABLE tool (e.g. daintree.call) is still blocked
// with SeverityBlocked, but carries NO grant.create recommendation — grant.create
// would reject that tool, so a one-click "unblock" that silently fails is worse
// than offering nothing.
func TestDispatchNonInteractiveBlockedUngrantableToolNoAction(t *testing.T) {
	r := NewRegistry()
	// daintree.call is in the ungrantable set; give it a confirm-required risk so it
	// reaches Branch A.
	_ = r.Register(echoTool("daintree.call", domain.RiskGit))
	s := &fakeStore{}
	q := &fakeQueue{}
	ctx := baseCtx(s, q, domain.TierSystem, domain.ActorWatcher)
	ctx.ActorID = "wch_1"
	res := r.Dispatch(context.Background(), "daintree.call", json.RawMessage(`{"x":1}`), ctx)
	if res.Error.Code != "CONFIRMATION_REQUIRED" {
		t.Fatalf("want CONFIRMATION_REQUIRED, got %+v", res.Error)
	}
	if len(q.published) != 1 {
		t.Fatalf("a denial event should be published, got %d", len(q.published))
	}
	if q.published[0].Severity != domain.SeverityBlocked {
		t.Fatalf("denial event should be SeverityBlocked, got %q", q.published[0].Severity)
	}
	if len(q.published[0].RecommendedActions) != 0 {
		t.Fatalf("ungrantable tool must not get a grant.create recommendation, got %d",
			len(q.published[0].RecommendedActions))
	}
}

// A non-interactive actor that is neither watcher nor timer (e.g. workflow) is
// blocked with SeverityBlocked but gets no grant.create recommendation —
// grant.create's actorType only accepts watcher|timer, so a suggestion for any
// other actor would be invalid.
func TestDispatchNonInteractiveBlockedNonGrantableActorNoAction(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{}
	q := &fakeQueue{}
	ctx := baseCtx(s, q, domain.TierSystem, domain.ActorWorkflow)
	ctx.ActorID = "wf_1"
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if res.Error.Code != "CONFIRMATION_REQUIRED" {
		t.Fatalf("want CONFIRMATION_REQUIRED, got %+v", res.Error)
	}
	if len(q.published) != 1 {
		t.Fatalf("a denial event should be published, got %d", len(q.published))
	}
	if q.published[0].Severity != domain.SeverityBlocked {
		t.Fatalf("denial event should be SeverityBlocked, got %q", q.published[0].Severity)
	}
	if len(q.published[0].RecommendedActions) != 0 {
		t.Fatalf("non-grantable actor must not get a grant.create recommendation, got %d",
			len(q.published[0].RecommendedActions))
	}
}

// The happy grant path must NOT publish a denial event — a blocked-event leak
// here would spam the inbox on every authorized autonomous call.
func TestDispatchNonInteractiveGrantOKPublishesNoDenial(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	s := &fakeStore{grant: &domain.AutomationGrantRecord{ID: "grt_abc", Source: domain.GrantSourceLocal}}
	q := &fakeQueue{}
	ctx := baseCtx(s, q, domain.TierSystem, domain.ActorWatcher)
	ctx.ActorID = "wch_1"
	res := r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	if !res.Ok {
		t.Fatalf("grant should authorize the call, got %+v", res.Error)
	}
	if len(q.published) != 0 {
		t.Fatalf("an authorized call must publish no denial event, got %d", len(q.published))
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
