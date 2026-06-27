package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/tools/extractionx"
	"github.com/daintreehq/daintree-assistant/internal/tools/terminalid"
)

// terminalListTimeout bounds the best-effort terminal.list roster read used for id
// resolution. A var (not const) only so tests can shorten it. Mirrors the boot
// reconcile read budget.
var terminalListTimeout = 5 * time.Second

// openTerminalsTimeout bounds the best-effort per-turn open-terminal inventory read
// (one terminal.list + one no-output terminal.getStatus, together) attached to the
// runtime block. A var (not const) only so tests can shorten it. Independent of
// terminalListTimeout so the two read budgets can be tuned separately.
var openTerminalsTimeout = 5 * time.Second

// mcpToolCaller is the minimal MCP surface the terminal read adapter needs: connection
// state plus a single tool call. A narrow interface (satisfied by *mcp.Client) so the
// adapter's read/merge logic is unit-testable with a scripted fake, with no live MCP.
type mcpToolCaller interface {
	IsConnected() bool
	CallTool(ctx context.Context, name string, args map[string]any, opts mcp.CallOptions) (mcp.CallResult, error)
}

// terminalReaderAdapter satisfies extractionx.TerminalReader over the MCP client. The
// daemon's identical read helpers (daemon/mcpreads.go) are unexported and bound to
// *daemon.CheckContext, and the family forbids importing internal/daemon, so this
// re-implements the same reads (terminal.getStatus / terminal.getOutput) with the same
// defensive parse: Daintree returns the payload in the text content block (never
// structuredContent), so we read both and merge, text last.
type terminalReaderAdapter struct{ c mcpToolCaller }

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

// FetchOpenTerminals reads a fresh, metadata-only snapshot of the open Daintree
// terminals for the per-turn runtime inventory: one terminal.list (full rows) followed
// by one no-output terminal.getStatus to refresh the volatile agentState/waitingReason/
// exitCode. It is best-effort and MUST NEVER block or break a turn — every failure path
// (disconnected, list error, empty roster, status error) returns the best snapshot it
// has (nil when there is nothing), never an error. The WHOLE two-call operation is
// bounded by a CANCEL-based deadline (time.AfterFunc(cancel), NOT context.WithTimeout):
// mcp.Client tears the connection down on a DeadlineExceeded, so a slow inventory read
// must abort via Canceled to avoid degrading a working connection (the mcp-bestEffort
// rule). getStatus is SKIPPED entirely when the roster is empty — no wasted call, and the
// list fields alone already describe a terminal with no live agent.
func (r terminalReaderAdapter) FetchOpenTerminals(ctx context.Context) []backend.OpenTerminal {
	if !r.Connected() {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(openTerminalsTimeout, cancel)
	defer func() {
		timer.Stop()
		cancel()
	}()

	res, err := r.c.CallTool(cctx, "terminal.list", map[string]any{}, mcp.CallOptions{})
	if err != nil || res.IsError {
		return nil
	}
	entries := terminalid.ParseListEntries(res.StructuredContent, res.Text)
	if len(entries) == 0 {
		return nil
	}

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	// Refresh the volatile fields from one no-output getStatus, sharing the same
	// cancel-bounded ctx so the whole inventory stays inside one read budget. A status
	// failure (OK=false) leaves the list-derived fields intact — degrade, don't drop.
	statuses := r.ReadStatuses(cctx, ids, false)

	out := make([]backend.OpenTerminal, 0, len(entries))
	for _, e := range entries {
		ot := backend.OpenTerminal{
			ID:         e.ID,
			Kind:       e.Kind,
			WorktreeID: e.WorktreeID,
			Title:      e.Title,
			AgentID:    e.AgentID,
			AgentState: e.AgentState,
		}
		if statuses.OK {
			if st, ok := statuses.ByID[e.ID]; ok {
				if st.AgentState != "" {
					ot.AgentState = st.AgentState // status carries the fresher transition
				}
				ot.WaitingReason = st.WaitingReason
				ot.ExitCode = st.ExitCode
			}
		}
		out = append(out, ot)
	}
	return out
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
