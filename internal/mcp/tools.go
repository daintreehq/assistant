package mcp

import (
	"encoding/json"
	"strings"
)

// ToolAnnotations is the normalized subset of the MCP `annotations` object this
// client acts on. Only the three hints that bear on RETRY SAFETY are kept —
// title/openWorldHint are presentation/scope metadata with no effect here.
//
// DestructiveHint stays a pointer because the MCP spec defaults it to TRUE when
// absent; collapsing it to a bool would silently read "omitted" as "not
// destructive", which is the unsafe direction. ReadOnlyHint/IdempotentHint
// default to false in the spec, so a plain bool already fails closed.
type ToolAnnotations struct {
	ReadOnlyHint    bool
	IdempotentHint  bool
	DestructiveHint *bool
}

// ToolInfo is a normalized live-tool descriptor (McpToolInfo).
type ToolInfo struct {
	Name        string
	Description string         // optional; "" when absent
	InputSchema map[string]any // defaulted to {"type":"object","properties":{}} when live tool has none
	// Annotations is the server's declared behaviour hints; nil when the tool
	// advertised none. See retrySafeFromAnnotations for how it gates auto-retry.
	Annotations *ToolAnnotations
}

// retrySafeFromAnnotations decides whether a tool may be AUTO-RETRIED after a
// transient transport failure, from the server's own declared annotations. This
// replaces the former hand-maintained `readOnlyToolNames` allowlist: the host
// already ships this metadata on every tools/list entry (Daintree derives it from
// each action's kind/danger), so the client no longer keeps a second, always-stale
// copy of a fact the server states authoritatively.
//
// The rule is deliberately the SPEC's own semantic, not a stricter conjunction:
//
//	retry-safe  ⟺  ReadOnlyHint == true  ∧  ¬(DestructiveHint == true)
//
// ReadOnlyHint means "the tool does not modify its environment", which is exactly
// and only what retry safety requires — re-issuing a call that changes nothing can
// never double-apply. DestructiveHint is checked solely to reject a
// SELF-CONTRADICTORY server (read-only AND destructive); it is not required to be
// present, because the spec declares both DestructiveHint and IdempotentHint
// "meaningful only when readOnlyHint == false". Demanding them on a read-only tool
// would be spec-incoherent and would fail closed against a correctly-annotated
// server, silently costing every read its retry budget.
//
// nil annotations → false (fail closed). A server that says nothing gets no
// auto-retry; the ONLY exception is the explicit per-client fallback in
// Options.ReadOnlyFallback, which applies to a server known not to annotate at all.
func retrySafeFromAnnotations(a *ToolAnnotations) bool {
	if a == nil || !a.ReadOnlyHint {
		return false
	}
	if a.DestructiveHint != nil && *a.DestructiveHint {
		return false
	}
	return true
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
