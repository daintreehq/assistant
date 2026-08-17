package extractionx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// merge_result_test.go pins the multi-terminal MERGE signal (issue #345).
//
// Over several terminalIds an extraction concatenates every tail into ONE pass, so the
// answer can cover a single terminal while the result echoes all N ids back next to
// matched:true / truncated:false. Nothing in the old result distinguished that from
// full-cohort coverage, and a real session burned a round rediscovering it. These tests
// pin that the merge is now reported — and, just as importantly, that it is NOT reported
// where it would be noise (a single id, or a gate-only call with no answer to misattribute).

// A multi-terminal TEXT extraction flags the merge in the result map and leads the
// summary with the same warning, while leaving the honest pre-existing fields alone.
func TestExtractTool_MultiTerminalTextMarksMerged(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("waiting", "", "fact from t1"),
		"t2": ent("waiting", "", "fact from t2"),
	}}}
	router := &routeRouter{textRes: "answer covering t1 only"}
	tool := newExtractTool(Deps{Reader: reader, Router: router})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1","t2"],"instruction":"each agent's fact"}`), nil)
	if !res.Ok {
		t.Fatalf("extract should succeed, got %+v", res.Error)
	}

	m := res.Result.(map[string]any)
	if merged, _ := m["merged"].(bool); !merged {
		t.Fatalf("merged must be true for a multi-id extraction, got %v", m["merged"])
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "MERGED") || !strings.Contains(note, "2 tails") {
		t.Fatalf("note must name the merge and the tail count, got %q", note)
	}
	if !strings.Contains(note, "terminal.extract.json") {
		t.Fatalf("the text remedy must point at the per-terminal alternative, got %q", note)
	}
	// The warning must LEAD the summary: an oversized result keeps only the first
	// TruncationSummaryChars of it, so a trailing warning would be sliced away.
	if !strings.HasPrefix(res.Summary, note) {
		t.Fatalf("summary must lead with the merge note, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "answer covering t1 only") {
		t.Fatalf("summary must still carry the extracted text, got %q", res.Summary)
	}
	// The merge flag ADDS information; it must not restate the other fields as failures.
	ids, _ := m["terminalIds"].([]string)
	if len(ids) != 2 {
		t.Fatalf("terminalIds stays full input provenance, got %v", m["terminalIds"])
	}
	if matched, _ := m["matched"].(bool); !matched {
		t.Fatalf("matched reports the wait verdict and must stay true, got %v", m["matched"])
	}
}

// One terminal cannot be a merge, so neither field appears — same omit-when-inapplicable
// convention as watchersRetired, and the summary keeps its plain shape.
func TestExtractTool_SingleTerminalTextOmitsMerged(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("waiting", "", "the answer"),
	}}}
	tool := newExtractTool(Deps{Reader: reader, Router: &routeRouter{textRes: "the answer"}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1"],"instruction":"the answer"}`), nil)
	if !res.Ok {
		t.Fatalf("extract should succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if _, present := m["merged"]; present {
		t.Fatal("merged must be absent for a single terminal")
	}
	if _, present := m["note"]; present {
		t.Fatal("note must be absent for a single terminal")
	}
	if res.Summary != "the answer" {
		t.Fatalf("single-id summary must be unchanged, got %q", res.Summary)
	}
}

// A gate-only cohort call returns booleans, never an extracted answer, so there is
// nothing to misattribute — and `contains` is documented to match the COMBINED tail.
// Flagging a merge here would report an ambiguity that does not exist.
func TestExtractTool_GateOnlyCohortOmitsMerged(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("working", "", "still going"),
		"t2": ent("waiting", "", "found the needle"),
	}}}
	router := &routeRouter{}
	tool := newExtractTool(Deps{Reader: reader, Router: router})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1","t2"],"wait":{"contains":"needle"},"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if !res.Ok {
		t.Fatalf("gate should succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if matched, _ := m["matched"].(bool); !matched {
		t.Fatalf("the combined tail contains the needle, matched should be true: %+v", m)
	}
	if _, present := m["merged"]; present {
		t.Fatal("a gate-only call has no extracted answer, so merged must be absent")
	}
	if _, present := m["note"]; present {
		t.Fatal("a gate-only call must not carry the merge note either")
	}
	if strings.Contains(res.Summary, "MERGED") {
		t.Fatalf("a gate-only summary must stay unwarned, got %q", res.Summary)
	}
	if router.textCalled {
		t.Fatal("a gate-only call must not invoke the extraction model")
	}
}

// The json tool merges its input the same way, so it carries the same flag — even when
// the schema DID attribute per terminal. merged describes the input pass; the remedy
// tells the model to verify coverage rather than assume it.
func TestExtractJSONTool_MultiTerminalMarksMerged(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("waiting", "", "fact from t1"),
		"t2": ent("waiting", "", "fact from t2"),
	}}}
	router := &routeRouter{jsonRes: []any{
		map[string]any{"terminalId": "t1", "fact": "one"},
		map[string]any{"terminalId": "t2", "fact": "two"},
	}}
	tool := newExtractJSONTool(Deps{Reader: reader, Router: router})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1","t2"],"instruction":"each fact","jsonSchema":"{\"type\":\"array\"}"}`), nil)
	if !res.Ok {
		t.Fatalf("extract.json should succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if merged, _ := m["merged"].(bool); !merged {
		t.Fatalf("merged must be true for a multi-id json extraction, got %v", m["merged"])
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "entry for every terminalId") {
		t.Fatalf("the json remedy must ask for the per-id coverage check, got %q", note)
	}
	// This fixture DID attribute every terminal, so the warning has to stay conditional.
	// Hardening it to an absolute claim ("covers only one terminal") would make the note
	// a false statement in exactly this case, and nothing else would catch that.
	if !strings.Contains(note, "may cover only one terminal") {
		t.Fatalf("the note must hedge — the answer MAY be partial, not IS partial: %q", note)
	}
	if !strings.HasPrefix(res.Summary, note) {
		t.Fatalf("summary must lead with the merge note, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "Extracted JSON result.") {
		t.Fatalf("summary must keep its base text, got %q", res.Summary)
	}
}

// Single-id json keeps the untouched summary and neither merge field.
func TestExtractJSONTool_SingleTerminalOmitsMerged(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("waiting", "", "fact from t1"),
	}}}
	tool := newExtractJSONTool(Deps{Reader: reader, Router: &routeRouter{jsonRes: map[string]any{"fact": "one"}}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1"],"instruction":"the fact","jsonSchema":"{\"type\":\"object\"}"}`), nil)
	if !res.Ok {
		t.Fatalf("extract.json should succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if _, present := m["merged"]; present {
		t.Fatal("merged must be absent for a single terminal")
	}
	if _, present := m["note"]; present {
		t.Fatal("note must be absent for a single terminal")
	}
	if res.Summary != "Extracted JSON result." {
		t.Fatalf("single-id json summary must be unchanged, got %q", res.Summary)
	}
}

// When BOTH warnings apply, merge scope must come FIRST: "this may be about the wrong
// terminal" outranks "there is more of the right output". Order is a deliberate contract,
// and nothing else asserts it — the length test below is order-insensitive, so without
// this a swap to note+mergeNote+text would pass the whole file.
func TestExtractTool_MergeNotePrecedesTruncationNote(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("waiting", "", "fact from t1"),
		"t2": ent("waiting", "", "fact from t2"),
	}}}
	router := &routeRouter{textRes: "partial answer", textTruncated: true}
	tool := newExtractTool(Deps{Reader: reader, Router: router})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1","t2"],"instruction":"each fact","maxTokens":2000}`), nil)
	if !res.Ok {
		t.Fatalf("extract should succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if truncated, _ := m["truncated"].(bool); !truncated {
		t.Fatalf("fixture must exercise the truncated path, got %+v", m)
	}
	mergeNote, _ := m["note"].(string)
	want := mergeNote + "\n\n" + textTruncationNote(2000) + "partial answer"
	if res.Summary != want {
		t.Fatalf("merge note must lead the truncation note.\n got: %q\nwant: %q", res.Summary, want)
	}
}

// Both warnings a text extraction can carry have to survive TOGETHER. An oversized result
// keeps only the first TruncationSummaryChars of the summary, so a longer note would
// silently amputate the other's remedy rather than fail anywhere visible. Measured against
// the PRODUCTION truncation string (not a copy, which would go stale unnoticed) at the
// schema's maxItems and the maximum maxTokens — the widest either can render.
func TestMergeNote_LeavesRoomForTheTruncationNote(t *testing.T) {
	ids := make([]string, 16) // terminalIds maxItems
	for i := range ids {
		ids[i] = fmt.Sprintf("t%d", i) // distinct: duplicates are now rejected
	}
	truncationNote := textTruncationNote(2000) // maxTokens maximum

	for _, remedy := range []string{mergeRemedyText, mergeRemedyJSON} {
		prefix := noteMergedExtraction(map[string]any{}, ids, remedy)
		if n := len([]rune(prefix + truncationNote)); n > domain.TruncationSummaryChars {
			t.Fatalf("merge note + truncation note = %d runes, over the %d-rune summary slice; shorten one",
				n, domain.TruncationSummaryChars)
		}
	}
}

// A repeated id would feed the SAME tail in twice and inflate the merged-tail count the
// result now reports. Id resolution dedupes, but it fails open on an unreadable roster, so
// the bound has to be enforced at decode — and declared uniqueItems so it is ungenerable.
func TestExtractTool_RejectsDuplicateTerminalIDs(t *testing.T) {
	tool := newExtractTool(Deps{})
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":["t1","t1"],"instruction":"x"}`)); err == nil {
		t.Fatal("a duplicated terminalId must be rejected at decode")
	}
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":["t1","t2"],"instruction":"x"}`)); err != nil {
		t.Fatalf("distinct ids must still decode: %v", err)
	}
	if !strings.Contains(string(extractSchema), `"uniqueItems": true`) {
		t.Fatal("terminalIds must declare uniqueItems so the model cannot generate a duplicate")
	}
}

// The merge caveat has to LEAD terminal.extract's description, not sit in the middle of
// it. That placement is the fix for the half of issue #345 no result field can reach —
// the model chooses the tool from the description, before any result exists — and the
// generated capability reference publishes only this first sentence.
func TestExtractTool_DescriptionLeadsWithMerge(t *testing.T) {
	desc := newExtractTool(Deps{}).Description
	lead := desc
	if i := strings.Index(lead, ". "); i > 0 {
		lead = lead[:i]
	}
	if !strings.Contains(strings.ToUpper(lead), "MERGE") || !strings.Contains(lead, "MULTIPLE") {
		t.Fatalf("the first sentence must lead with the multi-id merge, got %q", lead)
	}
	// The lead must also say what the merge is NOT. "MERGES tails into one answer" still
	// reads like a convenience to a model shopping for a fan-out; the explicit denial is
	// the part that stops the wrong call, so it cannot be edited away for brevity.
	if !strings.Contains(lead, "never one per terminal") {
		t.Fatalf("the lead must deny the per-terminal reading outright, got %q", lead)
	}
	// Whatever the old opener guaranteed for one-or-more terminals — a BOUNDED tail, plain
	// TEXT, the small model — must stay true of the MULTI-id case too. Restating them only
	// under "on a SINGLE terminalId" would quietly narrow the contract (toolbudget_test.go:
	// a budget must never push a load-bearing rule out of a description).
	for _, guarantee := range []string{"bounded", "small model", "plain-TEXT"} {
		if !strings.Contains(lead, guarantee) {
			t.Fatalf("the lead dropped the %q guarantee for multi-id calls: %q", guarantee, lead)
		}
	}
	// firstSentence() in the capability-reference generator ellipsizes past 120 runes.
	if n := len([]rune(lead)); n > 120 {
		t.Fatalf("lead sentence is %d runes; the generated docs cell truncates past 120", n)
	}
}
