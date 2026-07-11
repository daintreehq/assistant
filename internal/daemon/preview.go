package daemon

import (
	"context"
	"sync"

	// Aliased: the local `mcp MCP` parameters shadow the package name.
	mcpclient "github.com/daintreehq/daintree-assistant/internal/mcp"
)

// TerminalPreview is one preview card. Raw scrollback shown to the human ONLY —
// NEVER fed to the main model.
type TerminalPreview struct {
	TerminalID    string
	WatcherID     string
	Title         string
	AgentState    string
	RuntimeStatus string
	Tail          string
	UpdatedAt     int64
}

// PreviewTarget is one terminal to preview, attributed to its owning watcher (the
// first watcher to claim a terminal owns its card). The UI builds these by
// filtering kind=="terminal" watchers, flattening targets, deduping by terminalId,
// and capping at MaxTerminals.
type PreviewTarget struct {
	TerminalID string
	WatcherID  string
	Title      string
}

// BuildPreviewTargets flattens terminal watchers' targets into preview targets:
// dedupe by terminalId (first watcher owns the card), cap at MaxTerminals. Pure.
// kinds/titles/targets are parsed by the caller from WatcherRecords.
func BuildPreviewTargets(watchers []PreviewWatcher) []PreviewTarget {
	seen := make(map[string]bool)
	var out []PreviewTarget
	for _, w := range watchers {
		for _, tid := range w.TerminalIDs {
			if tid == "" || seen[tid] {
				continue
			}
			seen[tid] = true
			out = append(out, PreviewTarget{TerminalID: tid, WatcherID: w.ID, Title: w.Title})
			if len(out) >= MaxTerminals {
				return out
			}
		}
	}
	return out
}

// PreviewWatcher is the minimal watcher shape BuildPreviewTargets needs.
type PreviewWatcher struct {
	ID          string
	Title       string
	TerminalIDs []string
}

// FetchPreviews polls the given targets for status + a bounded output tail.
// Best-effort: per-terminal errors are swallowed (a failed card just shows no
// tail/state). This is the Go analogue of the useTerminalPreview poll — a
// caller drives it on a PreviewPollMS tick (a tea.Cmd or background goroutine).
//
// Load shape: ONE batched terminal.getStatus covers every card's status (the
// per-card single-id reads this replaced were 4 wire calls where 1 suffices),
// then one bounded terminal.getOutput per card for the tail — getOutput has no
// batch form, and the inline recentOutput screen-grab is deliberately NOT used
// here (bottom-padded TUIs grab as all-blank, which would blank the card even
// though scrollback has real content). The per-card reads still run
// concurrently for latency, but actual wire pressure is bounded by the MCP
// client's global governor.
//
// The tail is capped to previewTailBytes; status is attributed only when the
// returned id matches the requested one.
func FetchPreviews(ctx context.Context, mcp MCP, targets []PreviewTarget, now int64) []TerminalPreview {
	// Preview polls are Refresh-class MCP traffic: a card can show a slightly
	// stale tail, but the poll shouldn't be parked behind Background maintenance.
	ctx = mcpclient.WithPriority(ctx, mcpclient.PriorityRefresh)
	out := make([]TerminalPreview, len(targets))
	ids := make([]string, len(targets))
	for i, t := range targets {
		ids[i] = t.TerminalID
	}
	// Recover-guarded like the per-card reads below: previews are best-effort UI
	// chrome, so a panicking MCP adapter must yield status-less cards, never
	// escape into the dashboard build.
	statuses := func() (b StatusBatch) {
		defer func() {
			if recover() != nil {
				b = StatusBatch{Ok: false, ByID: map[string]TerminalStatusEntry{}}
			}
		}()
		return readStatusesWith(ctx, mcp, ids, false)
	}()

	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t PreviewTarget) {
			defer wg.Done()
			defer func() { _ = recover() }()
			p := TerminalPreview{
				TerminalID: t.TerminalID,
				WatcherID:  t.WatcherID,
				Title:      t.Title,
				UpdatedAt:  now,
			}
			if entry, ok := statuses.ByID[t.TerminalID]; statuses.Ok && ok {
				p.AgentState = entry.AgentState
				p.RuntimeStatus = runtimeFromAgentState(p.AgentState)
			}
			res, err := mcp.CallRead(ctx, "terminal.getOutput", map[string]any{
				"terminalId": t.TerminalID,
				"maxLines":   previewMaxLines,
			})
			if err == nil && !res.IsError {
				content, _ := parseMcpString(res, "content")
				p.Tail = tailBytes(content, previewTailBytes)
			}
			out[i] = p
		}(i, t)
	}
	wg.Wait()
	return out
}
