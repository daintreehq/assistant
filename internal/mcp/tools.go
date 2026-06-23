package mcp

import (
	"encoding/json"
	"strings"
)

// ToolInfo is a normalized live-tool descriptor (McpToolInfo).
type ToolInfo struct {
	Name        string
	Description string         // optional; "" when absent
	InputSchema map[string]any // defaulted to {"type":"object","properties":{}} when live tool has none
}

// defaultInputSchema returns a fresh copy of the substitute schema used when a
// live tool advertises no inputSchema. A fresh map per call so callers can mutate
// without aliasing.
func defaultInputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// GrantToolNames is the forward-compatible allowlist for Daintree's (future)
// external grants API. EMPTY today: Daintree exposes its grant lifecycle only to
// its own renderer over IPC, not over MCP. Kept an EXACT allowlist (not a
// heuristic) so HasGrantSupport can never false-positive; when Daintree ships the
// API, populate this and the seam lights up with no other change.
var GrantToolNames = []string{}

// ToolsAdvertiseGrantSupport is a pure predicate: returns false immediately when
// grantNames is empty (always, today). Otherwise true iff ANY grant-tool name is
// present in tools. Exported so the seam is unit-testable without a live
// connection. Keep the empty-list short-circuit.
func ToolsAdvertiseGrantSupport(tools []ToolInfo, grantNames []string) bool {
	if len(grantNames) == 0 {
		return false
	}
	live := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		live[t.Name] = struct{}{}
	}
	for _, g := range grantNames {
		if _, ok := live[g]; ok {
			return true
		}
	}
	return false
}

// DocumentedMcpToolNames is the EXACT, ORDERED drift baseline. Hand-maintained
// (NOT parsed from prose, which names non-existent tools as negative examples).
// Drift is missing-only:
// documented names absent from the live server are warned; extra live tools are
// expected and ignored. 59 names — keep byte-stable with the reference prose.
var DocumentedMcpToolNames = []string{
	"actions.getContext", "actions.list", "actions.search", "actions.getSchema",
	"agent.focusNextAgent", "agent.focusNextWaiting", "agent.focusNextWorking",
	"agent.focusPreviousAgent", "agent.launch", "agentSettings.get",
	"copyTree.generate", "copyTree.generateAndCopyFile", "copyTree.injectToTerminal",
	"forge.addIssueComment", "forge.addIssueLabel", "forge.approvePR", "forge.assignIssue",
	"forge.closeIssue", "forge.closePR", "forge.commentOnPR", "forge.convertPRToDraft",
	"forge.createIssue", "forge.createPR", "forge.dismissReview", "forge.editIssue", "forge.editPR",
	"forge.getIssue", "forge.getPR", "forge.listIssues", "forge.listPRs",
	"forge.markPRReadyForReview", "forge.mergePR", "forge.removeIssueLabel", "forge.reopenIssue",
	"forge.reopenPR", "forge.requestChanges", "forge.requestReviewers", "forge.unassignIssue",
	"git.getProjectPulse", "git.snapshotDelete", "git.snapshotRevert",
	"panel.focus", "recipe.list", "recipe.run",
	"terminal.arm", "terminal.close", "terminal.disarm", "terminal.disarmAll", "terminal.getOutput",
	"terminal.getStatus", "terminal.list", "terminal.sendCommand", "terminal.waitUntilIdle",
	"workflow.focusNextAttention", "workflow.prepBranchForReview", "workflow.startWorkOnIssue",
	"worktree.createWithRecipe", "worktree.getCurrent", "worktree.list",
}

// readOnlyToolNames is the allowlist of Daintree MCP tools that are safe to
// auto-retry on a transient transport failure: they only READ Daintree state, so
// re-issuing one after a blip can never double-apply a mutation. CallTool honors
// CallOptions.Retries ONLY for a name in this set — every other tool (mutations
// like terminal.sendCommand / agent.launch / recipe.run, and even non-mutating
// UI-focus calls, which carry no read result worth retrying) is forced single-shot.
//
// Source of truth for "read tool" is the MCP reference (daintree_mcp.go): the
// workbench-tier reads (actions.*, worktree.list/getCurrent, git.getProjectPulse,
// terminal.list) plus the other pure getters (terminal.getStatus/getOutput, the
// forge get/list reads, recipe.list). Keep this an EXACT allowlist, not a heuristic
// on a "get"/"list" prefix, so a future mutating tool that happens to start with
// "list"/"get" can never be silently retried.
var readOnlyToolNames = map[string]struct{}{
	"actions.getContext":  {},
	"actions.list":        {},
	"actions.search":      {},
	"actions.getSchema":   {},
	"worktree.list":       {},
	"worktree.getCurrent": {},
	"git.getProjectPulse": {},
	"terminal.list":       {},
	"terminal.getStatus":  {},
	"terminal.getOutput":  {},
	"forge.getIssue":      {},
	"forge.getPR":         {},
	"forge.listIssues":    {},
	"forge.listPRs":       {},
	"recipe.list":         {},
}

// isReadOnlyToolName reports whether name is on the read-only auto-retry allowlist.
func isReadOnlyToolName(name string) bool {
	_, ok := readOnlyToolNames[name]
	return ok
}

// ReadProjectName is the pure project-name extractor (readProjectName/
// fetchProjectName). Given an actions.getContext call result it reads
// structuredContent first; if absent it parses res.text as JSON. From either
// object it extracts a top-level "projectName" or a nested project.name, trimmed
// and non-empty. Any failure → "" (caller keeps its provisional name).
//
// The text-JSON fallback is LOAD-BEARING: Daintree only emits structuredContent
// when an action declares an output schema, but ALWAYS serializes the same object
// into text.
func ReadProjectName(res CallResult) string {
	if name := projectNameFromAny(res.StructuredContent); name != "" {
		return name
	}
	if strings.TrimSpace(res.Text) == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(res.Text), &parsed); err != nil {
		return ""
	}
	return projectNameFromAny(parsed)
}

// projectNameFromAny extracts projectName / project.name from a decoded JSON
// object. Returns "" for any non-object or missing/blank value.
func projectNameFromAny(v any) string {
	obj, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if pn, ok := obj["projectName"].(string); ok {
		if t := strings.TrimSpace(pn); t != "" {
			return t
		}
	}
	if proj, ok := obj["project"].(map[string]any); ok {
		if pn, ok := proj["name"].(string); ok {
			if t := strings.TrimSpace(pn); t != "" {
				return t
			}
		}
	}
	return ""
}
