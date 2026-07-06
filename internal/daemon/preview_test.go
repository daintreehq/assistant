package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// recordingPreviewMCP serves canned getStatus/getOutput responses while
// recording every call, so the preview poller's load shape (ONE batched status
// read, one output read per card) can be pinned.
type recordingPreviewMCP struct {
	mu          sync.Mutex
	statusFails bool       // terminal.getStatus returns a tool-level error
	statusCalls [][]string // terminalIds of each terminal.getStatus call
	outputCalls []string   // terminalId of each terminal.getOutput call
}

func (f *recordingPreviewMCP) CallRead(_ context.Context, name string, args map[string]any) (MCPResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "terminal.getStatus":
		var ids []string
		if raw, ok := args["terminalIds"].([]string); ok {
			ids = append(ids, raw...)
		}
		f.statusCalls = append(f.statusCalls, ids)
		if f.statusFails {
			return MCPResult{Text: "boom", IsError: true}, nil
		}
		body := `{"terminals":[`
		for i, id := range ids {
			if i > 0 {
				body += ","
			}
			body += fmt.Sprintf(`{"terminalId":%q,"agentState":"working"}`, id)
		}
		body += `]}`
		return MCPResult{Text: body}, nil
	case "terminal.getOutput":
		id, _ := args["terminalId"].(string)
		f.outputCalls = append(f.outputCalls, id)
		// Daintree returns scrollback as a RAW text body (parseMcpString falls back
		// to res.Text), never JSON.
		return MCPResult{Text: "tail of " + id}, nil
	}
	return MCPResult{}, nil
}
func (f *recordingPreviewMCP) Connected() bool                               { return true }
func (f *recordingPreviewMCP) SupportsSubscribe() bool                       { return false }
func (f *recordingPreviewMCP) Subscribe(_ context.Context, _ string) error   { return nil }
func (f *recordingPreviewMCP) Unsubscribe(_ context.Context, _ string) error { return nil }

// TestFetchPreviewsBatchesStatusRead pins the wire shape: N cards cost exactly
// ONE batched terminal.getStatus (all ids in one call) plus one
// terminal.getOutput per card — never a per-card status read.
func TestFetchPreviewsBatchesStatusRead(t *testing.T) {
	mcp := &recordingPreviewMCP{}
	targets := []PreviewTarget{
		{TerminalID: "term_1", WatcherID: "wch_1", Title: "one"},
		{TerminalID: "term_2", WatcherID: "wch_2", Title: "two"},
		{TerminalID: "term_3", WatcherID: "wch_3", Title: "three"},
	}
	previews := FetchPreviews(context.Background(), mcp, targets, 42)

	if len(mcp.statusCalls) != 1 {
		t.Fatalf("terminal.getStatus calls = %d, want exactly 1 batched call", len(mcp.statusCalls))
	}
	if got := mcp.statusCalls[0]; len(got) != 3 {
		t.Fatalf("batched getStatus ids = %v, want all 3 targets", got)
	}
	if len(mcp.outputCalls) != 3 {
		t.Fatalf("terminal.getOutput calls = %d, want one per card", len(mcp.outputCalls))
	}

	if len(previews) != 3 {
		t.Fatalf("previews = %d, want 3", len(previews))
	}
	for i, p := range previews {
		if p.TerminalID != targets[i].TerminalID {
			t.Errorf("preview %d terminal = %q, want %q", i, p.TerminalID, targets[i].TerminalID)
		}
		if p.AgentState != "working" {
			t.Errorf("preview %d agentState = %q, want status attributed from the batch", i, p.AgentState)
		}
		if want := "tail of " + targets[i].TerminalID; p.Tail != want {
			t.Errorf("preview %d tail = %q, want %q", i, p.Tail, want)
		}
		if p.UpdatedAt != 42 {
			t.Errorf("preview %d updatedAt = %d, want 42", i, p.UpdatedAt)
		}
	}
}

// TestFetchPreviewsStatusFailureStillServesTails proves a failed status batch
// degrades gracefully: cards keep their identity and still get their output
// tails, just with no agent state.
func TestFetchPreviewsStatusFailureStillServesTails(t *testing.T) {
	mcp := &recordingPreviewMCP{statusFails: true}
	targets := []PreviewTarget{
		{TerminalID: "term_1", WatcherID: "wch_1", Title: "one"},
		{TerminalID: "term_2", WatcherID: "wch_2", Title: "two"},
	}
	previews := FetchPreviews(context.Background(), mcp, targets, 7)

	if len(previews) != 2 {
		t.Fatalf("previews = %d, want 2", len(previews))
	}
	for i, p := range previews {
		if p.AgentState != "" {
			t.Errorf("preview %d agentState = %q, want empty on a failed status batch", i, p.AgentState)
		}
		if want := "tail of " + targets[i].TerminalID; p.Tail != want {
			t.Errorf("preview %d tail = %q, want %q (tails must survive a status failure)", i, p.Tail, want)
		}
	}
}
