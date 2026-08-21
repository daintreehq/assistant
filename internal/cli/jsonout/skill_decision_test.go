package jsonout

import (
	"bytes"
	"math"
	"sort"
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
		Selector:    agent.SkillSelectorOutcome{}, // zero value: every field must still appear
	})

	raw := buf.String()
	if !bytes.Contains([]byte(raw), []byte(`"active":[]`)) {
		t.Fatalf(`active must serialize as [], got: %s`, raw)
	}
	if !bytes.Contains([]byte(raw), []byte(`"newlyLoaded":[]`)) {
		t.Fatalf(`newlyLoaded must serialize as [], got: %s`, raw)
	}

	line := decodeOne(t, bytes.NewBufferString(raw))

	// The FULL key set, so an added omitempty (or a new stray field) fails here rather
	// than silently changing the contract a consumer parses.
	wantTop := map[string]bool{"type": true, "ts": true, "seq": true,
		"active": true, "newlyLoaded": true, "selector": true}
	assertExactKeys(t, "line", line, wantTop)

	sel, ok := line["selector"].(map[string]any)
	if !ok {
		t.Fatalf("selector = %#v, want an object", line["selector"])
	}
	assertExactKeys(t, "selector", sel, map[string]bool{
		"ran": true, "degraded": true, "taskType": true, "confidence": true, "reason": true})

	// Present-and-null, not absent: an absent key reads as "this CLI version does not
	// report confidence", which is a different fact.
	if c := sel["confidence"]; c != nil {
		t.Fatalf("selector.confidence = %#v, want null", c)
	}
	// Same for the string fields: "" rather than absent. The selector here is the ZERO
	// value, so an omitempty on either would drop the key entirely.
	if sel["taskType"] != "" {
		t.Fatalf("selector.taskType = %#v, want \"\"", sel["taskType"])
	}
	if sel["reason"] != "" {
		t.Fatalf("selector.reason = %#v, want \"\"", sel["reason"])
	}
}

// assertExactKeys pins a JSON object's key set exactly — neither a missing key nor an
// unexpected extra one may pass.
func assertExactKeys(t *testing.T, what string, got map[string]any, want map[string]bool) {
	t.Helper()
	for k := range want {
		if _, present := got[k]; !present {
			t.Errorf("%s is missing key %q (keys: %v)", what, k, keysOf(got))
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("%s carries unexpected key %q", what, k)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// Every field of a fully-populated event reaches the wire under its documented key.
// Deliberately NOT a round trip through the same tagged struct: that would pass even if
// every tag were renamed in a coordinated way, since the same tags would decode it back.
func TestSkillDecisionEmitsEveryFieldByDocumentedKey(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.SkillDecision(agent.SkillDecisionEvent{
		Active:      []agent.SkillRef{{ID: "a", Title: "Alpha"}},
		NewlyLoaded: []agent.SkillRef{{ID: "n", Title: "Newly"}},
		Selector: agent.SkillSelectorOutcome{
			Ran: true, Degraded: true, TaskType: "review",
			Confidence: confidence(0.5), Reason: "why",
		},
	})

	line := decodeOne(t, &buf)
	active, _ := line["active"].([]any)
	newly, _ := line["newlyLoaded"].([]any)
	if len(active) != 1 || len(newly) != 1 {
		t.Fatalf("active/newlyLoaded = %#v / %#v", line["active"], line["newlyLoaded"])
	}
	a, _ := active[0].(map[string]any)
	n, _ := newly[0].(map[string]any)
	if a["id"] != "a" || a["title"] != "Alpha" {
		t.Fatalf("active[0] = %#v", a)
	}
	// Distinct fixtures per slice, so a projection that emitted the same list twice fails.
	if n["id"] != "n" || n["title"] != "Newly" {
		t.Fatalf("newlyLoaded[0] = %#v", n)
	}
	sel, _ := line["selector"].(map[string]any)
	if sel["ran"] != true || sel["degraded"] != true || sel["taskType"] != "review" ||
		sel["reason"] != "why" {
		t.Fatalf("selector = %#v", sel)
	}
	if c, _ := sel["confidence"].(float64); c != 0.5 {
		t.Fatalf("selector.confidence = %#v", sel["confidence"])
	}
}

// emitStruct marshals before framing, so a value JSON cannot represent takes a different
// failure path than emit's. NaN is reachable: confidence comes off the wire as a float.
// The line must stay valid JSON and keep its seq, so a consumer's parser does not choke
// and the monotonic-seq contract survives.
func TestSkillDecisionUnserializableDegradesWithoutBreakingTheStream(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.SkillDecision(agent.SkillDecisionEvent{
		Selector: agent.SkillSelectorOutcome{Confidence: confidence(math.NaN())},
	})
	s.Info("still streaming")

	lines := decodeLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("expected the degraded line then info, got %d: %v", len(lines), lines)
	}
	if lines[0]["type"] != "skill:decision" {
		t.Fatalf("type = %v, want the event type preserved", lines[0]["type"])
	}
	if lines[0]["serializationError"] != true {
		t.Fatalf("line = %v, want serializationError:true", lines[0])
	}
	// No seq gap: a hole would break the monotonic-seq contract the stream promises.
	if lines[0]["seq"].(float64) != 0 || lines[1]["seq"].(float64) != 1 {
		t.Fatalf("seq = %v, %v; want 0, 1", lines[0]["seq"], lines[1]["seq"])
	}
}
