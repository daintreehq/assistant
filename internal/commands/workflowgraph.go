package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/workflowgraph"
)

// /workflow — the workflow-intelligence graph surfaces. All sub-commands are
// read-or-local-state only: nothing here touches terminals or external work.
//
//	/workflow                → list open graphs (same rows /workflows prepends)
//	/workflow <wfg_id>       → one graph's detail view
//	/workflow resume [msg]   → backend-ranked "where were we?" package
//	/workflow reconcile <id> → manual reconcile (reason "manual")
//	/workflow cancel <id>    → cancel the graph (LOCAL state only)

const workflowFeatureOffText = "Workflow intelligence is off. Set DAINTREE_WORKFLOW_INTELLIGENCE=1 (and run a backend with the workflow tasks) to enable execution graphs."

// workflowGraphCommand dispatches the /workflow sub-commands.
func workflowGraphCommand(ctx context.Context, a *app.App, rest []string) (title, text string) {
	if len(rest) > 0 {
		switch rest[0] {
		case "resume":
			// The remaining words are the optional resume message.
		case "reconcile":
			if len(rest) != 2 {
				return "Workflow reconcile", "Usage: /workflow reconcile <wfg_id>"
			}
		case "cancel":
			if len(rest) != 2 {
				return "Workflow cancel", "Usage: /workflow cancel <wfg_id>"
			}
		default:
			if len(rest) != 1 {
				return "Workflow", "Usage: /workflow <wfg_id> | resume [message] | reconcile <wfg_id> | cancel <wfg_id>"
			}
		}
	}
	svc := a.WorkflowGraphs()
	if svc == nil {
		return "Workflow", workflowFeatureOffText
	}
	if len(rest) == 0 {
		return "Workflows", workflowGraphListText(svc)
	}
	switch rest[0] {
	case "resume":
		return "Workflow resume", workflowResumeText(ctx, svc, strings.Join(rest[1:], " "))
	case "reconcile":
		return "Workflow reconcile", workflowReconcileText(ctx, svc, rest[1])
	case "cancel":
		return "Workflow cancel", workflowCancelText(svc, rest[1])
	default:
		return "Workflow", workflowGraphDetailText(svc, rest[0])
	}
}

// workflowGraphListText renders the open graphs, one two-line entry each.
func workflowGraphListText(svc *workflowgraph.Service) string {
	graphs, err := svc.List(workflowgraph.OpenStatuses, 10)
	if err != nil {
		return "Failed to list workflow graphs: " + err.Error()
	}
	if len(graphs) == 0 {
		return "(no active workflow graphs)"
	}
	var rows []string
	for _, g := range graphs {
		rows = append(rows, workflowGraphSummaryLines(g)...)
	}
	return strings.Join(rows, "\n")
}

// workflowGraphSummaryLines is the shared two-line list entry (also prepended
// to /workflows):
//
//	● wfg_1a2b3c4d  Fix watcher tests            active
//	  3/5 done · current: Run tests · next: Run tests in wt_4
func workflowGraphSummaryLines(g *workflowgraph.Graph) []string {
	marker := "●"
	if g.Status == workflowgraph.StatusBlocked {
		marker = "⚠"
	}
	first := fmt.Sprintf("%s %s  %s  [%s]", marker, g.ID, graphLine(g.Goal, 48), g.Status)

	parts := []string{fmt.Sprintf("%d/%d done", g.DoneNodeCount(), len(g.Nodes))}
	if cur := g.CurrentNode(); cur != nil {
		parts = append(parts, "current: "+graphLine(cur.Title, 32))
	}
	if blockers := g.OpenBlockers(); len(blockers) > 0 {
		parts = append(parts, fmt.Sprintf("%d blocker(s)", len(blockers)))
	}
	if g.NextAction != nil && g.NextAction.Label != "" {
		parts = append(parts, "next: "+graphLine(g.NextAction.Label, 40))
	}
	return []string{first, "  " + strings.Join(parts, " · ")}
}

// workflowGraphDetailText renders one graph's full plan view.
func workflowGraphDetailText(svc *workflowgraph.Service, id string) string {
	g, revision, err := svc.Get(id)
	if err != nil {
		return "Failed to load workflow graph: " + err.Error()
	}
	if g == nil {
		return "No such workflow graph: " + id + " (use /workflow to list)"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", graphLine(g.Goal, 72))
	fmt.Fprintf(&b, "status: %s · %d/%d nodes done · rev %d · updated %s\n",
		g.Status, g.DoneNodeCount(), len(g.Nodes), revision, localTime(g.UpdatedAt))
	b.WriteString("\n")
	for i := range g.Nodes {
		n := &g.Nodes[i]
		fmt.Fprintf(&b, "%s %s", nodeGlyph(n.Status), graphLine(n.Title, 44))
		var meta []string
		if n.Status != "" {
			meta = append(meta, string(n.Status))
		}
		if n.Owner != "" && n.Owner != "assistant" {
			meta = append(meta, n.Owner)
		}
		if n.LastError != "" {
			meta = append(meta, "err: "+graphLine(n.LastError, 32))
		}
		if len(meta) > 0 {
			fmt.Fprintf(&b, "  (%s)", strings.Join(meta, " · "))
		}
		b.WriteString("\n")
	}
	if len(g.Resources) > 0 {
		var refs []string
		for i := range g.Resources {
			r := &g.Resources[i]
			refs = append(refs, r.Type+" "+r.Ref)
		}
		b.WriteString("\nResources: " + graphLine(strings.Join(refs, " · "), 200) + "\n")
	}
	if blockers := g.OpenBlockers(); len(blockers) > 0 {
		b.WriteString("Blockers:\n")
		for _, bl := range blockers {
			fmt.Fprintf(&b, "  ⚠ %s\n", graphLine(bl.Reason, 64))
		}
	}
	if g.NextAction != nil {
		next := g.NextAction.Label
		if g.NextAction.ToolName != "" {
			next += " (" + g.NextAction.ToolName + ")"
		}
		b.WriteString("Next: " + graphLine(next, 72) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// workflowResumeText renders the backend-ranked resume package.
func workflowResumeText(ctx context.Context, svc *workflowgraph.Service, userMessage string) string {
	out, err := svc.ResumeDigest(ctx, userMessage)
	if err != nil {
		return "Resume digest unavailable: " + err.Error()
	}
	if len(out.Ranked) == 0 {
		return "(no active workflow graphs to resume)"
	}
	var b strings.Builder
	for i, item := range out.Ranked {
		focus := ""
		if item.WorkflowID == out.SuggestedFocusWorkflowID {
			focus = "  ← suggested focus"
		}
		fmt.Fprintf(&b, "%d. %s [%s] %s%s\n", i+1, item.WorkflowID, item.Status, graphLine(item.Goal, 48), focus)
		if item.Summary != "" {
			fmt.Fprintf(&b, "   %s\n", graphLine(item.Summary, 72))
		}
		if item.NextAction != "" {
			fmt.Fprintf(&b, "   next: %s\n", graphLine(item.NextAction, 68))
		}
		if item.BlockedReason != "" {
			fmt.Fprintf(&b, "   blocked: %s\n", graphLine(item.BlockedReason, 64))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// workflowReconcileText runs a manual reconcile and summarizes the outcome.
func workflowReconcileText(ctx context.Context, svc *workflowgraph.Service, id string) string {
	res, err := svc.Reconcile(ctx, workflowgraph.ReconcileRequest{
		WorkflowID: id, Reason: "manual", Apply: true,
	})
	if err != nil {
		return "Reconcile failed: " + err.Error()
	}
	lines := []string{fmt.Sprintf("Reconciled %s → revision %d.", id, res.Revision)}
	if res.Summary != "" {
		lines = append(lines, graphLine(res.Summary, 100))
	}
	if res.NextAction != nil && res.NextAction.Label != "" {
		lines = append(lines, "next: "+graphLine(res.NextAction.Label, 72))
	}
	for _, w := range res.Warnings {
		lines = append(lines, "warning: "+graphLine(w, 80))
	}
	return strings.Join(lines, "\n")
}

// workflowCancelText cancels a graph (local state only).
func workflowCancelText(svc *workflowgraph.Service, id string) string {
	g, _, err := svc.Cancel(id, "", "cancelled by user (/workflow cancel)")
	if err != nil {
		return "Cancel failed: " + err.Error()
	}
	return "Cancelled " + g.ID + " (graph state only — terminals, worktrees, and external work were NOT touched)."
}

// nodeGlyph maps a node status to its plan-view marker.
func nodeGlyph(s workflowgraph.NodeStatus) string {
	switch s {
	case workflowgraph.NodeDone, workflowgraph.NodeSkipped:
		return "✓"
	case workflowgraph.NodeRunning, workflowgraph.NodeWaiting:
		return "→"
	case workflowgraph.NodeFailed:
		return "×"
	case workflowgraph.NodeBlocked:
		return "⚠"
	case workflowgraph.NodeCancelled:
		return "∅"
	default: // pending, ready
		return "○"
	}
}

// graphLine flattens whitespace runs and rune-caps for a single card line.
func graphLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
