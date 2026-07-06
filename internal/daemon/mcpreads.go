package daemon

import (
	"context"
	"encoding/json"
	"strings"
)

// Pure MCP result parsers. Daintree's terminal tools return their payload only in
// the text content blocks, never structuredContent — so these read
// structuredContent FIRST then fall back to the text body. Never throw; a
// missing/garbled source yields the empty result.

// parseMcpArray extracts a named array (e.g. "terminals"), merging entries from
// BOTH structuredContent[field] and a JSON-encoded text body. structuredContent
// entries first, then text entries (callers keying by id in a map take the LAST
// occurrence — text).
func parseMcpArray(res MCPResult, field string) []any {
	var entries []any
	if res.StructuredContent != nil {
		if arr, ok := res.StructuredContent[field].([]any); ok {
			entries = append(entries, arr...)
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(res.Text), &parsed); err == nil {
			if arr, ok := parsed[field].([]any); ok {
				entries = append(entries, arr...)
			}
		}
	}
	return entries
}

// parseMcpString reads a string field, falling back to the raw text body when the
// structured field is absent (terminal scrollback is a RAW string in text, never
// JSON — so the fallback is the raw text, never a parse). Returns ("", false) when
// neither source provides a string, so callers can distinguish "no content" from
// an empty string.
func parseMcpString(res MCPResult, field string) (string, bool) {
	if res.StructuredContent != nil {
		if s, ok := res.StructuredContent[field].(string); ok {
			return s, true
		}
	}
	// In Go an MCPResult always carries a (possibly empty) Text; treat presence as
	// "the server populated a text body" only when adapters set it. We mirror the
	// TS `typeof res.text === "string"` check: an empty Text still counts as a
	// string. Adapters leave Text == "" when there genuinely is none, which the
	// callers handle (an errored read is gated by IsError before value use).
	return res.Text, true
}

// --- read result types -------------------------------------------------------

// TerminalStatusEntry is a single terminal's status from a batched
// terminal.getStatus call. Integer fields are validated to reject NaN/Infinity/
// fractional/null/string values (→ nil).
type TerminalStatusEntry struct {
	TerminalID string
	// AgentID keys the subscribable agent-state resource
	// (daintree://agent/{agentId}/state). Empty for a non-agent terminal, which
	// then stays on the polling path (nothing to subscribe to).
	AgentID          string
	AgentState       string
	WaitingReason    string
	Error            string
	RecentOutput     *string // nil when absent; "" is a valid "no output yet"
	ExitCode         *int
	SpawnedAt        *int64
	LastTransitionAt *int64
}

// agentStateResourceURI builds the subscribable resource URI for an agent's FSM
// state. Empty agentId → empty URI (caller must not subscribe). Kept in one place
// so the subscribe, re-subscribe, and unsubscribe paths form the URI identically.
func agentStateResourceURI(agentID string) string {
	if agentID == "" {
		return ""
	}
	return "daintree://agent/" + agentID + "/state"
}

// StatusBatch is the result of a batched status read. Ok=false ⇒ the call failed
// (a missing id is NOT "closed"); Ok=true + absent id ⇒ gone.
type StatusBatch struct {
	Ok   bool
	ByID map[string]TerminalStatusEntry
}

// readStatuses batch-reads N terminals in ONE terminal.getStatus call. When
// includeOutput is true it requests a bounded inline tail (capped at the
// Daintree-documented 50 lines) so the common case needs no per-terminal
// getOutput. recentOutput may still be absent per entry — callers fall back to
// readOutput.
func readStatuses(ctx *CheckContext, terminalIDs []string, includeOutput bool) StatusBatch {
	return readStatusesWith(ctx.Ctx, ctx.MCP, terminalIDs, includeOutput)
}

// readStatusesWith is the CheckContext-free core of readStatuses, shared with
// callers that hold a bare MCP seam (the preview poller) — every consumer of
// terminal.getStatus in this package MUST batch through here rather than issue
// per-terminal calls (one wire call per cohort is the load contract).
func readStatusesWith(ctx context.Context, mcp MCP, terminalIDs []string, includeOutput bool) StatusBatch {
	byID := make(map[string]TerminalStatusEntry)
	if len(terminalIDs) == 0 {
		return StatusBatch{Ok: true, ByID: byID}
	}
	args := map[string]any{"terminalIds": terminalIDs}
	if includeOutput {
		args["includeOutput"] = map[string]any{"lines": 50, "stripAnsi": true}
	}
	res, err := mcp.CallRead(ctx, "terminal.getStatus", args)
	if err != nil || res.IsError {
		return StatusBatch{Ok: false, ByID: byID}
	}
	// Daintree returns the terminals array in the text content blocks, not
	// structuredContent — read BOTH and merge.
	for _, t := range parseMcpArray(res, "terminals") {
		e, ok := t.(map[string]any)
		if !ok {
			continue
		}
		id := asString(e["terminalId"])
		if id == "" {
			continue
		}
		byID[id] = TerminalStatusEntry{
			TerminalID:       id,
			AgentID:          asString(e["agentId"]),
			AgentState:       asString(e["agentState"]),
			WaitingReason:    asString(e["waitingReason"]),
			Error:            asString(e["error"]),
			RecentOutput:     asStringPtr(e["recentOutput"]),
			ExitCode:         asIntPtr(e["exitCode"]),
			SpawnedAt:        asInt64Ptr(e["spawnedAt"]),
			LastTransitionAt: asInt64Ptr(e["lastTransitionAt"]),
		}
	}
	return StatusBatch{Ok: true, ByID: byID}
}

// ReadOutputResult distinguishes a failed read (Ok=false) from a genuinely silent
// terminal (Ok=true, Value=""). Never conflate them — conflation falsely advances
// noOutputForMs and re-runs the classifier on stale input.
type ReadOutputResult struct {
	Ok    bool
	Value string
}

// readOutput reads a bounded tail of one terminal via terminal.getOutput. The
// isError guard runs BEFORE value use so error JSON never leaks as fake output.
func readOutput(ctx *CheckContext, terminalID string) ReadOutputResult {
	res, err := ctx.MCP.CallRead(ctx.Ctx, "terminal.getOutput", map[string]any{
		"terminalId": terminalID,
		"maxLines":   200,
	})
	if err != nil || res.IsError {
		return ReadOutputResult{Ok: false}
	}
	content, _ := parseMcpString(res, "content")
	return ReadOutputResult{Ok: true, Value: tailBytes(content, 12000)}
}

// ListedTerminal is a terminal as reported by the authoritative terminal.list.
type ListedTerminal struct {
	AgentState    string
	WaitingReason string
	ExitCode      *int
}

// TerminalListResult is the terminal.list inventory. Ok is true ONLY when the call
// succeeded AND returned a recognizable terminals array (even empty). A non-empty
// array yielding ZERO parseable ids → schema drift → Ok=false (a target absent
// from it is not falsely declared exited).
type TerminalListResult struct {
	Ok   bool
	ByID map[string]ListedTerminal
}

// readTerminalList reads Daintree's authoritative terminal inventory, keyed by id
// (fallback terminalId). Merges structuredContent.terminals AND a JSON text body.
// Never throws.
func readTerminalList(ctx *CheckContext) TerminalListResult {
	byID := make(map[string]ListedTerminal)
	res, err := ctx.MCP.CallRead(ctx.Ctx, "terminal.list", map[string]any{})
	if err != nil || res.IsError {
		return TerminalListResult{Ok: false, ByID: byID}
	}
	var entries []any
	foundArray := false
	if res.StructuredContent != nil {
		if arr, ok := res.StructuredContent["terminals"].([]any); ok {
			entries = append(entries, arr...)
			foundArray = true
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed map[string]any
		if json.Unmarshal([]byte(res.Text), &parsed) == nil {
			if arr, ok := parsed["terminals"].([]any); ok {
				entries = append(entries, arr...)
				foundArray = true
			}
		}
	}
	if !foundArray {
		return TerminalListResult{Ok: false, ByID: byID}
	}
	for _, t := range entries {
		e, ok := t.(map[string]any)
		if !ok {
			continue
		}
		id := asString(e["id"])
		if id == "" {
			id = asString(e["terminalId"])
		}
		if id == "" {
			continue
		}
		byID[id] = ListedTerminal{
			AgentState:    asString(e["agentState"]),
			WaitingReason: asString(e["waitingReason"]),
			ExitCode:      asIntPtr(e["exitCode"]),
		}
	}
	// A non-empty inventory whose entries yielded ZERO parseable ids is schema
	// drift, not "everything is gone" — treat as unreadable.
	if len(entries) > 0 && len(byID) == 0 {
		return TerminalListResult{Ok: false, ByID: byID}
	}
	return TerminalListResult{Ok: true, ByID: byID}
}

// --- defensive coercion helpers ----------------------------------------------

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asStringPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// asIntPtr accepts only integral JSON numbers (Number.isInteger semantics):
// rejects NaN, Infinity, fractional values, null, strings.
func asIntPtr(v any) *int {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		return nil
	}
	n := int(int64(f))
	return &n
}

func asInt64Ptr(v any) *int64 {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		return nil
	}
	n := int64(f)
	return &n
}

// tailBytes returns the last n bytes (runes-safe via byte slice on the string),
// matching JS slice(-n) on the string length.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
