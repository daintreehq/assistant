package app

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/models"
	"github.com/daintreehq/assistant/internal/subagent"
)

// subagent.go wires the delegation layer: which tools a sub-agent may call, how
// its dispatches are actored, where its transcript lands, and how its rounds are
// traced. The runner itself (internal/subagent) knows none of this — it is handed
// a Dispatcher and a sink and stays free of the App's dependency graph.

// subagentToolDenylist names READ-RISK tools a sub-agent must not be offered even
// though the risk filter would admit them. Each exclusion is a reason, not a
// preference — a read-only tool is admitted by default, and anything removed here
// has to justify itself:
//
//   - subagent.run: recursion. A sub-agent spawning sub-agents turns a bounded
//     round budget into an unbounded tree, and nothing needs it — the orchestrator
//     can fan out several peers in one batch, which is easier to reason about and
//     to cost.
//   - runbook.run.get: runbook-run bookkeeping for the orchestrator's own procedures.
//     Runbook selection does not even run for the sub-agent profile (the backend
//     skips it), so there is no run for a sub-agent to inspect.
//   - queue.digest: the attention inbox is the human's, read at the boundary
//     between turns. A sub-agent reading it would pull unrelated pending work into
//     a search and quietly widen the brief.
//
// It holds only tools the filter would OTHERWISE ADMIT, and a test enforces that
// (TestSubagentDenylistEntriesAreLiveAndOtherwiseAdmissible). Listing an already
// excluded tool here would read as a decision someone made when it is really a
// no-op — user.askMultipleChoice (ui) and runbook.step.advance (local) both looked
// like they belonged and neither did.
var subagentToolDenylist = map[string]struct{}{
	"subagent.run":  {},
	"runbook.run.get": {},
	"queue.digest":  {},
}

// subagentToolNames is the sub-agent's inventory: every registered read-risk tool
// minus the denylist, sorted for a stable projection.
//
// Risk class is the gate, and that is the whole safety argument. domain.RiskRead
// is the registry's own claim that a tool changes nothing, enforced at
// registration and used by the tier policy for every other caller in the system —
// so the sub-agent inherits an invariant the codebase already maintains, rather
// than depending on a hand-kept allowlist that would silently go stale the next
// time a tool family is added.
func (a *App) subagentToolNames() []string {
	if a.Registry == nil {
		return nil
	}
	var names []string
	for _, t := range a.Registry.List() {
		if t == nil || t.Risk != domain.RiskRead {
			continue
		}
		if _, denied := subagentToolDenylist[t.Name]; denied {
			continue
		}
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}

// subagentDispatcher is the subagent.Dispatcher over the App's registry. It is a
// deliberately SEPARATE adapter from toolRunner rather than a mode on it: the two
// differ in actor, in inventory, and in whether an approval surface exists at all,
// and a shared adapter with an "is this a sub-agent" branch is exactly how one of
// those three eventually gets forgotten.
type subagentDispatcher struct{ app *App }

// Tools projects the read-only inventory. Recomputed per run rather than cached:
// the registry is fixed after Create, but the cost is a map walk over ~130 tools
// against a run that is about to make several model calls, and a cache here would
// have to be invalidated by any future dynamic registration.
func (d subagentDispatcher) Tools() ([]models.ChatTool, error) {
	specs, err := d.app.Registry.OpenAITools(d.app.subagentToolNames())
	if err != nil {
		return nil, err
	}
	out := make([]models.ChatTool, 0, len(specs))
	for _, sp := range specs {
		out = append(out, models.ChatTool{
			Type: sp.Type,
			Function: models.ChatToolFunc{
				Name:        sp.Function.Name,
				Description: sp.Function.Description,
				Parameters:  sp.Function.Parameters,
			},
		})
	}
	return out, nil
}

func (d subagentDispatcher) ResolveWireName(wireName string) string {
	return d.app.Registry.ResolveWireName(wireName)
}

// Dispatch runs one sub-agent tool call, under THREE layers applied in the order
// dispatch actually applies them. Worth stating precisely, because which layer
// does the work is not the obvious one:
//
//  1. ActiveToolNames is the sub-agent's own inventory, so the registry's
//     not-offered check (Dispatch step 1b) rejects anything outside it. This is
//     the PRIMARY gate and the only one covering EVERY risk class: local- and
//     ui-risk tools are not in safety.AlwaysConfirm, so they would pass the
//     confirmation branch below untouched and simply run. Never leave this unset.
//  2. The tier gate, unchanged from every other caller.
//  3. Actor is ActorSubagent — non-main, so an always-confirm class takes
//     dispatch's grant-or-blocked branch. ActorID is deliberately EMPTY, and
//     tryGrant returns nil on an empty ActorID before it ever queries: a
//     sub-agent therefore can never consume an automation grant minted for a
//     watcher, timer or wake, and such a call is blocked rather than authorized.
//     (publishDenial's `grantable` check likewise excludes this actor, so the
//     blocked inbox item carries no bogus "authorize it" action.)
//
// Confirm and AskChoice are nil'd explicitly. buildContext already returns
// (false, nil) from Confirm and leaves AskChoice nil for every non-main actor, so
// this is belt to that braces — it keeps the guarantee local to the adapter that
// depends on it rather than resting on a conditional two packages away.
//
// MCP priority is Background, not Interactive: a sub-agent's reads are real work
// but they are not the reads the human is waiting on, and a fan-out of three
// sub-agents must never crowd the main turn's calls out of the governor.
func (d subagentDispatcher) Dispatch(ctx context.Context, name, argsJSON string) domain.ToolResult {
	ctx = mcp.WithPriority(ctx, mcp.PriorityBackground)
	tctx := d.app.buildContext(domain.ActorSubagent, "")
	tctx.Confirm = nil
	tctx.AskChoice = nil
	tctx.ActiveToolNames = d.app.subagentToolNames()
	return d.app.Registry.Dispatch(ctx, name, json.RawMessage(argsJSON), tctx)
}

// subagentTranscriptSink files a finished run's transcript in the session's
// artifact store, which is what makes `artifact.read <transcriptId>` work with no
// second retrieval path. Resolved LAZILY for the same reason artifactStoreAdapter
// is: the tool builder runs before a.Session exists.
//
// A nil session (a sub-agent run outside a live session — today only in tests)
// yields an empty id, which the runner reports as "no transcript" rather than
// failing the run. Losing the receipt is a real cost, but it is strictly smaller
// than losing the finding.
type subagentTranscriptSink struct{ app *App }

func (s subagentTranscriptSink) Put(content string) string {
	if s.app.Session == nil {
		return ""
	}
	store := s.app.Session.Artifacts()
	if store == nil {
		return ""
	}
	return store.Put(content)
}

// newSubagentRunner builds the App's sub-agent runner. One runner serves every
// concurrent run (it holds no per-run state), which is what lets subagent.run be
// Parallelizable.
func (a *App) newSubagentRunner() *subagent.Runner {
	return subagent.New(subagent.Deps{
		// The Swappable, so a client replaced mid-run reaches the NEXT round rather
		// than stranding the run on a dead endpoint.
		Backend:    a.Backend,
		Tools:      subagentDispatcher{app: a},
		Transcript: subagentTranscriptSink{app: a},
		// The same stable project snapshot the main thread sends. A sub-agent asked
		// to find a file needs to know which project it is standing in, and this is
		// the block that says so.
		Startup: func() backend.StartupContext { return agent.BuildStartupContext(a.PromptContext()) },
		Trace: func(event string, fields map[string]any) {
			cfg := a.snapshotConfig()
			debuglog.LogDebug(debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}, event, fields)
		},
	})
}

// Compile-time proof the adapters still satisfy their seams — the seams live in
// another package, so a signature drift would otherwise only surface at wiring.
var (
	_ subagent.Dispatcher     = subagentDispatcher{}
	_ subagent.TranscriptSink = subagentTranscriptSink{}
)
