package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/host"
	"github.com/daintreehq/assistant/internal/safety"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/tools"
)

// capabilityref.go renders the GENERATED capability reference: one Markdown document
// describing every registered tool, and one compatibility manifest pinning the version
// numbers the CLI negotiates on.
//
// It exists because the same product surface was being maintained by hand in three
// places at once — README.md's tool table, docs/TOOLS.md's inventory, and the registry
// itself — and they had already drifted apart (67 tools listed in one, 83 in another,
// ~85 actually registered, with tools in the docs that no longer exist). A tester
// reading either document would be told about capabilities the binary does not have and
// left ignorant of ones it does.
//
// The input is the REAL registry from a fully-wired App, not a second list: whatever
// DefaultToolBuilder produces is what gets documented. The paired drift test regenerates
// and diffs, so adding a tool without regenerating fails CI rather than silently
// shipping a stale inventory.

// ToolDoc is one tool's row in the generated reference — the metadata the registry
// already carries, projected into the facts a reader needs.
type ToolDoc struct {
	Name        string
	Risk        domain.RiskClass
	Description string
	// Confirms is true when the tool needs interactive confirmation at the tier that
	// can reach it. Derived from the safety matrix, never restated.
	Confirms bool
	// TypedConfirm is true when a single keypress is not enough (git/system).
	TypedConfirm bool
	// MinTier is the lowest tier that may call this tool at all.
	MinTier domain.Tier
	// Requires names an external connection the tool needs to do its job.
	Requires tools.Connection
	// Grant describes what an UNATTENDED actor (watcher, timer, wake) can do with this
	// tool. Three states, not a boolean: for a tool that needs no confirmation in the
	// first place, "grantable: yes" is technically true and practically misleading —
	// no grant is consulted, the actor simply runs it.
	Grant string
	// Parallel describes how the tool may be dispatched concurrently.
	Parallel string
	// FeatureFlag names the env var gating registration, when one does.
	FeatureFlag string
}

// workflowGraphTools are the execution-graph tools that register only under
// DAINTREE_WORKFLOW_INTELLIGENCE. Named here so the generated reference can label
// them instead of appearing to promise them unconditionally; the drift test builds the
// registry with the flag ON and OFF and asserts this set is exactly the difference, so
// the list cannot rot.
var workflowGraphTools = map[string]bool{
	"workflow.plan":           true,
	"workflow.getGraph":       true,
	"workflow.next":           true,
	"workflow.attachResource": true,
	"workflow.recordEvidence": true,
	"workflow.reconcile":      true,
	"workflow.cancel":         true,
}

// CollectToolDocs projects a live registry into the generated reference's rows, in
// registration order (the same order the model is shown them).
func CollectToolDocs(reg *tools.Registry) []ToolDoc {
	list := reg.List()
	out := make([]ToolDoc, 0, len(list))
	for _, t := range list {
		d := ToolDoc{
			Name:         t.Name,
			Risk:         t.Risk,
			Description:  firstSentence(t.Description),
			TypedConfirm: safety.NeedsTypedConfirm(t.Risk),
			MinTier:      minTierFor(t.Risk),
			Requires:     t.Requires,
			Parallel:     parallelClass(t),
		}
		// Evaluate confirmation at the lowest tier that can actually reach the tool;
		// asking at a tier that denies it outright would report "no confirmation" for
		// the most dangerous tools in the registry.
		d.Confirms = safety.Decide(t.Risk, d.MinTier).NeedsConfirmation
		d.Grant = grantState(t.Name, d.Confirms)
		if workflowGraphTools[t.Name] {
			d.FeatureFlag = "DAINTREE_WORKFLOW_INTELLIGENCE"
		}
		out = append(out, d)
	}
	return out
}

// grantState describes what an unattended actor can do with a tool.
//
// The distinction that matters: a grant is only ever CONSULTED for a tool that would
// otherwise need confirmation. For a read/local/UI tool an unattended actor just runs it,
// so labelling those "grantable" invites the reader to conclude a grant is required —
// the opposite of the truth, and exactly the kind of half-right security claim that
// makes a reader trust the rest of the table less.
func grantState(name string, confirms bool) string {
	switch {
	case domain.IsUngrantableTool(name):
		return "never"
	case !confirms:
		return "not needed"
	default:
		return "grantable"
	}
}

// minTierFor returns the lowest tier permitted to call a risk class.
//
// Derived by asking the policy rather than restating its table, so a change to the tier
// matrix shows up in the generated docs on the next regeneration. An unrecognised risk
// class PANICS rather than defaulting: dispatch would deny such a tool at every tier,
// and quietly documenting it as system-callable would publish a capability that does not
// exist. This runs only from the generator and its test, so the blast radius is a failed
// regeneration — which is the correct outcome.
func minTierFor(risk domain.RiskClass) domain.Tier {
	for _, tier := range []domain.Tier{domain.TierSupervisor, domain.TierOperator, domain.TierSystem} {
		if safety.TierAllowsRisk(tier, risk) {
			return tier
		}
	}
	panic(fmt.Sprintf("capabilityref: risk class %q is reachable from no tier — dispatch would deny it everywhere; fix the tool's Risk or the tier matrix", risk))
}

// parallelClass names how a tool may be dispatched alongside its batch siblings.
func parallelClass(t *tools.Tool) string {
	switch {
	case t.Parallelizable:
		return "read-cohort"
	case t.ParallelHomogeneous:
		return "same-tool cohort"
	default:
		return "serial"
	}
}

// firstSentence trims a long model-facing Description down to one line for a table
// cell. Tool descriptions are deliberately instructional and can run to paragraphs;
// the generated reference is an inventory, not a substitute for reading the schema.
func firstSentence(desc string) string {
	s := strings.TrimSpace(desc)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	// Break on ". " (or a trailing "."), NEVER on a bare period: tool names are dotted,
	// and splitting on the first '.' truncated "List asynchronous operations
	// (terminal.run.async …)" to "List asynchronous operations (terminal" — a description
	// that reads as a typo and tells the reader nothing.
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i]
	} else if strings.HasSuffix(s, ".") {
		s = strings.TrimSuffix(s, ".")
	}
	s = strings.Join(strings.Fields(s), " ")
	// Truncate by RUNES, not bytes, and BEFORE escaping. Byte truncation split the
	// multi-byte "→" in workflow.create's description and wrote a lone continuation
	// byte, leaving the whole generated file invalid UTF-8 — enough to break awk,
	// iconv, and anything else that validates before parsing.
	if r := []rune(s); len(r) > 120 {
		s = strings.TrimSpace(string(r[:117])) + "…"
	}
	// A pipe would break the Markdown table it lands in. Escaped last so the escape
	// itself can never be cut in half.
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}

// generatedHeader is the banner every generated file carries. Regeneration is a test,
// not a `go generate` binary, so the instruction names the test.
const generatedHeader = `<!-- Code generated by internal/app/capabilityref.go. DO NOT EDIT. -->
<!-- Regenerate with: go test ./internal/app -run TestGeneratedDocsAreCurrent -update -->
`

// RenderToolReference renders the full generated tool reference.
func RenderToolReference(docs []ToolDoc) string {
	var b strings.Builder
	b.WriteString(generatedHeader)
	b.WriteString("\n# Tool reference (generated)\n\n")
	b.WriteString("Every tool the assistant can call, projected straight from the live registry\n")
	b.WriteString("(`internal/tools` as wired by `app.DefaultToolBuilder`). This file is the inventory;\n")
	b.WriteString("[`../TOOLS.md`](../TOOLS.md) is the contributor guide for *adding* one, and the\n")
	b.WriteString("argument schemas live in the tool definitions themselves.\n\n")
	b.WriteString("**Do not edit this file.** It is regenerated from the registry and diffed in CI, so a\n")
	b.WriteString("hand edit is reverted on the next run and a new tool that is not regenerated fails the\n")
	b.WriteString("build. That is the point: this inventory replaced three hand-maintained lists that had\n")
	b.WriteString("already drifted out of agreement with each other and with the binary.\n\n")

	b.WriteString("## Column meanings\n\n")
	b.WriteString("| Column | Meaning |\n|---|---|\n")
	b.WriteString("| **Risk** | The risk class driving tier gating and confirmation (`internal/safety`). |\n")
	b.WriteString("| **Min tier** | Lowest permission tier that may call it at all. Below this it is denied, not prompted. |\n")
	b.WriteString("| **Confirm** | What the interactive `main` actor is asked, **with `AUTO_APPROVE` off** (the default). `typed` = a typed phrase is required (git/system); `yes` = single-key approval; `—` = runs without asking. `AUTO_APPROVE=1` suppresses the prompt for every row — it does **not** widen the tier gate, and it never applies to unattended actors. |\n")
	b.WriteString("| **Grant** | What an **unattended** actor (watcher, timer, wake) can do. `not needed` = no confirmation is required in the first place, so no grant is consulted and the actor simply runs it. `grantable` = it needs a scoped automation grant, or it is blocked. `never` = no grant can ever authorise it, whatever its scope. |\n")
	b.WriteString("| **Needs** | The connection whose absence makes the tool unable to do its job. `—` means it works in degraded mode — either purely local, or it degrades gracefully and reports the outage as its answer. A tool needing two connections lists the one whose absence leaves nothing to do (no terminal to read ⇒ nothing to summarize). |\n")
	b.WriteString("| **Parallel** | `read-cohort` = may batch with a consecutive run of other read-cohort calls; `same-tool cohort` = batches only with consecutive, already-authorised calls of the same tool; `serial` = one at a time. Opt-in per tool — `serial` reads are not batched. |\n")
	b.WriteString("| **Flag** | Env var gating registration, when one does. |\n\n")
	b.WriteString("> Two rows carry a nuance a table cannot: `grant.create` is `local` risk and so shows\n")
	b.WriteString("> no confirmation, but it raises a **typed system confirmation** when the grant it is\n")
	b.WriteString("> minting would cover mutating work. And `user.askMultipleChoice` needs a human at a\n")
	b.WriteString("> TTY: every unattended actor has no question surface and gets `QUESTION_NOT_INTERACTIVE`.\n\n")

	// Group by the dotted-name prefix — the family boundary a reader already thinks in.
	groups := map[string][]ToolDoc{}
	var order []string
	for _, d := range docs {
		g := d.Name
		if i := strings.Index(g, "."); i > 0 {
			g = g[:i]
		}
		if _, seen := groups[g]; !seen {
			order = append(order, g)
		}
		groups[g] = append(groups[g], d)
	}
	sort.Strings(order)

	fmt.Fprintf(&b, "## Inventory — %d tools in %d groups\n", len(docs), len(order))
	for _, g := range order {
		fmt.Fprintf(&b, "\n### `%s.*`\n\n", g)
		b.WriteString("| Tool | Risk | Min tier | Confirm | Grant | Needs | Parallel | Flag | Description |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, d := range groups[g] {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s | %s | %s |\n",
				d.Name, d.Risk, d.MinTier,
				confirmCell(d), grantCell(d), dash(string(d.Requires)),
				d.Parallel, dash(d.FeatureFlag), d.Description)
		}
	}

	b.WriteString("\n## Invariants this inventory is subject to\n\n")
	b.WriteString("- **No tool edits project files.** `Registry.AssertSafe` rejects, at startup, any tool\n")
	b.WriteString("  whose name contains a file-mutating fragment. Code changes are delegated to a\n")
	b.WriteString("  *visible* agent terminal — `agentTask.spawnForEdits` directly, or\n")
	b.WriteString("  `workflow.startWorkOnIssue`, which creates the worktree and spawns into it.\n")
	b.WriteString("- **`daintree.call` is the raw MCP escape hatch.** System risk, never grantable, and\n")
	b.WriteString("  deliberately exceptional — prefer a typed wrapper whenever one exists.\n")
	b.WriteString("- **A grant is the only unattended path _for a tool that needs confirmation_.** Reads\n")
	b.WriteString("  and local writes (`not needed` above) run unattended without one; a watcher, timer,\n")
	b.WriteString("  or wake turn reaching a `grantable` tool without a matching grant produces a blocked\n")
	b.WriteString("  attention item instead of an action.\n")
	b.WriteString("- **`Needs` is documentation, never a gate.** Dispatch does not read it. A tool whose\n")
	b.WriteString("  connection is down still runs and returns its own clean \"not connected\" failure —\n")
	b.WriteString("  gating on it would block the diagnostics a disconnected user reaches for first.\n")
	return b.String()
}

func confirmCell(d ToolDoc) string {
	switch {
	case d.Confirms && d.TypedConfirm:
		return "**typed**"
	case d.Confirms:
		return "yes"
	default:
		return "—"
	}
}

func grantCell(d ToolDoc) string {
	if d.Grant == "never" {
		return "**never**"
	}
	return d.Grant
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return "`" + s + "`"
}

// RenderCompatibilityManifest renders every version number the CLI negotiates on.
//
// These are the values that make a release a COMBINATION rather than a single build: a
// backend outside the protocol range, a task id the server dropped, a host speaking a
// different NDJSON version, or a state DB from another schema each produce a different
// failure, and all four were previously only discoverable by reading source. Pinning
// them in one generated file gives support a single artefact to compare against, and
// gives the drift test something to fail on when one changes silently.
func RenderCompatibilityManifest() string {
	var b strings.Builder
	b.WriteString(generatedHeader)
	b.WriteString("\n# Compatibility manifest (generated)\n\n")
	b.WriteString("The versions this CLI build negotiates on. A release is the COMBINATION of Daintree,\n")
	b.WriteString("this CLI, and the backend — not any one of them alone.\n\n")

	b.WriteString("## Protocol and schema versions\n\n")
	b.WriteString("| Surface | Version | Failure mode when it mismatches |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Backend wire protocol | `%d` | backend answers HTTP 426; the turn cannot run |\n", backend.ProtocolVersion)
	fmt.Fprintf(&b, "| Embedded host (`host --stdio`) NDJSON | `%d` | Daintree and the CLI disagree on the request envelope |\n", host.ProtocolVersion)
	fmt.Fprintf(&b, "| SQLite state schema (`schemaUserVersion`) | `%d` | an OLDER non-zero on-disk schema is refused; an interactive launch then moves it aside to a timestamped backup and recreates it (a non-TTY launch fails loudly instead). A NEWER schema — an older CLI against a newer DB — is ALSO refused, with no reset offered: the file is not behind, this binary is |\n", storage.SchemaVersion())

	b.WriteString("\n## Backend tasks this CLI will call\n\n")
	b.WriteString("Every id here is one the CLI actually sends. A backend that does not advertise one\n")
	b.WriteString("guarantees a mid-turn 404 — which is why `doctor` treats a missing id as a gating\n")
	b.WriteString("failure rather than a warning (the 2026-07-07 de-versioning incident, which a\n")
	b.WriteString("count-only check could not see).\n\n")
	writeTaskList(&b, "Always required", backend.CoreTaskIDs())
	writeTaskList(&b, "Required unless `DAINTREE_WORKFLOW_INTELLIGENCE=0`", backend.WorkflowTaskIDs())

	b.WriteString("\n## Endpoints\n\n")
	b.WriteString("| Endpoint | URL |\n|---|---|\n")
	fmt.Fprintf(&b, "| Deployed (default) | `%s` |\n", backend.DefaultBaseURL)
	fmt.Fprintf(&b, "| Local development | `%s` |\n", backend.LocalBaseURL)
	b.WriteString("\nSelected with `DAINTREE_BACKEND_URL`; there is no sign-in and no key to supply.\n")
	b.WriteString("A **remote** endpoint that does not serve `/v1/daintree/auth/verify` is a `doctor`\n")
	b.WriteString("failure; only a loopback endpoint is forgiven. See `docs/BACKEND.md`.\n")

	b.WriteString("\n## Platform support\n\n")
	b.WriteString("| Platform | Cockpit / one-shot | Persistent supervision |\n|---|---|---|\n")
	b.WriteString("| macOS (arm64, amd64) | supported | supported |\n")
	b.WriteString("| Linux (amd64, arm64) | supported | supported |\n")
	b.WriteString("| Windows | **unsupported** | **unsupported** — `flock` + `Setsid` have no port |\n")
	b.WriteString("\nWindows is not built or tested in CI, so \"it compiles\" is not a claim this project\nmakes. The supervisor explicitly returns an unsupported error there rather than run\nwithout mutual exclusion.\n")
	return b.String()
}

func writeTaskList(b *strings.Builder, title string, ids []string) {
	fmt.Fprintf(b, "**%s** (%d)\n\n", title, len(ids))
	if len(ids) == 0 {
		b.WriteString("_none_\n\n")
		return
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for _, id := range sorted {
		fmt.Fprintf(b, "- `%s`\n", id)
	}
	b.WriteString("\n")
}
