package workflowgraph

import (
	"encoding/json"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// This file is the automatic event-capture side of the layer: it turns
// completed tool dispatches and settled async futures into graph evidence and
// resource links WITHOUT altering tool behaviour or safety decisions — it only
// records what dispatch already decided and executed.

// ObservedCall is one completed tool dispatch as the observer sees it (an
// app-layer adapter maps tools.DispatchObservation onto this shape so this
// package never imports the tools package).
type ObservedCall struct {
	ToolName   string
	Args       json.RawMessage
	Result     domain.ToolResult
	Risk       string
	Outcome    string // ok | error | denied | grant_ok
	Actor      string
	RunID      string
	ToolCallID string
}

// ObserveDispatch records a MATERIAL tool call against the right graph/node.
// Best-effort and self-contained: it must never fail the tool call it rides
// on, so every error is swallowed into a trace line. Targeting rules (in
// precedence order):
//  1. an explicit workflowId/workflowNodeId in the tool args;
//  2. exactly ONE open graph → that graph, and its single active node if
//     exactly one is active;
//  3. otherwise: ambiguous — skip (reconciliation can still resolve later via
//     the full audit trail; junk attribution is worse than none).
func (s *Service) ObserveDispatch(obs ObservedCall) {
	defer func() { _ = recover() }()
	if !s.materialCall(obs) {
		return
	}

	workflowID, nodeID := s.targetFor(obs)
	if workflowID == "" {
		return
	}

	patch := &Patch{WorkflowID: workflowID}
	patch.AddEvidence = []EvidenceRef{evidenceFromCall(obs, nodeID)}
	for _, res := range resourcesFromCall(obs, nodeID) {
		patch.AddResources = append(patch.AddResources, res)
	}

	kind := "evidence"
	if len(patch.AddResources) > 0 {
		kind = "resource_linked"
	}
	if _, _, err := s.mutate(workflowID, kind, func(g *Graph, rev int64) (*Patch, string, error) {
		// The targeted node may have vanished under a concurrent patch; degrade
		// to graph-level attribution rather than dropping the evidence.
		if nodeID != "" && g.NodeByID(nodeID) == nil {
			patch.AddEvidence[0].NodeID = ""
			for i := range patch.AddResources {
				patch.AddResources[i].NodeID = ""
			}
		}
		patch.BaseRevision = rev
		return patch, patch.AddEvidence[0].Summary, nil
	}); err != nil {
		s.trace("workflow.observe.failed", map[string]any{
			"workflowId": workflowID, "tool": obs.ToolName, "error": err.Error()})
	}
}

// materialCall filters the firehose down to calls worth recording: mutations
// (terminal/project/external/git/system risk), accepted async work, explicit
// supervision primitives (watcher/timer creation), and unrecoverable
// failures. Plain reads and the workflow tools themselves never qualify — the
// graph must not narrate its own bookkeeping or every poll loop.
func (s *Service) materialCall(obs ObservedCall) bool {
	if strings.HasPrefix(obs.ToolName, "workflow.") {
		return false
	}
	if obs.Outcome == "denied" {
		return false // never record confirmations the user declined
	}
	if obs.Result.Async != nil {
		return true
	}
	switch obs.Risk {
	case "terminal", "project", "external", "git", "system":
		return true
	}
	switch obs.ToolName {
	case "watcher.create", "timer.create":
		return true
	}
	if !obs.Result.Ok && obs.Result.Error != nil && !obs.Result.Error.Recoverable {
		// Dispatch-level model slip-ups (hallucinated names, malformed args) are
		// self-correcting next round — not workflow-material events.
		switch obs.Result.Error.Code {
		case "UNKNOWN_TOOL", "TOOL_NOT_OFFERED", "INVALID_ARGS":
			return false
		}
		return true
	}
	return false
}

// targetFor resolves which graph/node a call belongs to (see ObserveDispatch).
func (s *Service) targetFor(obs ObservedCall) (workflowID, nodeID string) {
	// 1. Explicit ids in the args always win.
	if len(obs.Args) > 0 {
		var probe struct {
			WorkflowID     string `json:"workflowId"`
			WorkflowNodeID string `json:"workflowNodeId"`
		}
		if json.Unmarshal(obs.Args, &probe) == nil && probe.WorkflowID != "" {
			// Only honour ids that reference a real graph of THIS layer.
			if strings.HasPrefix(probe.WorkflowID, domain.PrefixWorkflowGraph) {
				if g, _, err := s.Get(probe.WorkflowID); err == nil && g != nil {
					return probe.WorkflowID, probe.WorkflowNodeID
				}
			}
		}
	}

	// 2. Exactly one open graph → default target.
	graphs, err := s.List(OpenStatuses, 2)
	if err != nil || len(graphs) != 1 {
		return "", ""
	}
	g := graphs[0]
	// Its single non-blocked in-flight node, when unambiguous.
	var active []string
	for i := range g.Nodes {
		switch g.Nodes[i].Status {
		case NodeReady, NodeRunning, NodeWaiting:
			active = append(active, g.Nodes[i].ID)
		}
	}
	if len(active) == 1 {
		return g.ID, active[0]
	}
	return g.ID, ""
}

// evidenceFromCall renders one dispatch as a bounded evidence item.
func evidenceFromCall(obs ObservedCall, nodeID string) EvidenceRef {
	kind := "tool_result"
	verdict := "ok"
	if !obs.Result.Ok {
		verdict = "failed"
		if obs.Result.Error != nil && obs.Result.Error.Code != "" {
			verdict = "failed (" + obs.Result.Error.Code + ")"
		}
	}
	summary := obs.ToolName + " " + verdict
	if s := strings.TrimSpace(obs.Result.Summary); s != "" {
		summary += ": " + s
	}
	return EvidenceRef{
		NodeID:  nodeID,
		Kind:    kind,
		Summary: clampRunes(flattenLine(summary), MaxSummaryRunes),
		RefID:   obs.ToolCallID,
	}
}

// resourceKeyTypes maps well-known result/argument keys to resource types. The
// walk is a bounded known-key scan — never a generic scrape — so only handles
// with a real follow-up use become resources.
var resourceKeyTypes = map[string]string{
	"terminalId": "terminal",
	"watcherId":  "watcher",
	"timerId":    "timer",
	"worktreeId": "worktree",
	"asyncId":    "async",
	"branch":     "branch",
	"prUrl":      "pr",
	"issueUrl":   "issue",
	"artifactId": "artifact",
}

// resourceWalkMaxDepth bounds the nested-map scan of a tool result.
const resourceWalkMaxDepth = 3

// resourcesFromCall extracts the durable handles a call produced: the typed
// async handle first (authoritative), then well-known id keys from the result
// payload.
func resourcesFromCall(obs ObservedCall, nodeID string) []Resource {
	var out []Resource
	seen := make(map[string]bool)
	add := func(resType, ref, label string) {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[resType+"\x00"+ref] {
			return
		}
		seen[resType+"\x00"+ref] = true
		out = append(out, Resource{Type: resType, Ref: ref, Label: label, NodeID: nodeID, Status: "active"})
	}

	if h := obs.Result.Async; h != nil {
		add("async", h.ID, h.Title)
		for _, tid := range h.TerminalIDs {
			add("terminal", tid, "")
		}
	}

	if m, ok := obs.Result.Result.(map[string]any); ok {
		walkResourceKeys(m, 0, func(key, val string) {
			if resType, known := resourceKeyTypes[key]; known {
				add(resType, val, "from "+obs.ToolName)
			}
		})
	}
	return out
}

// walkResourceKeys visits string values under well-known keys in a nested
// map, depth-bounded.
func walkResourceKeys(m map[string]any, depth int, visit func(key, val string)) {
	if depth > resourceWalkMaxDepth {
		return
	}
	for k, v := range m {
		switch tv := v.(type) {
		case string:
			visit(k, tv)
		case map[string]any:
			walkResourceKeys(tv, depth+1, visit)
		}
	}
}

/* ----------------------------- async settlement --------------------------- */

// NoteAsyncSettled routes one settled async invocation back to the graph that
// owns it: evidence is appended, the linked node transitions out of its wait
// (waiting/running → done|failed|cancelled), and the resource link/row status
// is refreshed — so the wake turn continues the ORIGINAL workflow instead of
// treating the completion as an isolated notification. Satisfies the
// asyncwork.WorkflowSink seam structurally. Best-effort: never panics, never
// blocks the coordinator beyond fast local writes.
func (s *Service) NoteAsyncSettled(asyncID, finalStatus, summary, queueEventID string) {
	defer func() { _ = recover() }()
	links, err := s.deps.Store.FindWorkflowResourceLinks("async", asyncID)
	if err != nil || len(links) == 0 {
		return
	}

	var nodeStatus NodeStatus
	switch finalStatus {
	case string(domain.AsyncSucceeded):
		nodeStatus = NodeDone
	case string(domain.AsyncFailed), string(domain.AsyncExpired):
		nodeStatus = NodeFailed
	case string(domain.AsyncCancelled):
		nodeStatus = NodeCancelled
	default:
		nodeStatus = "" // unknown → evidence only, no transition
	}

	for _, link := range links {
		linkNode := ""
		if link.NodeID != nil {
			linkNode = *link.NodeID
		}
		if _, _, err := s.mutate(link.WorkflowID, "async_settled", func(g *Graph, rev int64) (*Patch, string, error) {
			if g.Status.IsTerminal() {
				return nil, "", nil // nothing to update — swallow quietly
			}
			evSummary := clampRunes(flattenLine("async "+asyncID+" "+finalStatus+": "+summary), MaxSummaryRunes)
			p := &Patch{
				WorkflowID:   link.WorkflowID,
				BaseRevision: rev,
				AddEvidence: []EvidenceRef{{
					NodeID: linkNode, Kind: "async_completion", Summary: evSummary, RefID: queueEventID,
				}},
			}
			if res := g.FindResource("async", asyncID); res != nil {
				st := finalStatus
				p.ResourceUpdates = []ResourcePatch{{ID: res.ID, Status: &st}}
			}
			if linkNode != "" && nodeStatus != "" {
				if n := g.NodeByID(linkNode); n != nil && NodeTransitionLegal(n.Status, nodeStatus) && n.Status != nodeStatus {
					st := nodeStatus
					np := NodePatch{ID: linkNode, Status: &st}
					if nodeStatus == NodeFailed {
						msg := clampRunes(flattenLine(summary), MaxSummaryRunes)
						np.LastError = &msg
					}
					p.NodeUpdates = []NodePatch{np}
				} else if n == nil {
					p.AddEvidence[0].NodeID = ""
				}
			}
			return p, evSummary, nil
		}); err != nil {
			s.trace("workflow.async_settled.failed", map[string]any{
				"workflowId": link.WorkflowID, "asyncId": asyncID, "error": err.Error()})
			continue
		}
		// Refresh the reverse-index row (best-effort).
		st := finalStatus
		link.Status = &st
		if err := s.deps.Store.UpsertWorkflowResourceLink(link); err != nil {
			s.trace("workflow.resource_link.upsert_failed", map[string]any{
				"workflowId": link.WorkflowID, "type": "async", "ref": asyncID, "error": err.Error()})
		}
		s.trace("workflow.async_settled", map[string]any{
			"workflowId": link.WorkflowID, "asyncId": asyncID, "status": finalStatus, "queueEventId": queueEventID})
	}
}
