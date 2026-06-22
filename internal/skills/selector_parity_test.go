package skills

import (
	"context"
	"strings"
	"testing"
)

// userContent extracts the captured user message from a stubRouter request.
func userContent(req JSONRequest) string {
	for _, m := range req.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// Returns the small model's chosen skill ids for the query — the selection
// (ids + taskType) is surfaced verbatim.
func TestSelectorReturnsChosenIDs(t *testing.T) {
	reg := parityRegistry(t)
	r := &stubRouter{sel: SkillSelection{
		SkillIDs: []string{"b.two"}, Confidence: 0.9,
		Reason: "fits", TaskType: "code_edit",
	}}
	sel, err := SelectSkills(context.Background(), r, reg.MetadataForSelection(), "implement a fix")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(sel.SkillIDs) != 1 || sel.SkillIDs[0] != "b.two" {
		t.Errorf("skillIds = %v", sel.SkillIDs)
	}
	if sel.TaskType != "code_edit" {
		t.Errorf("taskType = %q", sel.TaskType)
	}
}

// Mirrors "sends the query and skill headers to the small model — never bodies":
// the query text and candidate ids land in the user message, but no body marker.
func TestSelectorSendsHeadersNeverBodies(t *testing.T) {
	reg := parityRegistry(t)
	r := &stubRouter{sel: SkillSelection{SkillIDs: []string{}, Confidence: 0.1}}
	if _, err := SelectSkills(context.Background(), r, reg.MetadataForSelection(), "what is going on?"); err != nil {
		t.Fatalf("err: %v", err)
	}
	user := userContent(r.lastReq)
	if !strings.Contains(user, "what is going on?") {
		t.Errorf("query missing from user message: %q", user)
	}
	if !strings.Contains(user, "a.one") {
		t.Errorf("candidate id missing from user message")
	}
	if strings.Contains(user, bodyMarker) {
		t.Errorf("skill body leaked into selector input: %q", user)
	}
}

// Mirrors "bounds an over-long query before sending it to the model": a 5000-char
// query is clipped to the 2000-char cap, so the full string never lands but the
// 2000-char prefix does.
func TestSelectorBoundsOverLongQuery(t *testing.T) {
	reg := parityRegistry(t)
	r := &stubRouter{sel: SkillSelection{SkillIDs: []string{}, Confidence: 0.1}}
	huge := strings.Repeat("x", 5000)
	if _, err := SelectSkills(context.Background(), r, reg.MetadataForSelection(), huge); err != nil {
		t.Fatalf("err: %v", err)
	}
	user := userContent(r.lastReq)
	if strings.Contains(user, huge) {
		t.Errorf("full over-long query was not clipped")
	}
	if !strings.Contains(user, strings.Repeat("x", maxQueryChars)) {
		t.Errorf("clipped query prefix (%d chars) missing", maxQueryChars)
	}
}
