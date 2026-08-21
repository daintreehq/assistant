package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
)

// The tests below pin the dispatch half of target-aware invocation: that a tool
// whose ResolveTarget hook is set is gated, previewed, granted and audited as the
// TARGET it resolved, and that a tool without one is completely unaffected.

// recordingStore captures the (name, risk) pair grant consumption was attempted
// with — the fact these tests exist to check, and the one the shared fakeStore
// discards.
type recordingStore struct {
	fakeStore
	consumedName string
	consumedRisk domain.RiskClass
}

func (s *recordingStore) ConsumeGrant(_ context.Context, _ string, _ domain.AutomationGrantActorType,
	name string, risk domain.RiskClass, _ int64) (*domain.AutomationGrantRecord, error) {
	s.consumeCalled = true
	s.consumedName, s.consumedRisk = name, risk
	return s.grant, nil
}

// targetTool is a miniature target-aware invoker: `action` selects the effective
// identity and risk, mirroring how daintree.invoke resolves an MCP action name.
func targetTool(refuse *ToolResult) *Tool {
	type args struct {
		Action string `json:"action"`
	}
	risks := map[string]domain.RiskClass{
		"read.thing":  domain.RiskRead,
		"mutate.term": domain.RiskTerminal,
		"nuke.repo":   domain.RiskGit,
	}
	return &Tool{
		Name: "fake.invoke",
		// Registered at the ceiling, narrowed per call — the same fail-closed
		// arrangement daintree.invoke uses.
		Risk:        domain.RiskSystem,
		Consequence: "generic invoker consequence",
		Schema:      json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"action":{"type":"string"}}}`),
		Decode:      StrictDecoder(func() any { return &args{} }),
		ResolveTarget: func(_ context.Context, raw json.RawMessage, _ *ToolContext) (TargetInfo, *ToolResult) {
			if refuse != nil {
				return TargetInfo{}, refuse
			}
			var a args
			_ = json.Unmarshal(raw, &a)
			risk, ok := risks[a.Action]
			if !ok {
				// Unknown target: a resolver must refuse rather than default.
				res := Fail("NO_POLICY", "no policy for "+a.Action, Unrecoverable())
				return TargetInfo{}, &res
			}
			return TargetInfo{
				// Built literally rather than through domain.DynamicTargetName: dispatch
				// must be generic over any target-aware invoker, not coupled to the MCP
				// one that happens to be the only caller today.
				Name:        "fake.invoke:" + a.Action,
				Display:     a.Action,
				Risk:        risk,
				Consequence: "runs " + a.Action,
			}, nil
		},
		Handle: func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult {
			return Ok("ran", nil)
		},
	}
}

func dispatchTarget(t *testing.T, tool *Tool, store *recordingStore, tctx *ToolContext, action string) ToolResult {
	t.Helper()
	r := NewRegistry()
	if err := r.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r.Dispatch(context.Background(), tool.Name, json.RawMessage(`{"action":"`+action+`"}`), tctx)
}

// A read target must run with NO confirmation, even though the invoker itself is
// registered at system risk. This is the headline acceptance criterion: the
// generic invoker must stop being the reason a harmless read needs approval.
func TestResolveTargetReadRunsWithoutConfirmation(t *testing.T) {
	store := &recordingStore{}
	tctx := &ToolContext{
		Config: config.AppConfig{Tier: domain.TierSystem},
		DB:     store,
		Actor:  domain.ActorMain,
		Confirm: func(_ context.Context, _ ConfirmRequest) (bool, error) {
			t.Fatal("a read target must not prompt for confirmation")
			return false, nil
		},
	}
	res := dispatchTarget(t, targetTool(nil), store, tctx, "read.thing")
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if got := lastAudit(&store.fakeStore).ToolName; got != "fake.invoke:read.thing" {
		t.Errorf("audit must name the target identity, got %q", got)
	}
}

// A mutating target confirms, and the preview must describe the ACTION — not the
// invoker — or the human is approving something they cannot see.
func TestResolveTargetMutationConfirmsWithTargetPreview(t *testing.T) {
	store := &recordingStore{}
	var seen ConfirmRequest
	tctx := &ToolContext{
		Config: config.AppConfig{Tier: domain.TierSystem},
		DB:     store,
		Actor:  domain.ActorMain,
		Confirm: func(_ context.Context, req ConfirmRequest) (bool, error) {
			seen = req
			return true, nil
		},
	}
	res := dispatchTarget(t, targetTool(nil), store, tctx, "mutate.term")
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if seen.ToolName != "mutate.term" {
		t.Errorf("preview must name the target action, got %q", seen.ToolName)
	}
	if seen.Risk != domain.RiskTerminal {
		t.Errorf("preview must carry the target risk, got %q", seen.Risk)
	}
	if seen.Consequence != "runs mutate.term" {
		t.Errorf("preview must carry the target consequence, got %q", seen.Consequence)
	}
	if seen.NeedsTypedConfirm {
		t.Error("a terminal-risk target must not inherit the invoker's typed-confirm requirement")
	}
}

// A git-risk target keeps the typed-phrase requirement: narrowing works in both
// directions and must not quietly downgrade a dangerous action.
func TestResolveTargetKeepsTypedConfirmForGitRisk(t *testing.T) {
	store := &recordingStore{}
	var seen ConfirmRequest
	tctx := &ToolContext{
		Config: config.AppConfig{Tier: domain.TierSystem},
		DB:     store,
		Actor:  domain.ActorMain,
		Confirm: func(_ context.Context, req ConfirmRequest) (bool, error) {
			seen = req
			return true, nil
		},
	}
	if res := dispatchTarget(t, targetTool(nil), store, tctx, "nuke.repo"); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if !seen.NeedsTypedConfirm {
		t.Error("a git-risk target must still demand a typed confirmation")
	}
}

// The tier gate uses the TARGET's risk, so a supervisor may run a read target
// through an invoker registered at system risk...
func TestResolveTargetTierGateUsesTargetRisk(t *testing.T) {
	store := &recordingStore{}
	tctx := &ToolContext{Config: config.AppConfig{Tier: domain.TierSupervisor}, DB: store, Actor: domain.ActorMain}
	if res := dispatchTarget(t, targetTool(nil), store, tctx, "read.thing"); !res.Ok {
		t.Fatalf("supervisor must be allowed a read target: %+v", res.Error)
	}
	// ...and is still denied a git target.
	tool := targetTool(nil)
	ran := false
	inner := tool.Handle
	tool.Handle = func(ctx context.Context, a json.RawMessage, tc *ToolContext) ToolResult {
		ran = true
		return inner(ctx, a, tc)
	}
	res := dispatchTarget(t, tool, store, tctx, "nuke.repo")
	if res.Ok || res.Error == nil || res.Error.Code != codeTierDenied {
		t.Fatalf("supervisor must be tier-denied a git target, got %+v", res)
	}
	if ran {
		t.Fatal("a tier-denied call must not reach the handler")
	}
	// The denial row must name the ACTION. Auditing it under the generic invoker
	// loses the only fact that makes the row worth keeping — nobody can tell which
	// action a supervisor was refused.
	if got := lastAudit(&store.fakeStore).ToolName; got != "fake.invoke:nuke.repo" {
		t.Errorf("tier-denied audit must attribute the target, got %q", got)
	}
}

// Dispatch must publish the identity it GATED, so a handler can refuse to act
// under anything else.
func TestResolveTargetStampsGatedTarget(t *testing.T) {
	store := &recordingStore{}
	var seen *TargetInfo
	tool := targetTool(nil)
	tool.Handle = func(_ context.Context, _ json.RawMessage, tc *ToolContext) ToolResult {
		seen = tc.GatedTarget
		return Ok("ran", nil)
	}
	tctx := &ToolContext{Config: config.AppConfig{Tier: domain.TierSystem}, DB: store, Actor: domain.ActorMain}
	if res := dispatchTarget(t, tool, store, tctx, "read.thing"); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if seen == nil {
		t.Fatal("GatedTarget must be stamped for a target-aware tool")
	}
	if seen.Name != "fake.invoke:read.thing" || seen.Risk != domain.RiskRead {
		t.Errorf("GatedTarget = %+v", seen)
	}

	// And left nil for an ordinary tool, so a handler cannot mistake a stale value
	// for its own.
	plain := echoTool("plain.tool", domain.RiskRead)
	var plainSeen *TargetInfo
	plain.Handle = func(_ context.Context, _ json.RawMessage, tc *ToolContext) ToolResult {
		plainSeen = tc.GatedTarget
		return Ok("ok", nil)
	}
	r := NewRegistry()
	if err := r.Register(plain); err != nil {
		t.Fatalf("register: %v", err)
	}
	tctx2 := &ToolContext{Config: config.AppConfig{Tier: domain.TierSystem}, DB: store, Actor: domain.ActorMain}
	if res := r.Dispatch(context.Background(), "plain.tool", json.RawMessage(`{"x":1}`), tctx2); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if plainSeen != nil {
		t.Errorf("GatedTarget must stay nil for a tool with no resolver, got %+v", plainSeen)
	}
}

// Grant consumption must be attempted against the TARGET identity, so a grant can
// authorize one action without authorizing the invoker.
func TestResolveTargetGrantConsumesTargetIdentity(t *testing.T) {
	store := &recordingStore{fakeStore: fakeStore{grant: &domain.AutomationGrantRecord{ID: "g1", Source: "test"}}}
	tctx := &ToolContext{
		Config:  config.AppConfig{Tier: domain.TierSystem},
		DB:      store,
		Actor:   domain.ActorWatcher,
		ActorID: "wat_1",
	}
	res := dispatchTarget(t, targetTool(nil), store, tctx, "mutate.term")
	if !res.Ok {
		t.Fatalf("granted call should run, got %+v", res.Error)
	}
	if store.consumedName != "fake.invoke:mutate.term" {
		t.Errorf("grant must be consumed on the target identity, got %q", store.consumedName)
	}
	if store.consumedRisk != domain.RiskTerminal {
		t.Errorf("grant must be consumed at the target risk, got %q", store.consumedRisk)
	}
	rec := lastAudit(&store.fakeStore)
	if rec.Outcome != outcomeGrantOK || rec.GrantID == nil || *rec.GrantID != "g1" {
		t.Errorf("grant provenance missing from the audit row: %+v", rec)
	}
	if rec.ToolName != "fake.invoke:mutate.term" {
		t.Errorf("audit must attribute the target, got %q", rec.ToolName)
	}
}

// Without a grant the non-interactive actor is blocked, and the recommended
// grant.create must pre-fill the TARGET identity — a suggestion naming the bare
// invoker would be rejected by grant.create as ungrantable, i.e. a one-click
// unblock that silently fails.
func TestResolveTargetDenialRecommendsTargetScopedGrant(t *testing.T) {
	store := &recordingStore{}
	q := &fakeQueue{}
	tctx := &ToolContext{
		Config:  config.AppConfig{Tier: domain.TierSystem},
		DB:      store,
		Queue:   q,
		Actor:   domain.ActorWatcher,
		ActorID: "wat_1",
	}
	res := dispatchTarget(t, targetTool(nil), store, tctx, "mutate.term")
	if res.Ok || res.Error == nil || res.Error.Code != codeConfirmRequired {
		t.Fatalf("ungranted mutation must be blocked, got %+v", res)
	}
	if len(q.published) != 1 {
		t.Fatalf("expected one blocked-inbox event, got %d", len(q.published))
	}
	ev := q.published[0]
	if !strings.Contains(ev.Title, "mutate.term") {
		t.Errorf("blocked title must name the action, got %q", ev.Title)
	}
	if len(ev.RecommendedActions) != 1 {
		t.Fatalf("expected a grant.create recommendation, got %+v", ev.RecommendedActions)
	}
	args, ok := ev.RecommendedActions[0].Args.(map[string]any)
	if !ok {
		t.Fatalf("recommendation args have the wrong shape: %T", ev.RecommendedActions[0].Args)
	}
	names, _ := args["allowedToolNames"].([]string)
	if len(names) != 1 || names[0] != "fake.invoke:mutate.term" {
		t.Errorf("recommended grant must name the target identity, got %v", names)
	}
}

// A resolver refusal short-circuits: the handler never runs, and the refusal is
// audited as denied rather than lost.
func TestResolveTargetRefusalBlocksHandler(t *testing.T) {
	store := &recordingStore{}
	tctx := &ToolContext{Config: config.AppConfig{Tier: domain.TierSystem}, DB: store, Actor: domain.ActorMain}
	res := dispatchTarget(t, targetTool(nil), store, tctx, "unclassified.action")
	if res.Ok || res.Error == nil || res.Error.Code != "NO_POLICY" {
		t.Fatalf("unclassified target must be refused by the resolver, got %+v", res)
	}
	if got := lastAudit(&store.fakeStore).Outcome; got != outcomeDenied {
		t.Errorf("a resolver refusal must audit as denied, got %q", got)
	}
}

// A resolver that returns a blank identity is a bug that could only ever widen
// authority, so dispatch refuses rather than silently falling back to the static
// (system) risk — which would run the handler.
func TestResolveTargetBlankIdentityFailsClosed(t *testing.T) {
	store := &recordingStore{}
	tool := targetTool(nil)
	tool.ResolveTarget = func(_ context.Context, _ json.RawMessage, _ *ToolContext) (TargetInfo, *ToolResult) {
		return TargetInfo{}, nil
	}
	ran := false
	tool.Handle = func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult {
		ran = true
		return Ok("ran", nil)
	}
	tctx := &ToolContext{Config: config.AppConfig{Tier: domain.TierSystem}, DB: store, Actor: domain.ActorMain,
		Confirm: func(_ context.Context, _ ConfirmRequest) (bool, error) { return true, nil }}
	res := dispatchTarget(t, tool, store, tctx, "read.thing")
	if res.Ok {
		t.Fatal("a blank resolved identity must not run")
	}
	if ran {
		t.Fatal("handler ran despite an unresolvable identity")
	}
}

// recordingObserver captures what the workflow-evidence layer was told.
type recordingObserver struct{ seen []DispatchObservation }

func (o *recordingObserver) ObserveDispatch(_ context.Context, obs DispatchObservation) {
	o.seen = append(o.seen, obs)
}

// The observer must be told the risk the call was GATED at. It used to re-derive
// risk from the registry, which has no row for a composite identity — so a
// successful dynamic mutation arrived with an empty risk, read as non-material,
// and disappeared from workflow evidence while auditing correctly in SQLite.
func TestResolveTargetObserverSeesEffectiveRisk(t *testing.T) {
	store := &recordingStore{}
	obs := &recordingObserver{}
	tctx := &ToolContext{
		Config:  config.AppConfig{Tier: domain.TierSystem},
		DB:      store,
		Actor:   domain.ActorMain,
		Confirm: func(_ context.Context, _ ConfirmRequest) (bool, error) { return true, nil },
	}
	r := NewRegistry()
	if err := r.Register(targetTool(nil)); err != nil {
		t.Fatalf("register: %v", err)
	}
	r.SetDispatchObserver(obs)
	if res := r.Dispatch(context.Background(), "fake.invoke",
		json.RawMessage(`{"action":"mutate.term"}`), tctx); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(obs.seen) != 1 {
		t.Fatalf("expected one observation, got %d", len(obs.seen))
	}
	if obs.seen[0].Risk != domain.RiskTerminal {
		t.Errorf("observed risk = %q, want terminal (the risk it was gated at)", obs.seen[0].Risk)
	}
	if obs.seen[0].ToolName != "fake.invoke:mutate.term" {
		t.Errorf("observed tool = %q, want the target identity", obs.seen[0].ToolName)
	}
}

// A panic AFTER resolution must still be audited against the action that was being
// gated. The firewall used to close over the static name, so the one row explaining
// what blew up named the invoker instead.
func TestResolveTargetPanicAfterResolutionAuditsTarget(t *testing.T) {
	store := &recordingStore{}
	tool := targetTool(nil)
	tool.Handle = func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult {
		panic("boom")
	}
	tctx := &ToolContext{Config: config.AppConfig{Tier: domain.TierSystem}, DB: store, Actor: domain.ActorMain}
	res := dispatchTarget(t, tool, store, tctx, "read.thing")
	if res.Ok || res.Error == nil || res.Error.Code != codeToolThrew {
		t.Fatalf("a panicking handler must surface as TOOL_THREW, got %+v", res)
	}
	rec := lastAudit(&store.fakeStore)
	if rec.ToolName != "fake.invoke:read.thing" {
		t.Errorf("panic audit must attribute the target, got %q", rec.ToolName)
	}
}

// The pipeline is unchanged for every ordinary tool: nil ResolveTarget means the
// static registration is the whole story.
func TestNilResolveTargetLeavesStaticPipelineIntact(t *testing.T) {
	store := &recordingStore{}
	var seen ConfirmRequest
	tctx := &ToolContext{
		Config: config.AppConfig{Tier: domain.TierSystem},
		DB:     store,
		Actor:  domain.ActorMain,
		Confirm: func(_ context.Context, req ConfirmRequest) (bool, error) {
			seen = req
			return true, nil
		},
	}
	r := NewRegistry()
	if err := r.Register(echoTool("plain.tool", domain.RiskTerminal)); err != nil {
		t.Fatalf("register: %v", err)
	}
	res := r.Dispatch(context.Background(), "plain.tool", json.RawMessage(`{"x":1}`), tctx)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if seen.ToolName != "plain.tool" || seen.Risk != domain.RiskTerminal {
		t.Errorf("static tool preview drifted: %+v", seen)
	}
	if got := lastAudit(&store.fakeStore).ToolName; got != "plain.tool" {
		t.Errorf("static tool must audit under its own name, got %q", got)
	}
}

// A target-aware tool's STATIC Risk is a fail-closed ceiling, and two surfaces read
// it without ever being able to run the resolver: the read-only sub-agent inventory
// filter and parallel dispatch. Parallel dispatch is the half that can be pinned
// structurally — a tool whose risk is decided per call cannot also declare itself
// safe to run concurrently on the basis of the risk it happened to register at.
// Until now the rule lived only in a comment on Tool.ResolveTarget, and a comment
// has never stopped a field being set.
func TestAssertSafeRejectsAParallelTargetAwareTool(t *testing.T) {
	resolver := func(_ context.Context, _ json.RawMessage, _ *ToolContext) (TargetInfo, *ToolResult) {
		return TargetInfo{Name: "mcp.dynamic:terminal.list", Risk: domain.RiskRead}, nil
	}
	handle := func(_ context.Context, _ json.RawMessage, _ *ToolContext) ToolResult { return Ok("ok", nil) }

	// The shape daintree.invoke actually has: a resolver, no parallel flags. Legal.
	ok := NewRegistry()
	if err := ok.Register(&Tool{Name: "mcp.invoke", Risk: domain.RiskSystem,
		ResolveTarget: resolver, Handle: handle}); err != nil {
		t.Fatal(err)
	}
	if err := ok.AssertSafe(); err != nil {
		t.Fatalf("a non-parallel target-aware tool must be accepted: %v", err)
	}

	for _, tc := range []struct {
		name string
		tool *Tool
	}{
		{"Parallelizable", &Tool{Name: "mcp.invokeParallel", Risk: domain.RiskRead,
			ResolveTarget: resolver, Parallelizable: true, Handle: handle}},
		{"ParallelHomogeneous", &Tool{Name: "mcp.invokeHomogeneous", Risk: domain.RiskSystem,
			ResolveTarget: resolver, ParallelHomogeneous: true, Handle: handle}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			if err := r.Register(tc.tool); err != nil {
				t.Fatal(err)
			}
			err := r.AssertSafe()
			if err == nil {
				t.Fatalf("AssertSafe must reject a target-aware tool declaring %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.tool.Name) {
				t.Errorf("the error must name the offending tool, got %q", err)
			}
		})
	}
}
