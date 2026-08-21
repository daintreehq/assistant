package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// toolbudget_test.go bounds the tool projection's size.
//
// The inventory is sent on EVERY model round, and is paid for on every one. Measured on
// the backend: the tool schemas were ~61% of a representative request — more than double
// the permanent system prompt. Nothing was enforcing that, so it grew the way an
// unbounded thing grows: one reasonable paragraph at a time.
//
// These are budgets, not laws of nature. An orchestrator tool genuinely needs more room
// than a file read. What the budgets buy is that exceeding one has to be a decision
// somebody makes, with a comment, rather than a paragraph nobody weighed.
//
// The one thing they must NOT do is push a load-bearing rule out of the description and
// into nowhere. A rule about ONE tool belongs in that tool's description, where the model
// sees it beside the thing it governs; a rule spanning several tools belongs in a
// backend-owned skill. Deleting it because of a byte count is the failure mode, and no
// test can catch that — only review can.

const (
	// maxOrdinaryDescription is the budget for a tool a reader can understand in one
	// sentence and a shape.
	maxOrdinaryDescription = 600
	// maxOrchestratorDescription is the budget for the few tools that coordinate other
	// work and carry a real contract: enforced budgets, retirement semantics, what to do
	// when a wait is interrupted. Those rules are the reason the tool is usable.
	maxOrchestratorDescription = 1200
	// maxParameterDescription bounds one argument's description. Past this, an argument
	// is being used to document a workflow.
	maxParameterDescription = 300
)

// orchestratorTools are allowed the larger description budget. Deliberately a short,
// explicit list rather than a heuristic: "is this an orchestrator?" is a judgement about
// product surface, and making it a named exception is what keeps the budget from being
// quietly reinterpreted.
var orchestratorTools = map[string]bool{
	// A cohort finish-wait with an enforced cumulative budget, watcher-retirement
	// semantics, and an interruption contract. Every one of those is a rule the model
	// gets wrong without being told.
	"terminal.awaitAll": true,
	// The async future pair: each has to explain that it returns a HANDLE rather than a
	// result, and that the completion arrives through the attention queue rather than as
	// a late tool result.
	"terminal.run.async":   true,
	"terminal.await.async": true,
	// Extraction: what it reads, what it costs, and why a leaf call must not recurse.
	"terminal.extract": true,
}

// TestToolDescriptionsAreWithinBudget fails when a description outgrows its budget.
func TestToolDescriptionsAreWithinBudget(t *testing.T) {
	a := newFlaggedApp(t)
	defer a.Shutdown()

	type over struct {
		name  string
		chars int
		limit int
	}
	var violations []over
	for _, tool := range a.Registry.List() {
		limit := maxOrdinaryDescription
		if orchestratorTools[tool.Name] {
			limit = maxOrchestratorDescription
		}
		if n := utf8.RuneCountInString(tool.Description); n > limit {
			violations = append(violations, over{tool.Name, n, limit})
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].chars > violations[j].chars })
	for _, v := range violations {
		t.Errorf("%s: description is %d chars, over its %d budget — compact it, or move a cross-tool rule into a backend skill. "+
			"If it genuinely needs the room, add it to orchestratorTools with a reason.",
			v.name, v.chars, v.limit)
	}

	// Every named orchestrator must still exist AND still need the room. Checking only
	// existence lets an exemption outlive its justification: a description that shrinks
	// to 500 chars keeps a 1,200 allowance and can later grow back to 1,199 without
	// anyone deciding that was fine.
	sizes := map[string]int{}
	for _, tool := range a.Registry.List() {
		sizes[tool.Name] = utf8.RuneCountInString(tool.Description)
	}
	for name := range orchestratorTools {
		n, registered := sizes[name]
		switch {
		case !registered:
			t.Errorf("orchestratorTools names %q, which is not registered — remove the stale exemption", name)
		case n <= maxOrdinaryDescription:
			t.Errorf("orchestratorTools still exempts %q (%d chars, now within the ordinary %d budget) — remove the exemption",
				name, n, maxOrdinaryDescription)
		}
	}
}

// parameterBudgetExceptions are the argument descriptions allowed past the budget,
// keyed "tool path". Each entry is a claim that the text IS the shape and cannot be
// shortened without losing the contract — the same standard orchestratorTools is held to.
var parameterBudgetExceptions = map[string]string{
	// The list of accepted JSON Schema keywords. It cannot be summarised: the backend
	// REJECTS any other key outright, and "some keywords are not allowed" is exactly the
	// guidance that produced the failure this list exists to prevent — a schema written
	// with `description`/`$ref` in it, rejected mid-turn with nothing to correct against.
	// An enumeration is the only form of this rule that works.
	"terminal.extract.json .jsonSchema": "the accepted-keyword list IS the contract; an unlisted key is rejected outright",
}

// A parameter description that runs long is usually a workflow rule wearing an argument's
// clothes. The model reads it while filling in ONE field, which is the wrong moment for
// policy.
func TestParameterDescriptionsAreWithinBudget(t *testing.T) {
	a := newFlaggedApp(t)
	defer a.Shutdown()

	for _, tool := range a.Registry.List() {
		if len(tool.Schema) == 0 {
			continue
		}
		for _, d := range schemaDescriptions(t, tool.Schema) {
			if _, excepted := parameterBudgetExceptions[tool.Name+" "+d.Path]; excepted {
				continue
			}
			if n := utf8.RuneCountInString(d.Text); n > maxParameterDescription {
				t.Errorf("%s %s: parameter description is %d chars, over the %d budget — state the SHAPE here and put the workflow in the tool description or a skill",
					tool.Name, d.Path, n, maxParameterDescription)
			}
		}
	}
}

// An exception that no longer applies is worse than none: it silently exempts whatever
// argument later takes that name. Every entry must still name a real over-budget
// description.
func TestParameterBudgetExceptionsAreAllStillNeeded(t *testing.T) {
	a := newFlaggedApp(t)
	defer a.Shutdown()

	// Longest wins per path: an exemption is justified by the description that actually
	// exceeds the budget, and a path can name more than one (see schemaDescriptions).
	live := map[string]int{}
	for _, tool := range a.Registry.List() {
		if len(tool.Schema) == 0 {
			continue
		}
		for _, d := range schemaDescriptions(t, tool.Schema) {
			key := tool.Name + " " + d.Path
			if n := utf8.RuneCountInString(d.Text); n > live[key] {
				live[key] = n
			}
		}
	}
	for key, why := range parameterBudgetExceptions {
		n, present := live[key]
		switch {
		case !present:
			t.Errorf("parameterBudgetExceptions names %q, which no longer exists — remove it", key)
		case n <= maxParameterDescription:
			t.Errorf("parameterBudgetExceptions still exempts %q (%d chars, now within the %d budget) — remove it. Reason given: %s",
				key, n, maxParameterDescription, why)
		}
	}
}

// TestToolProjectionTotalIsBounded is the aggregate backstop. The per-tool budgets bound
// each description, but 80 tools each landing just under theirs is still a payload nobody
// chose. A ceiling on the whole thing makes growth visible as growth.
func TestToolProjectionTotalIsBounded(t *testing.T) {
	// BOTH projections. Checking only the default would leave the seven
	// workflow-intelligence tools — ~2.4 KB of description alone — entirely outside the
	// budget, so a flagged session could sail past the ceiling while CI stayed green.
	for _, tc := range []struct {
		name string
		opts ToolInventoryOptions
		max  int
	}{
		// Measured at ~77.8 KB after the compaction pass (down from ~85 KB), plus ~2 KB
		// of headroom. The headroom is deliberate: a budget that fails on the next honest
		// one-line addition is a budget somebody deletes. Raising either number is
		// allowed — raising it WITHOUT noticing is not, which is the whole point.
		//
		// Raised to 83.5 KB: two tools landed in the same window and together ate the
		// remaining headroom — subagent.run (~1.8 KB) and terminal.moveToWorktree
		// (~1.3 KB), measuring ~81.3 KB. The raise is forced, not chosen: subagent.run
		// merged WITHOUT raising the ceiling, leaving origin/develop at 79,902 of
		// 80,000 — 98 bytes from the wall — so this PR pays that bill as well as its
		// own. Both tools are deliberate: the first keeps an expensive search out of
		// the main conversation, the second replaces a hand-rolled passthrough. This
		// is the noticing the budget exists to force.
		//
		// Raised to 91.5 KB for issue #367: nine tools in one PR, measuring 89,383.
		// This is the largest single raise the budget has taken, so it owes the
		// clearest justification — and the justification is that these tools make the
		// EXPENSIVE surface smaller even as they make this one bigger. Every one of
		// them was previously reachable only through daintree.call: system risk,
		// always confirmed, ungrantable, and carrying nothing but an opaque
		// `arguments` bag. Reaching them cost a typed human approval AND a schema the
		// model had to guess at, which is how a "just check the CI comments" step
		// turned into several rounds. Six now carry read risk and run with no
		// approval at all.
		//
		// Measured contribution: 8,262 bytes for the nine, after a compaction pass
		// that took 957 bytes off the first draft. The largest is project.runCheck at
		// 1,443 — under half of agentTask.spawnForEdits and well under a third of
		// watcher.terminal.create, so none of them is an outlier worth singling out.
		// tool.schema (803) pays for itself immediately: it is the tool that stops the
		// model guessing the argument keys of everything else.
		//
		// UNCHANGED at 91.5 KB for issue #368's daintree.invoke, which measures 90,452
		// — the two tools it adds on top of tool.schema cost 1,069 bytes and were paid
		// for out of the existing headroom rather than by moving the number. The
		// savings came from prose duplicated across surfaces: "search → schema →
		// invoke" was restated in three descriptions, the never-auto-corrected rule sat
		// in both tool.schema's description and its `name` property, and
		// validate-before-invoke in both daintree.invoke's description and its
		// `arguments` property. A rule the model reads twice costs twice and teaches
		// once.
		//
		// That leaves ~1.0 KB of headroom rather than the ~2 KB the previous two raises
		// each left. Recorded here rather than topped back up: the budget's job is to
		// make the next addition argue for itself, and halving the slack without
		// raising the ceiling is that job being done. The next tool to land here should
		// expect to raise the number, deliberately, with its own paragraph.
		{"default", ToolInventoryOptions{}, 91_500},
		// The flag adds seven execution-graph tools. It is off by default and off in
		// production, so it gets its own ceiling rather than eating the default's
		// headroom — but it is still bounded, because a rollout flag is not an excuse.
		// Raised in lockstep with the default for the same two additions (~88.1 KB),
		// and to the same ~2 KB of headroom rather than more.
		// Raised in lockstep for the same nine additions (measuring 96,110), to the
		// same ~2 KB of headroom rather than more.
		// Also UNCHANGED for daintree.invoke (measuring 97,179): the flagged set carries
		// everything the default carries, so it absorbed the identical 1,069 bytes and
		// is left with ~0.8 KB. Same reasoning as above — the shrinking headroom is the
		// signal, and hiding it behind a raise nobody argued for is what the budget
		// exists to prevent.
		{"workflow-intelligence", ToolInventoryOptions{WorkflowIntelligence: true}, 98_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := BuildToolInventory(tc.opts)
			if err != nil {
				t.Fatalf("BuildToolInventory: %v", err)
			}
			wire, err := json.Marshal(inv)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if len(wire) > tc.max {
				var desc, params int
				for _, tool := range inv {
					desc += len(tool.Function.Description)
					params += len(tool.Function.Parameters)
				}
				t.Errorf("the tool projection is %d bytes, over the %d budget (descriptions %d, schemas %d).\n"+
					"It is sent on EVERY model round and the user pays for it on their own key. "+
					"Compact the largest offenders (go run ./cmd/tooldump, then sort by size) or raise the budget deliberately.",
					len(wire), tc.max, desc, params)
			}
			t.Logf("%s projection: %d bytes of %d", tc.name, len(wire), tc.max)
		})
	}
}

// schemaDescription is one `description` string found in a schema, with the path that
// located it.
type schemaDescription struct {
	Path string
	Text string
}

// schemaDescriptions walks a JSON Schema and returns EVERY `description` string it finds.
// Recursive because the long ones hide inside nested objects, array items and combinators
// — exactly where a check that only looked at top-level properties would miss them.
//
// A SLICE, not a map keyed by path. Paths are deliberately readable rather than unique
// (an array's `items` flattens onto its parent), so a map would let an item's short
// description silently overwrite its container's over-budget one — the check would then
// pass on the very schema it exists to catch.
func schemaDescriptions(t *testing.T, raw json.RawMessage) []schemaDescription {
	t.Helper()
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Errorf("schema is not valid JSON: %v", err)
		return nil
	}
	var out []schemaDescription
	var walk func(n any, path string)
	walk = func(n any, path string) {
		switch v := n.(type) {
		case map[string]any:
			if d, ok := v["description"].(string); ok {
				out = append(out, schemaDescription{Path: path, Text: d})
			}
			for key, child := range v {
				if key == "description" {
					continue
				}
				next := path + "." + key
				if key == "properties" || key == "items" || key == "$defs" || key == "definitions" {
					next = path
				}
				walk(child, next)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(node, strings.TrimPrefix("", "."))
	return out
}
