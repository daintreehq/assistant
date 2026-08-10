package workflowgraph

import (
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/backend"
)

// Digest caps — the whole point of the digest is to IMPROVE context, not flood
// it, so every list is short and every line is one line.
const (
	digestMaxActiveNodes = 4
	digestMaxResources   = 6
	digestMaxBlockers    = 4
)

// BuildDigest renders one graph as a compact, prompt-ready digest. lastEvent
// is the newest workflow-event summary ("" to omit). The result is Clamp()ed
// to the wire limits by backend.CapWorkflowDigests before it rides a request.
func BuildDigest(g *Graph, lastEvent string) backend.WorkflowDigest {
	d := backend.WorkflowDigest{
		ID:     g.ID,
		Goal:   flattenLine(g.Goal),
		Status: string(g.Status),
	}

	total := len(g.Nodes)
	done := g.DoneNodeCount()
	progress := fmt.Sprintf("%d/%d nodes done", done, total)
	if cur := g.CurrentNode(); cur != nil {
		progress += "; current: " + flattenLine(cur.Title)
	}
	d.Progress = progress

	for i := range g.Nodes {
		n := &g.Nodes[i]
		if !n.Status.IsActive() {
			continue
		}
		d.ActiveNodes = append(d.ActiveNodes, fmt.Sprintf("%s %q [%s]", n.ID, flattenLine(n.Title), n.Status))
		if len(d.ActiveNodes) == digestMaxActiveNodes {
			break
		}
	}

	// Newest resources first — the handles a follow-up action actually needs.
	for i := len(g.Resources) - 1; i >= 0 && len(d.Resources) < digestMaxResources; i-- {
		r := &g.Resources[i]
		line := r.Type + " " + r.Ref
		if r.Label != "" {
			line += ": " + flattenLine(r.Label)
		}
		if r.Status != "" {
			line += " (" + r.Status + ")"
		}
		d.Resources = append(d.Resources, line)
	}

	for _, b := range g.OpenBlockers() {
		line := flattenLine(b.Reason)
		if b.NodeID != "" {
			line = b.NodeID + ": " + line
		}
		d.Blockers = append(d.Blockers, line)
		if len(d.Blockers) == digestMaxBlockers {
			break
		}
	}

	if g.NextAction != nil {
		na := flattenLine(g.NextAction.Label)
		if g.NextAction.ToolName != "" {
			na += " (" + g.NextAction.ToolName + ")"
		}
		d.NextAction = na
	}
	d.LastEvent = flattenLine(lastEvent)
	return d
}

// flattenLine collapses all whitespace runs (incl. newlines) to single spaces
// so one digest field is exactly one line — a multi-line goal or label can
// never break the rendered block or inject a fake row.
func flattenLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
