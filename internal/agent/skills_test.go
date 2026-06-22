package agent

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// realRegistry loads the embedded built-in skills (the same registry the app uses)
// so control-message + tool-filter assertions run against real skill bodies.
func realRegistry(t *testing.T) *skills.SkillRegistry {
	t.Helper()
	reg, err := skills.BuiltinRegistry()
	if err != nil {
		t.Fatalf("builtin registry: %v", err)
	}
	return reg
}

// selStub returns a fixed selection from skill.find.
type selStub struct {
	sel skills.SkillSelection
	err error
}

func (s selStub) Select(context.Context, []skills.SkillMetadata, string) (skills.SkillSelection, error) {
	return s.sel, s.err
}

// captureStreamTools records the projected tool wire-names + promptCacheKey from a
// stream so the per-turn projection can be asserted.
type captureStreamTools struct {
	*fakeTools
	full []models.ChatTool // returned by OpenAITools for a nil filter
	last []string          // internal names of the last OpenAITools projection
}

func (c *captureStreamTools) OpenAITools(filter []string) ([]models.ChatTool, error) {
	if filter == nil {
		c.last = nil
		out := make([]models.ChatTool, len(c.full))
		copy(out, c.full)
		return out, nil
	}
	c.last = append([]string{}, filter...)
	out := make([]models.ChatTool, 0, len(filter))
	for _, n := range filter {
		out = append(out, models.ChatTool{Function: models.ChatToolFunc{Name: strings.ReplaceAll(n, ".", "__")}})
	}
	return out, nil
}

// skillSession builds a session backed by the real skill registry.
func skillSession(t *testing.T, reg *skills.SkillRegistry, r Router, tr ToolRunner) *Session {
	t.Helper()
	deps := SessionDeps{
		Router:        r,
		Tools:         tr,
		SkillSelector: fakeSelector{},
		SkillCatalog:  reg,
		SessionID:     "ses_skills",
		Events:        NoopEventSink{},
		PromptContext: prompts.MainPromptContext{
			Tier: domain.TierOperator, ProjectPath: "/proj",
			MCPConnected: true, MCPStatusLine: "connected",
			LargeModel: "large-x", SmallModel: "small-x", SchedulerActive: true,
		},
	}
	return NewSession(deps)
}

func plainRouter() *fakeRouter {
	return &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
}

// --- control messages ---

func TestControlMessagesIndexing(t *testing.T) {
	s := skillSession(t, realRegistry(t), plainRouter(), &fakeTools{})
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("control messages = %d want 3", len(msgs))
	}
	for i, m := range msgs {
		if m.Role != "system" {
			t.Fatalf("control[%d] role = %q want system", i, m.Role)
		}
	}
	if !strings.Contains(msgs[0].StringContent, "Daintree Assistant") {
		t.Fatal("control[0] should be the base prompt")
	}
	if !strings.Contains(msgs[1].StringContent, "# Runtime context") {
		t.Fatal("control[1] should hold runtime context")
	}
	// The skill catalog rides along in message[1].
	if !strings.Contains(msgs[1].StringContent, "# Skill catalog") ||
		!strings.Contains(msgs[1].StringContent, skills.IDSpawnAgentForEdits) {
		t.Fatal("control[1] should hold the skill catalog with every skill id")
	}
	if !strings.Contains(msgs[2].StringContent, "# Loaded skills") {
		t.Fatal("control[2] should be the loaded-skills slot")
	}
}

func TestRefreshRuntimeContextRewritesOnlyIndex1(t *testing.T) {
	s := skillSession(t, realRegistry(t), plainRouter(), &fakeTools{})
	before := s.Messages()
	base, loaded := before[0].StringContent, before[2].StringContent
	ctx := prompts.MainPromptContext{
		Tier: domain.TierSupervisor, ProjectPath: "/proj",
		MCPConnected: true, MCPStatusLine: "connected",
		LargeModel: "L", SmallModel: "S", SchedulerActive: true,
	}
	s.RefreshRuntimeContext(ctx)
	after := s.Messages()
	if after[0].StringContent != base || after[2].StringContent != loaded {
		t.Fatal("refreshRuntimeContext must rewrite only index 1")
	}
	if !strings.Contains(after[1].StringContent, "supervisor") {
		t.Fatal("index 1 should reflect the new tier")
	}
}

func TestSetSkillsRewritesOnlyIndex2(t *testing.T) {
	s := skillSession(t, realRegistry(t), plainRouter(), &fakeTools{})
	before := s.Messages()
	base, runtime := before[0].StringContent, before[1].StringContent
	s.SetSkills([]string{skills.IDSpawnAgentForEdits})
	after := s.Messages()
	if after[0].StringContent != base || after[1].StringContent != runtime {
		t.Fatal("setSkills must rewrite only index 2")
	}
	if !strings.Contains(after[2].StringContent, "Spawn a visible agent") {
		t.Fatal("index 2 should hold the loaded skill body")
	}
	if got := s.ActiveSkillIDs(); len(got) != 1 || got[0] != skills.IDSpawnAgentForEdits {
		t.Fatalf("active skills = %v", got)
	}
}

func TestLoadAdditionalSkillsRewritesOnlyIndex2(t *testing.T) {
	s := skillSession(t, realRegistry(t), plainRouter(), &fakeTools{})
	before := s.Messages()
	base, runtime := before[0].StringContent, before[1].StringContent
	active := s.LoadAdditionalSkills([]string{skills.IDSpawnAgentForEdits})
	after := s.Messages()
	if after[0].StringContent != base || after[1].StringContent != runtime {
		t.Fatal("loadAdditionalSkills must rewrite only index 2")
	}
	if !strings.Contains(after[2].StringContent, "Skill id: "+skills.IDSpawnAgentForEdits) {
		t.Fatal("index 2 should name the loaded skill id")
	}
	if len(active) != 1 || active[0] != skills.IDSpawnAgentForEdits {
		t.Fatalf("active = %v", active)
	}
}

func TestLoadAdditionalSkillsPrioritizesExplicitAtCap(t *testing.T) {
	s := skillSession(t, realRegistry(t), plainRouter(), &fakeTools{})
	s.SetSkills([]string{
		skills.IDBasicOrchestration,
		skills.IDRecipeRunner,
		skills.IDSpawnAgentForEdits,
	})
	if len(s.ActiveSkillIDs()) != 3 {
		t.Fatalf("expected the cap of 3, got %v", s.ActiveSkillIDs())
	}
	// A fourth explicit load merges FIRST, survives the cap, and evicts the
	// lowest-priority prior id (recipe runner, last in the pre-cap merge).
	active := s.LoadAdditionalSkills([]string{skills.IDWorkflowStartWork})
	if len(active) != 3 {
		t.Fatalf("active len = %d want 3", len(active))
	}
	if !containsStr(active, skills.IDWorkflowStartWork) {
		t.Fatal("explicit load must survive the cap")
	}
	if containsStr(active, skills.IDRecipeRunner) {
		t.Fatal("the evicted id should fall off the end of the merge")
	}
}

func TestSetSkillsDropsUnknownIDs(t *testing.T) {
	s := skillSession(t, realRegistry(t), plainRouter(), &fakeTools{})
	s.SetSkills([]string{"nope.not.real"})
	if len(s.ActiveSkillIDs()) != 0 {
		t.Fatalf("unknown ids should resolve to none, got %v", s.ActiveSkillIDs())
	}
	if !strings.Contains(s.Messages()[2].StringContent, "No task-specific skills") {
		t.Fatal("empty bundle should render the fallback body")
	}
}

func TestResolveKnownIDsDoesNotPushKnownOutOfCap(t *testing.T) {
	s := skillSession(t, realRegistry(t), plainRouter(), &fakeTools{})
	s.SetSkills([]string{"x.unknown.1", "x.unknown.2", "x.unknown.3", skills.IDBasicOrchestration})
	if got := s.ActiveSkillIDs(); len(got) != 1 || got[0] != skills.IDBasicOrchestration {
		t.Fatalf("unknown ids must not evict the known one: %v", got)
	}
}

func TestSendPassesStablePromptCacheKey(t *testing.T) {
	captured := make(chan string, 1)
	r := &cacheKeyRouter{captured: captured}
	s := skillSession(t, realRegistry(t), r, &fakeTools{})
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case key := <-captured:
		if key != domain.MainPromptCacheKey {
			t.Fatalf("promptCacheKey = %q want %q", key, domain.MainPromptCacheKey)
		}
	default:
		t.Fatal("stream was not called")
	}
}

func TestSendAppendsTurnsAfterControls(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "hi"}}}
	s := skillSession(t, realRegistry(t), r, &fakeTools{})
	if _, err := s.Send(context.Background(), "hello there", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if msgs[3].Role != "user" || msgs[3].StringContent != "hello there" {
		t.Fatalf("msg[3] = %+v want user/hello there", msgs[3])
	}
	if msgs[4].Role != "assistant" {
		t.Fatalf("msg[4] role = %q want assistant", msgs[4].Role)
	}
}

// --- tool projection (buildToolFilter) ---

func TestSendFullRegistryWhenNoSkillActive(t *testing.T) {
	full := []models.ChatTool{
		{Function: models.ChatToolFunc{Name: "fs__read"}},
		{Function: models.ChatToolFunc{Name: "timer__schedule"}},
	}
	tools := &captureStreamTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}, full: full}
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s := skillSession(t, realRegistry(t), r, tools)
	if _, err := s.Send(context.Background(), "simple question", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// No skill ⇒ nil filter ⇒ the full registry is offered (last==nil).
	if tools.last != nil {
		t.Fatalf("expected a nil (full-registry) filter, got %v", tools.last)
	}
}

func TestSendPrunesToCoreUnionRequiredWhenSkillActive(t *testing.T) {
	tools := &captureStreamTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s := skillSession(t, realRegistry(t), r, tools)
	s.SetSkills([]string{skills.IDSpawnAgentForEdits})
	if _, err := s.Send(context.Background(), "implement the feature", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]struct{}, len(tools.last))
	for _, n := range tools.last {
		got[n] = struct{}{}
	}
	// Core tools always present.
	for _, core := range []string{"context.snapshot", "tool.search", "skill.step.advance", "skill.run.get", "skill.find", "skill.load"} {
		if _, ok := got[core]; !ok {
			t.Fatalf("core tool %q pruned", core)
		}
	}
	// The active skill's required tools present.
	for _, req := range []string{"agentTask.spawnForEdits", "watcher.terminal.create"} {
		if _, ok := got[req]; !ok {
			t.Fatalf("required tool %q missing", req)
		}
	}
	// Tools no active skill requires are pruned.
	for _, pruned := range []string{"timer.schedule", "skill.run"} {
		if _, ok := got[pruned]; ok {
			t.Fatalf("tool %q should be pruned", pruned)
		}
	}
}

func TestSendNeverEmptyToolListUnconstrained(t *testing.T) {
	full := []models.ChatTool{{Function: models.ChatToolFunc{Name: "fs__read"}}}
	tools := &captureStreamTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}, full: full}
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s := skillSession(t, realRegistry(t), r, tools)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// Guard: empty activeSkillIds returns nil (full registry), never an empty slice.
	if tools.last != nil {
		t.Fatalf("unconstrained turn must use the full registry, got filter %v", tools.last)
	}
}

// --- findSkills (skill.find engine) ---

func TestFindSkillsLoadsSelectionAndMergesFirst(t *testing.T) {
	reg := realRegistry(t)
	deps := SessionDeps{
		Router:        plainRouter(),
		Tools:         &fakeTools{},
		SkillSelector: selStub{sel: skills.SkillSelection{SkillIDs: []string{skills.IDRecipeRunner}, Confidence: 0.8, Reason: "x", TaskType: "skill"}},
		SkillCatalog:  reg,
		SessionID:     "ses_find",
		Events:        NoopEventSink{},
	}
	s := NewSession(deps)
	s.SetSkills([]string{skills.IDBasicOrchestration})
	res := s.FindSkills(context.Background(), "how do I run a workspace skill")
	if !res.Matched {
		t.Fatal("expected a match")
	}
	got := s.ActiveSkillIDs()
	sort.Strings(got)
	want := []string{skills.IDBasicOrchestration, skills.IDRecipeRunner}
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Fatalf("active = %v want %v (new merges in front of loaded)", got, want)
	}
}

func TestFindSkillsIgnoresHallucinatedIDs(t *testing.T) {
	reg := realRegistry(t)
	deps := SessionDeps{
		Router:        plainRouter(),
		Tools:         &fakeTools{},
		SkillSelector: selStub{sel: skills.SkillSelection{SkillIDs: []string{"hallucinated.recipe.id"}, Confidence: 0.4}},
		SkillCatalog:  reg,
		SessionID:     "ses_find2",
		Events:        NoopEventSink{},
	}
	s := NewSession(deps)
	s.SetSkills([]string{skills.IDSpawnAgentForEdits})
	res := s.FindSkills(context.Background(), "do a thing")
	if res.Matched {
		t.Fatal("an all-hallucinated selection must not match")
	}
	if got := s.ActiveSkillIDs(); len(got) != 1 || got[0] != skills.IDSpawnAgentForEdits {
		t.Fatalf("loaded set must be unchanged, got %v", got)
	}
}

func TestFindSkillsSelectorErrorKeepsSkills(t *testing.T) {
	reg := realRegistry(t)
	deps := SessionDeps{
		Router:        plainRouter(),
		Tools:         &fakeTools{},
		SkillSelector: selStub{err: errSelector},
		SkillCatalog:  reg,
		SessionID:     "ses_find3",
		Events:        NoopEventSink{},
	}
	s := NewSession(deps)
	s.SetSkills([]string{skills.IDBasicOrchestration})
	res := s.FindSkills(context.Background(), "anything")
	if res.Ok {
		t.Fatal("selector error should be ok:false")
	}
	if got := s.ActiveSkillIDs(); len(got) != 1 || got[0] != skills.IDBasicOrchestration {
		t.Fatalf("loaded set must survive a selector error, got %v", got)
	}
}

// --- read-only (wake) turn projection ---

// roTools is a runner whose ReadOnlyToolNames returns only the read tool, so the
// wake turn offers nothing else.
type roTools struct {
	*captureStreamTools
}

func (r *roTools) ReadOnlyToolNames() []string { return []string{"inspect.read"} }

func TestWakeTurnOffersOnlyReadOnlyNames(t *testing.T) {
	tools := &roTools{captureStreamTools: &captureStreamTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}}
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s := skillSession(t, realRegistry(t), r, tools)
	if _, err := s.Send(context.Background(), "[wake]", SendOptions{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if len(tools.last) != 1 || tools.last[0] != "inspect.read" {
		t.Fatalf("wake turn projection = %v want [inspect.read]", tools.last)
	}
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// cacheKeyRouter captures the promptCacheKey the loop passes to the large stream.
type cacheKeyRouter struct{ captured chan string }

func (r *cacheKeyRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	select {
	case r.captured <- opts.PromptCacheKey:
	default:
	}
	return models.ChatResult{Content: "ok"}, nil
}
func (r *cacheKeyRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *cacheKeyRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

var errSelector = errString("flash model down")

type errString string

func (e errString) Error() string { return string(e) }
