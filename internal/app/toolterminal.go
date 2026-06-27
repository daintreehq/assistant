package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/tools/extractionx"
	"github.com/daintreehq/daintree-assistant/internal/tools/terminalid"
)

// terminalListTimeout bounds the best-effort terminal.list roster read used for id
// resolution. A var (not const) only so tests can shorten it. Mirrors the boot
// reconcile read budget.
var terminalListTimeout = 5 * time.Second

// terminalReaderAdapter satisfies extractionx.TerminalReader over the concrete MCP
// client. The daemon's identical read helpers (daemon/mcpreads.go) are unexported
// and bound to *daemon.CheckContext, and the family forbids importing internal/daemon,
// so this re-implements the same two reads (terminal.getStatus / terminal.getOutput)
// with the same defensive parse: Daintree returns the payload in the text content
// block (never structuredContent), so we read both and merge, text last.
type terminalReaderAdapter struct{ c *mcp.Client }

func (r terminalReaderAdapter) Connected() bool { return r.c.IsConnected() }

// ListTerminals reads Daintree's live terminal inventory (terminal.list, no args) for
// prefix→canonical id resolution. It is bounded by a CANCEL-based deadline, NOT
// context.WithTimeout: mcp.Client tears the connection down on a DeadlineExceeded (only
// a Canceled counts as an abort), and a best-effort roster read must never degrade a
// working connection just because it was slow (see the mcp-bestEffort-reads rule + the
// boot reconcile read). ok=false on a transport error / error result so the caller fails
// open (skips resolution) rather than blocking a wait on a discovery hiccup.
func (r terminalReaderAdapter) ListTerminals(ctx context.Context) ([]string, bool) {
	cctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(terminalListTimeout, cancel)
	res, err := r.c.CallTool(cctx, "terminal.list", map[string]any{}, mcp.CallOptions{})
	timer.Stop()
	cancel()
	if err != nil || res.IsError {
		return nil, false
	}
	return terminalid.ParseListIDs(res.StructuredContent, res.Text), true
}

func (r terminalReaderAdapter) ReadStatuses(ctx context.Context, terminalIDs []string, includeOutput bool) extractionx.StatusReadResult {
	byID := make(map[string]extractionx.TerminalStatusEntry)
	if len(terminalIDs) == 0 {
		return extractionx.StatusReadResult{OK: true, ByID: byID}
	}
	args := map[string]any{"terminalIds": terminalIDs}
	if includeOutput {
		args["includeOutput"] = map[string]any{"lines": 50, "stripAnsi": true}
	}
	res, err := r.c.CallTool(ctx, "terminal.getStatus", args, mcp.ReadCallOptions())
	if err != nil || res.IsError {
		return extractionx.StatusReadResult{OK: false, ByID: byID}
	}
	for _, t := range parseMCPArray(res, "terminals") {
		e, ok := t.(map[string]any)
		if !ok {
			continue
		}
		id := mcpString(e["terminalId"])
		if id == "" {
			continue
		}
		byID[id] = extractionx.TerminalStatusEntry{
			AgentState:    mcpString(e["agentState"]),
			WaitingReason: mcpString(e["waitingReason"]),
			RecentOutput:  mcpStringPtr(e["recentOutput"]),
			ExitCode:      mcpIntPtr(e["exitCode"]),
		}
	}
	return extractionx.StatusReadResult{OK: true, ByID: byID}
}

func (r terminalReaderAdapter) ReadOutput(ctx context.Context, terminalID string, tailBytes int) extractionx.OutputReadResult {
	res, err := r.c.CallTool(ctx, "terminal.getOutput", map[string]any{
		"terminalId": terminalID,
		"maxLines":   200,
	}, mcp.ReadCallOptions())
	if err != nil || res.IsError {
		return extractionx.OutputReadResult{OK: false}
	}
	content := mcpStringField(res, "content")
	return extractionx.OutputReadResult{OK: true, Value: tailString(content, tailBytes)}
}

// --- shared MCP result parsing (mirror of daemon/mcpreads.go pure parsers) ---

func parseMCPArray(res mcp.CallResult, field string) []any {
	var entries []any
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if arr, ok := sc[field].([]any); ok {
			entries = append(entries, arr...)
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed map[string]any
		if json.Unmarshal([]byte(res.Text), &parsed) == nil {
			if arr, ok := parsed[field].([]any); ok {
				entries = append(entries, arr...)
			}
		}
	}
	return entries
}

func mcpStringField(res mcp.CallResult, field string) string {
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if s, ok := sc[field].(string); ok {
			return s
		}
	}
	return res.Text
}

func mcpString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func mcpStringPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// mcpIntPtr accepts only integral JSON numbers (rejects NaN/Inf/fractional/null).
func mcpIntPtr(v any) *int {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		return nil
	}
	n := int(int64(f))
	return &n
}

func tailString(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
