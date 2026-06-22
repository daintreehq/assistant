package skills

import (
	"context"
	"errors"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// stubRouter returns a canned SkillSelection (or error) for SelectSkills.
type stubRouter struct {
	sel SkillSelection
	err error
	// captured request for assertions
	lastReq  JSONRequest
	lastTier domain.ModelTier
}

func (s *stubRouter) JSON(_ context.Context, tier domain.ModelTier, req JSONRequest, out any) error {
	s.lastReq = req
	s.lastTier = tier
	if s.err != nil {
		return s.err
	}
	if p, ok := out.(*SkillSelection); ok {
		*p = s.sel
	}
	return nil
}

func testRegistry(t *testing.T) *SkillRegistry {
	t.Helper()
	mk := func(id, ver string) Skill {
		return Skill{ID: id, Title: id, Version: ver, Summary: "s", WhenToUse: "w",
			Tags: []string{}, MaxTurns: 8, Risk: RiskRead, RequiredTools: []string{}, Body: "b"}
	}
	reg, err := NewRegistry([]Skill{
		mk("a.one", "1.0.0"), mk("b.two", "2.0.0"), mk("c.three", "3.0.0"), mk("d.four", "4.0.0"),
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

func TestSelectorUsesSmallTierAndPinsTemp(t *testing.T) {
	reg := testRegistry(t)
	r := &stubRouter{sel: SkillSelection{SkillIDs: []string{"a.one"}, Confidence: 0.9}}
	_, err := SelectSkills(context.Background(), r, reg.MetadataForSelection(), "query")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.lastTier != domain.ModelSmall {
		t.Errorf("tier = %q, want small", r.lastTier)
	}
	if r.lastReq.Temperature != 0 || r.lastReq.MaxTokens != 500 {
		t.Errorf("temp/maxTokens = %v/%d", r.lastReq.Temperature, r.lastReq.MaxTokens)
	}
}

func TestSelectorCapsAndClamps(t *testing.T) {
	reg := testRegistry(t)
	// Model returns 4 ids and an out-of-range confidence.
	r := &stubRouter{sel: SkillSelection{
		SkillIDs:   []string{"a.one", "b.two", "c.three", "d.four"},
		Confidence: 1.7,
	}}
	sel, err := SelectSkills(context.Background(), r, reg.MetadataForSelection(), "q")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(sel.SkillIDs) != 3 {
		t.Errorf("expected cap to 3, got %d", len(sel.SkillIDs))
	}
	if sel.Confidence != 1 {
		t.Errorf("expected confidence clamped to 1, got %v", sel.Confidence)
	}
}

func TestResolveKnownIdsOrder(t *testing.T) {
	reg := testRegistry(t)
	a := NewActiveSkills(reg, &stubRouter{}, nil, "sess", nil)
	// dedup → drop unknown ("zzz") BEFORE cap → cap at 3.
	got := a.resolveKnownIds([]string{"a.one", "a.one", "zzz", "b.two", "c.three", "d.four"})
	want := []string{"a.one", "b.two", "c.three"}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("at %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestFindSkillsMergesNewFirstAndCaps(t *testing.T) {
	reg := testRegistry(t)
	a := NewActiveSkills(reg, &stubRouter{sel: SkillSelection{SkillIDs: []string{"a.one", "b.two", "c.three"}}}, nil, "sess", nil)
	// First find loads 3.
	a.FindSkills(context.Background(), "q1")
	if len(a.ActiveSkillIDs()) != 3 {
		t.Fatalf("expected 3 active, got %v", a.ActiveSkillIDs())
	}
	// A new find for d.four must merge d.four FIRST and cap to 3.
	a.router = &stubRouter{sel: SkillSelection{SkillIDs: []string{"d.four"}}}
	res := a.FindSkills(context.Background(), "q2")
	if !res.Ok || !res.Matched {
		t.Fatalf("expected matched find, got %+v", res)
	}
	ids := a.ActiveSkillIDs()
	if len(ids) != 3 {
		t.Fatalf("expected 3, got %v", ids)
	}
	found := false
	for _, id := range ids {
		if id == "d.four" {
			found = true
		}
	}
	if !found {
		t.Errorf("d.four should have survived the cap (merged first): %v", ids)
	}
	// res.selected reflects only what THIS query resolved.
	if len(res.Selected) != 1 || res.Selected[0].ID != "d.four" {
		t.Errorf("selected = %+v", res.Selected)
	}
}

func TestFindSkillsSelectorErrorLeavesSetUnchanged(t *testing.T) {
	reg := testRegistry(t)
	a := NewActiveSkills(reg, &stubRouter{sel: SkillSelection{SkillIDs: []string{"a.one"}}}, nil, "sess", nil)
	a.FindSkills(context.Background(), "q1")
	before := a.ActiveSkillIDs()

	a.router = &stubRouter{err: errors.New("boom")}
	res := a.FindSkills(context.Background(), "q2")
	if res.Ok {
		t.Errorf("expected ok=false on selector error")
	}
	if res.Reason != "skill selector unavailable" {
		t.Errorf("reason = %q", res.Reason)
	}
	if len(a.ActiveSkillIDs()) != len(before) {
		t.Errorf("loaded set changed on error: %v vs %v", a.ActiveSkillIDs(), before)
	}
}

func TestBuildToolFilter(t *testing.T) {
	mk := func(id string, tools ...string) Skill {
		return Skill{ID: id, Title: id, Version: "1.0.0", Summary: "s", WhenToUse: "w",
			Tags: []string{}, MaxTurns: 8, Risk: RiskRead, RequiredTools: tools, Body: "b"}
	}
	reg, _ := NewRegistry([]Skill{mk("a.one", "alpha", "beta"), mk("b.two", "beta", "gamma")})
	core := []string{"skill.find", "skill.load"}
	a := NewActiveSkills(reg, &stubRouter{}, nil, "sess", core)

	// No skills loaded ⇒ nil (unconstrained).
	if f := a.BuildToolFilter(); f != nil {
		t.Errorf("expected nil filter when unconstrained, got %v", f)
	}

	a.SetSkills([]string{"a.one", "b.two"})
	f := a.BuildToolFilter()
	// Must contain core + union of requiredTools, deduped.
	want := map[string]bool{"skill.find": true, "skill.load": true, "alpha": true, "beta": true, "gamma": true}
	if len(f) != len(want) {
		t.Fatalf("filter = %v (want %d unique)", f, len(want))
	}
	for _, name := range f {
		if !want[name] {
			t.Errorf("unexpected tool in filter: %q", name)
		}
	}
}

func TestValidateRequiredTools(t *testing.T) {
	sk := Skill{ID: "a.one", RequiredTools: []string{"present", "absent"}}
	tp := stubTools{"present": true}
	missing := ValidateRequiredTools([]Skill{sk}, tp)
	if len(missing) != 1 || missing[0].Tool != "absent" {
		t.Errorf("missing = %+v", missing)
	}
}

type stubTools map[string]bool

func (s stubTools) Has(name string) bool { return s[name] }
