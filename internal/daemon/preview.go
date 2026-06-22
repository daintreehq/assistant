package daemon

import (
	"context"
	"sync"
)

// TerminalPreview is one preview card. Raw scrollback shown to the human ONLY —
// NEVER fed to the main model (useTerminalPreview.ts invariant).
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

// FetchPreviews polls the given targets concurrently for status + a bounded output
// tail. Best-effort: per-terminal errors are swallowed (a failed card just shows
// no tail/state). This is the Go analogue of the useTerminalPreview poll — a
// caller drives it on a PreviewPollMS tick (a tea.Cmd or background goroutine).
//
// The tail is capped to previewTailBytes; status is attributed only when the
// returned id matches the requested one.
func FetchPreviews(ctx context.Context, mcp MCP, targets []PreviewTarget, now int64) []TerminalPreview {
	out := make([]TerminalPreview, len(targets))
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
			// Parallel status + output (one terminal each).
			var sw sync.WaitGroup
			sw.Add(2)
			go func() {
				defer sw.Done()
				defer func() { _ = recover() }()
				res, err := mcp.CallRead(ctx, "terminal.getStatus", map[string]any{"terminalIds": []string{t.TerminalID}})
				if err != nil || res.IsError {
					return
				}
				for _, e := range parseMcpArray(res, "terminals") {
					m, ok := e.(map[string]any)
					if !ok {
						continue
					}
					// Attribute status only when the returned id matches.
					if asString(m["terminalId"]) != t.TerminalID {
						continue
					}
					p.AgentState = asString(m["agentState"])
					p.RuntimeStatus = runtimeFromAgentState(p.AgentState)
				}
			}()
			go func() {
				defer sw.Done()
				defer func() { _ = recover() }()
				res, err := mcp.CallRead(ctx, "terminal.getOutput", map[string]any{
					"terminalId": t.TerminalID,
					"maxLines":   previewMaxLines,
				})
				if err != nil || res.IsError {
					return
				}
				content, _ := parseMcpString(res, "content")
				p.Tail = tailBytes(content, previewTailBytes)
			}()
			sw.Wait()
			out[i] = p
		}(i, t)
	}
	wg.Wait()
	return out
}
