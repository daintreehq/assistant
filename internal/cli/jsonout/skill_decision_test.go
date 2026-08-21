package jsonout

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
)

// skill_decision_test.go pins the WIRE SHAPE of the skill:decision line, which is the
// whole point of the event: it exists so a scripted run can assert which runbook was
// active and whether the selector actually chose it. A consumer's assertion breaks on a
// renamed key just as hard as on a missing event, so the keys are pinned literally here
// rather than round-tripped through the same struct that produces them.

// decodeOne returns the single JSONL line the sink wrote, as a generic map.
func decodeOne(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 line, got %d: %v", len(lines), lines)
	}
	return lines[0]
}

func confidence(f float64) *float64 { return &f }

// A round that loaded something: ids AND titles, the whole active set (not just the
// delta), and the selector's verdict — the three things the titles-only skill:loaded
// line could not answer.
func TestSkillDecisionLineShape(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)

	s.SkillDecision(agent.SkillDecisionEvent{
		Active: []agent.SkillRef{
			{ID: "multi_agent", Title: "Multi-agent orchestration"},
			{ID: "daintree_foundation", Title: "Daintree orchestration foundation"},
		},
		NewlyLoaded: []agent.SkillRef{{ID: "multi_agent", Title: "Multi-agent orchestration"}},
		Selector: agent.SkillSelectorOutcome{
			Ran:        true,
			Degraded:   false,
			TaskType:   "orchestration",
			Confidence: confidence(0.96),
			Reason:     "coordinating multiple agents",
		},
	})

	line := decodeOne(t, &buf)
	if line["type"] != "skill:decision" {
		t.Fatalf("type = %v, want skill:decision", line["type"])
	}

	active, ok := line["active"].([]any)
	if !ok || len(active) != 2 {
		t.Fatalf("active = %#v, want 2 entries", line["active"])
	}
	first, ok := active[0].(map[string]any)
	if !ok {
		t.Fatalf("active[0] = %#v, want an object", active[0])
	}
	// The id is the reason this event exists at all — a title is a display string that
	// can drift, an id is the identity a test asserts on.
	if first["id"] != "multi_agent" || first["title"] != "Multi-agent orchestration" {
		t.Fatalf("active[0] = %#v, want {id:multi_agent, title:Multi-agent orchestration}", first)
	}

	// The delta is still reported, alongside the active set rather than instead of it.
	newly, ok := line["newlyLoaded"].([]any)
	if !ok || len(newly) != 1 {
		t.Fatalf("newlyLoaded = %#v, want 1 entry", line["newlyLoaded"])
	}

	sel, ok := line["selector"].(map[string]any)
	if !ok {
		t.Fatalf("selector = %#v, want an object", line["selector"])
	}
	if sel["ran"] != true || sel["degraded"] != false {
		t.Fatalf("selector ran/degraded = %v/%v, want true/false", sel["ran"], sel["degraded"])
	}
	if sel["taskType"] != "orchestration" {
		t.Fatalf("selector.taskType = %v", sel["taskType"])
	}
	if c, ok := sel["confidence"].(float64); !ok || c != 0.96 {
		t.Fatalf("selector.confidence = %#v, want 0.96", sel["confidence"])
	}
	if sel["reason"] != "coordinating multiple agents" {
		t.Fatalf("selector.reason = %v", sel["reason"])
	}
}

// The stream is camelCase throughout; the backend's own wire tags are snake_case. Emitting
// the backend struct verbatim would have leaked task_type/newly_loaded into a stream where
// every other line reads auditId/exitCode, so the snake_case spellings are pinned ABSENT.
func TestSkillDecisionUsesCamelCaseNotBackendWireKeys(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.SkillDecision(agent.SkillDecisionEvent{
		Active:      []agent.SkillRef{},
		NewlyLoaded: []agent.SkillRef{},
		Selector:    agent.SkillSelectorOutcome{Ran: true, TaskType: "supervise"},
	})

	line := decodeOne(t, &buf)
	for _, banned := range []string{"newly_loaded", "task_type"} {
		if _, present := line[banned]; present {
			t.Fatalf("line carries backend wire key %q; the --json stream is camelCase", banned)
		}
	}
	sel, _ := line["selector"].(map[string]any)
	if _, present := sel["task_type"]; present {
		t.Fatal("selector carries backend wire key task_type")
	}
	// Selector token usage is deliberately not on this seam — it would be read against
	// the terminal `result` stats, which do not include it.
	if _, present := sel["usage"]; present {
		t.Fatal("selector carries usage; it is intentionally excluded from the stream")
	}
	if _, present := line["prelude"]; present {
		t.Fatal("line carries prelude; it is vestigial backend metadata")
	}
}

// An empty set must marshal as [] and not null: a consumer distinguishing "no skills
// active" from "this field failed to serialize" should not have to guess. Confidence is
// the deliberate exception — it is a pointer so "the selector reported none" is null
// rather than a misleading 0.0.
func TestSkillDecisionEmptySetsAreArraysAndConfidenceIsNull(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.SkillDecision(agent.SkillDecisionEvent{
		Active:      []agent.SkillRef{},
		NewlyLoaded: []agent.SkillRef{},
		Selector:    agent.SkillSelectorOutcome{Ran: false, Reason: "selector unavailable"},
	})

	raw := buf.String()
	if !bytes.Contains([]byte(raw), []byte(`"active":[]`)) {
		t.Fatalf(`active must serialize as [], got: %s`, raw)
	}
	if !bytes.Contains([]byte(raw), []byte(`"newlyLoaded":[]`)) {
		t.Fatalf(`newlyLoaded must serialize as [], got: %s`, raw)
	}

	line := decodeOne(t, bytes.NewBufferString(raw))
	sel, _ := line["selector"].(map[string]any)
	// Present-and-null, not absent: an absent key reads as "this CLI version does not
	// report confidence", which is a different fact.
	c, present := sel["confidence"]
	if !present {
		t.Fatal("selector.confidence must be present as null, not omitted")
	}
	if c != nil {
		t.Fatalf("selector.confidence = %#v, want null", c)
	}
	// Same for the string fields: "" rather than absent.
	if _, present := sel["taskType"]; !present {
		t.Fatal("selector.taskType must be present as \"\", not omitted")
	}
}

// The degraded case is the one this event exists for. A selector that fails open reuses
// the PRIOR active set, so `active` looks completely healthy — only this flag says the
// round never actually decided on it.
func TestSkillDecisionReportsDegradedSelector(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.SkillDecision(agent.SkillDecisionEvent{
		Active:      []agent.SkillRef{{ID: "multi_agent", Title: "Multi-agent orchestration"}},
		NewlyLoaded: []agent.SkillRef{}, // nothing new: the prior set was reused wholesale
		Selector: agent.SkillSelectorOutcome{
			Ran:      true,
			Degraded: true,
			Reason:   "selector timed out; reused the prior active set",
		},
	})

	line := decodeOne(t, &buf)
	sel, _ := line["selector"].(map[string]any)
	if sel["degraded"] != true {
		t.Fatalf("selector.degraded = %v, want true", sel["degraded"])
	}
	if active, _ := line["active"].([]any); len(active) != 1 {
		t.Fatalf("a degraded round still reports the set it fell open into, got %#v", line["active"])
	}
}

// skill:loaded keeps its exact prior shape. That is what makes this an ADDITIVE change
// needing no JSONOutputSchemaVersion bump, and it is why an existing consumer keeps
// working — so it is pinned rather than assumed.
func TestSkillLoadedShapeUnchangedAlongsideDecision(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.SkillLoaded([]string{"Multi-agent orchestration"})

	line := decodeOne(t, &buf)
	if line["type"] != "skill:loaded" {
		t.Fatalf("type = %v", line["type"])
	}
	titles, ok := line["titles"].([]any)
	if !ok || len(titles) != 1 || titles[0] != "Multi-agent orchestration" {
		t.Fatalf("titles = %#v, want the unchanged titles array", line["titles"])
	}
	// Strictly titles + framing: no ids leaked onto the eager cue, which would invite a
	// consumer to treat a per-attempt delta as authoritative.
	for _, k := range []string{"active", "newlyLoaded", "selector", "skills"} {
		if _, present := line[k]; present {
			t.Fatalf("skill:loaded gained key %q; it must stay titles-only", k)
		}
	}
}

// Ordering: a decision arriving mid-stream flushes buffered prose first, so the line lands
// at the round boundary it describes instead of inside the previous round's text.
func TestSkillDecisionFlushesBufferedProseFirst(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.AssistantToken("thinking about it")
	s.SkillDecision(agent.SkillDecisionEvent{
		Active:      []agent.SkillRef{},
		NewlyLoaded: []agent.SkillRef{},
	})

	lines := decodeLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("expected content then decision, got %d lines: %v", len(lines), lines)
	}
	if lines[0]["type"] != "assistant:content" || lines[1]["type"] != "skill:decision" {
		t.Fatalf("order = %v, %v; want assistant:content then skill:decision",
			lines[0]["type"], lines[1]["type"])
	}
	// seq stays monotonic across the inserted line.
	if lines[0]["seq"].(float64) != 0 || lines[1]["seq"].(float64) != 1 {
		t.Fatalf("seq = %v, %v; want 0, 1", lines[0]["seq"], lines[1]["seq"])
	}
}

// The line must survive a strict round trip back into the event type, so the documented
// shape and the type that produces it cannot drift apart silently.
func TestSkillDecisionRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	want := agent.SkillDecisionEvent{
		Active:      []agent.SkillRef{{ID: "a", Title: "Alpha"}},
		NewlyLoaded: []agent.SkillRef{{ID: "a", Title: "Alpha"}},
		Selector: agent.SkillSelectorOutcome{
			Ran: true, TaskType: "review", Confidence: confidence(0.5), Reason: "why",
		},
	}
	s.SkillDecision(want)

	var got agent.SkillDecisionEvent
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("line does not decode back into SkillDecisionEvent: %v", err)
	}
	if len(got.Active) != 1 || got.Active[0] != want.Active[0] {
		t.Fatalf("active round trip = %#v", got.Active)
	}
	if got.Selector.TaskType != "review" || got.Selector.Confidence == nil || *got.Selector.Confidence != 0.5 {
		t.Fatalf("selector round trip = %#v", got.Selector)
	}
}
