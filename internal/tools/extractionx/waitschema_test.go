package extractionx

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// waitschema_test.go pins the leafDocs split in sharedBaseProps.
//
// terminal.extract and terminal.extract.json render the SAME wait union, and the
// inventory ships on every model round, so the .json copy documents its leaves
// tersely and leans on terminal.extract (which projects first) for the full prose.
// That is only safe while three things hold, none of which review reliably catches
// because every failure here is SILENT — the call still validates, the model just
// stops being told something:
//
//  1. the machine-readable half is intact, leaf by leaf (TestExtractWaitCarriesTheFullUnion);
//  2. the terse copy keeps the clauses that exist because a caller got them wrong
//     (TestExtractWaitKeepsItsHardWonWarnings);
//  3. the two copies are actually wired to different modes, and the terse one is
//     actually smaller (TestExtractWaitLeafDocsAreWiredToTheRightTools).
//
// Structure is asserted against a hardcoded expectation rather than by diffing the
// two renderings against each other. Diffing is what the watcher package's
// equivalent does, and against a single shared template it is a TAUTOLOGY: both
// copies render from sharedBaseProps, so a keyword deleted from the template
// disappears from both and the comparison still holds. (That weakness predates this
// file and is worth a follow-up in internal/tools/watcher.)

// waitOf decodes one tool's schema and returns its `wait` subschema.
func waitOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties object")
	}
	wait, ok := props["wait"].(map[string]any)
	if !ok {
		t.Fatal("schema has no wait property")
	}
	return wait
}

// waitCopies are the two renderings under test. Run as subtests so a malformed
// first schema reports and moves on instead of aborting before the second is seen.
var waitCopies = []struct {
	tool   string
	schema json.RawMessage
}{
	{"terminal.extract", extractSchema},
	{"terminal.extract.json", extractJSONSchema},
}

// Every leaf pinned by SHAPE, not just by presence: keys alone would pass while an
// enum value, a type or a bound quietly went missing from the shared template.
func TestExtractWaitCarriesTheFullUnion(t *testing.T) {
	wantLeaves := map[string]struct {
		typ    string
		enum   []string
		bounds map[string]float64
	}{
		"stateIs": {typ: "string", enum: []string{
			"idle", "working", "waiting", "directing", "completed", "exited",
		}},
		"runtimeStatusIs": {typ: "string", enum: []string{"running", "exited"}},
		"contains":        {typ: "string", bounds: map[string]float64{"minLength": 1}},
		"regex":           {typ: "string", bounds: map[string]float64{"minLength": 1}},
		"noOutputForMs":   {typ: "integer", bounds: map[string]float64{"minimum": 1}},
		"all":             {typ: "array", bounds: map[string]float64{"minItems": 1}},
		"any":             {typ: "array", bounds: map[string]float64{"minItems": 1}},
		"not": {typ: "object", bounds: map[string]float64{
			"minProperties": 1, "maxProperties": 1,
		}},
	}

	for _, tc := range waitCopies {
		t.Run(tc.tool, func(t *testing.T) {
			wait := waitOf(t, tc.schema)
			if wait["type"] != "object" {
				t.Errorf("wait has type %v, want object", wait["type"])
			}
			if wait["maxProperties"] != float64(1) {
				t.Errorf("wait must encode at-most-one-key as maxProperties 1, got %v", wait["maxProperties"])
			}
			// minProperties must be ABSENT, and this is the assertion the prose
			// alone would not have bought: {} is a VALID wait here — it is the
			// coerced settled default, the one the container description prefers
			// over stateIs:'waiting'. Adding minProperties:1 (which is what the
			// watcher union correctly carries) would make the documented default
			// unrepresentable while every other check here still passed.
			if _, present := wait["minProperties"]; present {
				t.Errorf("wait must NOT set minProperties: {} is the coerced settled default and has to stay representable")
			}
			if wait["additionalProperties"] != false {
				t.Error("wait must set additionalProperties:false")
			}

			leaves, ok := wait["properties"].(map[string]any)
			if !ok {
				t.Fatal("wait has no properties")
			}
			if len(leaves) != len(wantLeaves) {
				t.Errorf("wait has %d union keys, want %d", len(leaves), len(wantLeaves))
			}
			for key, want := range wantLeaves {
				leaf, ok := leaves[key].(map[string]any)
				if !ok {
					t.Errorf("wait is missing union key %q", key)
					continue
				}
				if leaf["type"] != want.typ {
					t.Errorf("wait.%s has type %v, want %q", key, leaf["type"], want.typ)
				}
				if want.enum != nil {
					raw, _ := leaf["enum"].([]any)
					got := make([]string, 0, len(raw))
					for _, v := range raw {
						s, _ := v.(string)
						got = append(got, s)
					}
					if !slices.Equal(got, want.enum) {
						t.Errorf("wait.%s enum is %v, want %v — a state dropped here is one the model can no longer wait for",
							key, got, want.enum)
					}
				}
				for kw, n := range want.bounds {
					if leaf[kw] != n {
						t.Errorf("wait.%s lost %s (got %v, want %v)", key, kw, leaf[kw], n)
					}
				}
			}
			// modelJudge must stay UNGENERABLE at the top level, not merely
			// undocumented: extraction rejects it (rejectModelJudge), so the schema
			// should stop the model producing it rather than fail mid-turn.
			if _, present := leaves["modelJudge"]; present {
				t.Error("wait enumerates modelJudge, which extraction rejects — it must not be generable")
			}
			// The combinators nest the same one-key shape. (Deliberately shallow:
			// the nested schemas bound cardinality only, so a nested modelJudge is
			// still machine-legal and is caught at runtime — which is exactly why
			// the "anywhere" scope word below is load-bearing prose.)
			for _, key := range []string{"all", "any"} {
				leaf, _ := leaves[key].(map[string]any)
				items, ok := leaf["items"].(map[string]any)
				if !ok {
					t.Errorf("wait.%s has no items schema", key)
					continue
				}
				if items["minProperties"] != float64(1) || items["maxProperties"] != float64(1) {
					t.Errorf("wait.%s items must encode exactly-one-key, got %v/%v",
						key, items["minProperties"], items["maxProperties"])
				}
			}
		})
	}
}

// Every leaf must still be DOCUMENTED in both copies. Terse is the point; absent is
// the failure — an undescribed leaf is one the model has to guess at.
func TestExtractWaitLeavesAreAllDocumented(t *testing.T) {
	for _, tc := range waitCopies {
		t.Run(tc.tool, func(t *testing.T) {
			leaves, ok := waitOf(t, tc.schema)["properties"].(map[string]any)
			if !ok {
				t.Fatal("wait has no properties")
			}
			for key, leaf := range leaves {
				desc, _ := leaf.(map[string]any)["description"].(string)
				if strings.TrimSpace(desc) == "" {
					t.Errorf("wait.%s has no description — terse is fine, absent is not", key)
				}
			}
		})
	}
}

// The clauses below are protected content: each exists because a caller got it
// wrong, so terseness may take restatement but never these. Asserted on BOTH
// copies — and terminal.extract.json is the one that matters, because it is the
// variant reached FOR multi-terminal work, which is where a combined tail
// straddling a boundary bites.
//
// Matched as independent substrings rather than one phrase because the verbose and
// terse renderings word the same rule differently ("modelJudge is not supported
// anywhere in the tree" vs "no modelJudge anywhere"). What is asserted is that both
// the SUBJECT and its SCOPE survive, which is the part that carries meaning.
func TestExtractWaitKeepsItsHardWonWarnings(t *testing.T) {
	for _, tc := range waitCopies {
		t.Run(tc.tool, func(t *testing.T) {
			wait := waitOf(t, tc.schema)
			container, _ := wait["description"].(string)
			// The container is never terse-able. It carries the stateIs:'waiting'
			// trap (the reason {} exists), the single-terminal scope of {}, and the
			// pointer to terminal.awaitAll for a cohort.
			for _, want := range []string{"stateIs:'waiting'", "No modelJudge", "single-terminal", "terminal.awaitAll"} {
				if !strings.Contains(container, want) {
					t.Errorf("wait container description lost %q", want)
				}
			}

			leaves, _ := wait["properties"].(map[string]any)
			for _, want := range []struct {
				leaf    string
				clauses []string
			}{
				// contains/regex match the COMBINED tail across terminals, so a
				// match can come from a terminal the caller was not asking about.
				{"contains", []string{"COMBINED tail"}},
				{"regex", []string{"COMBINED tail", "straddle a terminal boundary"}},
				// minLength cannot encode "is a valid RE2 pattern"; the runtime
				// rejects one that does not compile, so prose has to say it.
				{"regex", []string{"must compile"}},
				// Extraction rejects modelJudge at ANY DEPTH (condition.go walks the
				// tree), so the scope word is the load-bearing half. Without it the
				// advertised schema still accepts {"not":{"not":{"modelJudge":"…"}}}
				// — legal against this union, UNSUPPORTED_CONDITION at runtime.
				{"all", []string{"modelJudge", "anywhere"}},
				{"any", []string{"modelJudge", "anywhere"}},
				{"not", []string{"modelJudge", "anywhere"}},
				// `not` is a property literally named not, not the JSON-Schema keyword.
				{"not", []string{"NOT the JSON-Schema keyword"}},
			} {
				leaf, ok := leaves[want.leaf].(map[string]any)
				if !ok {
					t.Errorf("wait.%s is missing", want.leaf)
					continue
				}
				desc, _ := leaf["description"].(string)
				for _, clause := range want.clauses {
					if !strings.Contains(desc, clause) {
						t.Errorf("wait.%s dropped protected clause %q — it is there because a caller got it wrong;\n  got: %s",
							want.leaf, clause, desc)
					}
				}
			}
		})
	}
}

// The split only pays if the two tools are wired to DIFFERENT modes, and nothing
// above notices if they are not: both renderings satisfy every structural and
// protected-clause assertion, so flipping either call site to the other mode passes
// the whole file. This is the test that fails for that.
//
// The repo-wide ceiling in internal/app/toolbudget_test.go is not a backstop here
// either — its documented headroom is larger than this saving, so restoring the
// duplicated prose would sit comfortably inside it.
func TestExtractWaitLeafDocsAreWiredToTheRightTools(t *testing.T) {
	// A clause that exists ONLY in the verbose rendering: present in the tool that
	// carries the full prose, absent from the one that leans on it.
	const verboseOnly = "Do NOT use stateIs:'waiting' to mean finished"
	stateIsOf := func(raw json.RawMessage) string {
		leaves, _ := waitOf(t, raw)["properties"].(map[string]any)
		leaf, _ := leaves["stateIs"].(map[string]any)
		desc, _ := leaf["description"].(string)
		return desc
	}

	if !strings.Contains(stateIsOf(extractSchema), verboseOnly) {
		t.Errorf("terminal.extract must carry the FULL leaf prose — it projects first and is what the terse copy leans on")
	}
	if strings.Contains(stateIsOf(extractJSONSchema), verboseOnly) {
		t.Errorf("terminal.extract.json is rendering the verbose leaves — the duplication this split removed is back")
	}

	// And the terse rendering must actually be smaller. Guards the saving itself:
	// a future edit that grows the terse leaves back toward the verbose ones would
	// otherwise pass every assertion above while quietly undoing the change.
	full, terse := len(extractSchema), len(extractJSONSchema)
	// extract.json carries an extra required `jsonSchema` property (~600 B of
	// accepted-keyword contract), so it is compared against the saving rather than
	// against terminal.extract outright.
	const minSaving = 300
	verbose := len(sharedBaseProps(true))
	if saved := verbose - len(sharedBaseProps(false)); saved < minSaving {
		t.Errorf("the terse wait rendering now saves only %d bytes (want >= %d) — the leafDocs split has eroded;\n"+
			"extract=%d extract.json=%d", saved, minSaving, full, terse)
	}
}
