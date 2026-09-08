package mcp

import "encoding/json"

// TerminalStatusViewless reports whether a terminal.getStatus response came back
// through Daintree's REDUCED, view-less projection (`source:"pty"`, Daintree
// PR #12318) rather than the renderer's authoritative one.
//
// It matters for exactly one row shape: a per-entry error with a null agentState.
// From the RENDERER that shape is proof the id no longer resolves. From the PTY
// fallback it is ambiguous — the host may simply have no view of a terminal that
// is still alive — so such a row must never be condemned on the status read
// alone. It is NOT a batch-level outage: the healthy siblings in the same
// response are real, and treating the whole batch as failed would suppress the
// roster arbitration (terminal.list) that is the only thing able to RESOLVE the
// ambiguity, turning a permanently-repeating row into a permanent stall.
func TerminalStatusViewless(structured any, text string) bool {
	if body, ok := structured.(map[string]any); ok && body["source"] == "pty" {
		return true
	}
	var body map[string]any
	return json.Unmarshal([]byte(text), &body) == nil && body["source"] == "pty"
}
