package world

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/daintreehq/daintree-assistant/internal/tools/mcpx"
)

// Serve starts the fake Daintree MCP server over the go-sdk's Streamable HTTP
// handler and returns the /mcp URL for DAINTREE_MCP_URL. Fidelity rules, all
// learned from the real integration:
//
//   - Payloads are returned as TEXT content only — no structuredContent — like
//     live Daintree. Every CLI parser unions structuredContent + text with a
//     text fallback, and at least one real bug (the watcher's "empty getStatus")
//     came from assuming structuredContent; the fake must keep that pressure.
//   - agent.launch returns ONLY {terminalId, location} — the LEGACY minimal
//     shape. Live Daintree now returns launched + full worktree/branch identity;
//     the spawn tool reads terminalId/spawnStatus/worktreeId (falling back to
//     the requested worktree)/taskId, and the fixture keeps the minimal shape
//     so a benchmark run exercises the CLI's tolerance of the old response.
//   - Every documented Daintree tool name is registered (unimplemented ones as
//     recorded failing stubs) so the boot drift check stays silent and a model
//     wandering onto an unmodelled tool shows up in the call log instead of as
//     an opaque "unknown tool" transport error.
type Server struct {
	World *World
	URL   string
	srv   *httptest.Server
}

// Close shuts the HTTP server down.
func (s *Server) Close() { s.srv.Close() }

// anyObjectSchema is the permissive input schema raw AddTool requires.
var anyObjectSchema = json.RawMessage(`{"type":"object"}`)

// benchReadTools are the bench world's READ tools — the ones real Daintree
// declares `kind: "query"` and therefore annotates readOnlyHint=true.
//
// The fake MUST reproduce those annotations, because the client now derives its
// auto-retry classification purely from them (there is no local allowlist any
// more). A read left unannotated here would be classified single-shot, silently
// removing retry coverage from every benchmark scenario that exercises a flaky
// read — the benchmark would still pass while measuring the wrong thing.
var benchReadTools = map[string]bool{
	"actions.getContext":  true,
	"getContext":          true,
	"agent.listAvailable": true,
	"agent.listToolbar":   true,
	"agentSettings.get":   true,
	"cliAvailability.get": true,
	"project.getCurrent":  true,
	"terminal.getOutput":  true,
	"terminal.getStatus":  true,
	"terminal.list":       true,
	"worktree.getCurrent": true,
	"worktree.list":       true,
	// Registered as failing stubs rather than implemented, but still genuine reads
	// on the real host (kind: "query"). Annotating them as mutations would make the
	// fake describe a surface Daintree never serves.
	"git.getProjectPulse":          true,
	"recipe.list":                  true,
	"workflow.prepBranchForReview": true,
	"forge.getPR":                  true,
	"forge.getIssue":               true,
	"forge.listIssues":             true,
	"forge.listPRs":                true,
	"copyTree.generate":            true,
}

// benchAnnotations mirrors Daintree's buildAnnotations: a query is read-only and
// idempotent and never destructive; a command is none of those.
func benchAnnotations(name string) *sdkmcp.ToolAnnotations {
	readOnly := benchReadTools[name]
	destructive := !readOnly
	return &sdkmcp.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		IdempotentHint:  readOnly,
		DestructiveHint: &destructive,
	}
}

// Serve wires every tool and starts the server.
func Serve(w *World) *Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "bench-daintree", Version: "v0.0.1"}, nil)

	implemented := map[string]bool{}
	// addRich registers a tool whose handler may also return a structuredContent
	// payload (nil = text-only, the Daintree norm).
	addRich := func(name string, fn func(args map[string]any) (string, any, bool)) {
		implemented[name] = true
		server.AddTool(&sdkmcp.Tool{
			Name:        name,
			Description: "bench-world " + name,
			InputSchema: anyObjectSchema,
			Annotations: benchAnnotations(name),
		}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			var args map[string]any
			if len(req.Params.Arguments) > 0 {
				_ = json.Unmarshal(req.Params.Arguments, &args)
			}
			w.record(name, args)
			text, structured, isErr := fn(args)
			return &sdkmcp.CallToolResult{
				Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: text}},
				StructuredContent: structured,
				IsError:           isErr,
			}, nil
		})
	}
	add := func(name string, fn func(args map[string]any) (string, bool)) {
		addRich(name, func(args map[string]any) (string, any, bool) {
			text, isErr := fn(args)
			return text, nil, isErr
		})
	}

	// --- identity / boot reads ---------------------------------------------

	projectContext := func(args map[string]any) (string, bool) {
		return fmt.Sprintf(`{"projectName":%q}`, w.ProjectName), false
	}
	add("actions.getContext", projectContext)
	// Older probe name kept alongside actions.getContext (the e2e fake serves it).
	add("getContext", projectContext)

	add("agentSettings.get", func(args map[string]any) (string, bool) {
		agents := map[string]any{}
		for _, id := range w.AgentRoster {
			agents[id] = map[string]any{}
		}
		b, _ := json.Marshal(map[string]any{"agents": agents})
		return string(b), false
	})

	add("worktree.getCurrent", func(args map[string]any) (string, bool) {
		return `{"worktree":null}`, false
	})

	add("worktree.list", func(args map[string]any) (string, bool) {
		type wt struct {
			ID     string `json:"id"`
			Path   string `json:"path"`
			Branch string `json:"branch"`
		}
		var rows []wt
		for _, x := range w.Worktrees {
			rows = append(rows, wt{ID: x.ID, Path: x.Path, Branch: x.Branch})
		}
		b, _ := json.Marshal(map[string]any{"worktrees": rows})
		return string(b), false
	})

	// --- terminals -----------------------------------------------------------

	add("terminal.list", func(args map[string]any) (string, bool) {
		w.mu.Lock()
		defer w.mu.Unlock()
		now := time.Now()
		var rows []map[string]any
		for _, id := range w.order {
			t := w.terminals[id]
			if t.closed {
				continue
			}
			snap := w.snapshotLocked(t, now)
			// Both `name` (the spawn-reconcile parser) and `title` (the roster
			// parser, terminalid.ParseListEntries) are read by real CLI paths.
			rows = append(rows, map[string]any{
				"terminalId": t.ID,
				"id":         t.ID,
				"name":       t.Name,
				"title":      t.Name,
				"kind":       "agent",
				"agentId":    t.AgentID,
				"worktreeId": t.WorktreeID,
				"agentState": snap.State,
			})
		}
		b, _ := json.Marshal(map[string]any{"terminals": rows})
		return string(b), false
	})

	add("terminal.getStatus", func(args map[string]any) (string, bool) {
		ids := stringSlice(args["terminalIds"])
		includeLines := 0
		if inc, ok := args["includeOutput"].(map[string]any); ok {
			includeLines = intArg(inc["lines"], 50)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		now := time.Now()
		var rows []map[string]any
		for _, id := range ids {
			t := w.terminals[id]
			if t == nil || t.closed {
				rows = append(rows, map[string]any{"terminalId": id, "error": "terminal not found"})
				continue
			}
			snap := w.snapshotLocked(t, now)
			row := map[string]any{
				"terminalId": t.ID,
				"agentId":    t.AgentID,
				"agentState": snap.State,
				"spawnedAt":  t.SpawnedAt.UnixMilli(),
			}
			if snap.State == "waiting" && snap.WaitingReason != "" {
				row["waitingReason"] = snap.WaitingReason
			}
			// exitCode presence (not value) signals the exit — absent while running.
			if snap.State == "exited" {
				code := 0
				if snap.ExitCode != nil {
					code = *snap.ExitCode
				}
				row["exitCode"] = code
			}
			if includeLines > 0 {
				if w.Faults.BlankStatusTail {
					// The Codex bottom-padded-TUI quirk: an all-whitespace
					// inline tail even though the deep scrollback has content.
					row["recentOutput"] = strings.Repeat(" \n", includeLines)
				} else {
					row["recentOutput"] = lastLines(snap.Output, includeLines)
				}
			}
			rows = append(rows, row)
		}
		b, _ := json.Marshal(map[string]any{"terminals": rows})
		return string(b), false
	})

	addRich("terminal.getOutput", func(args map[string]any) (string, any, bool) {
		id, _ := args["terminalId"].(string)
		w.mu.Lock()
		defer w.mu.Unlock()
		t := w.terminals[id]
		if t == nil || t.closed {
			return fmt.Sprintf(`{"error":"terminal %s not found"}`, id), nil, true
		}
		snap := w.snapshotLocked(t, time.Now())
		maxLines := intArg(args["maxLines"], 200)
		content := lastLines(snap.Output, maxLines)
		payload := map[string]any{
			"terminalId": t.ID,
			"content":    content,
			"lineCount":  len(strings.Split(content, "\n")),
			"truncated":  false,
		}
		b, _ := json.Marshal(payload)
		// getOutput needs structuredContent: the CLI's mcpStringField reads
		// structuredContent.content and would otherwise take the raw text —
		// i.e. the whole JSON envelope — as the scrollback.
		return string(b), payload, false
	})

	add("agent.launch", func(args map[string]any) (string, bool) {
		agentID, _ := args["agentId"].(string)
		name, _ := args["name"].(string)
		worktreeID, _ := args["worktreeId"].(string)
		prompt, _ := args["prompt"].(string)
		requestKey, _ := args["requestKey"].(string)
		if strings.TrimSpace(prompt) == "" {
			return `{"error":"prompt is required"}`, true
		}
		id := w.launch(agentID, name, worktreeID, prompt, requestKey)
		// Legacy minimal shape: terminalId + location ONLY. Live Daintree now
		// also returns launched + worktree/branch identity; the CLI reads
		// terminalId/spawnStatus/worktreeId (falling back to the requested
		// worktree)/taskId, so the minimal shape exercises that fallback.
		return fmt.Sprintf(`{"terminalId":%q,"location":"window-1"}`, id), false
	})

	add("terminal.sendCommand", func(args map[string]any) (string, bool) {
		id, _ := args["terminalId"].(string)
		cmd, _ := args["command"].(string)
		if !w.sendInput(id, cmd) {
			return fmt.Sprintf(`{"error":"terminal %s not found"}`, id), true
		}
		return `{"ok":true}`, false
	})

	add("terminal.rename", func(args map[string]any) (string, bool) {
		id, _ := args["terminalId"].(string)
		name, _ := args["name"].(string)
		if !w.rename(id, name) {
			return fmt.Sprintf(`{"error":"terminal %s not found"}`, id), true
		}
		return `{"ok":true}`, false
	})

	add("terminal.close", func(args map[string]any) (string, bool) {
		id, _ := args["terminalId"].(string)
		if !w.close(id) {
			return fmt.Sprintf(`{"error":"terminal %s not found"}`, id), true
		}
		return `{"ok":true}`, false
	})

	armed := func(args map[string]any) (string, bool) { return `{"armed":[]}`, false }
	add("terminal.arm", armed)
	add("terminal.disarm", armed)
	add("terminal.disarmAll", armed)

	add("panel.focus", func(args map[string]any) (string, bool) { return `{"ok":true}`, false })

	// Startup discovery reads are intentionally outside the documented tools/list
	// drift baseline, but the real assistant calls them when available. Keep the
	// deterministic world capability-complete so an optional read cannot turn every
	// tool scenario into an unrelated disconnected-MCP fallback.
	add("project.getCurrent", func(args map[string]any) (string, bool) {
		return `{"project":null}`, false
	})
	add("agent.listAvailable", func(args map[string]any) (string, bool) {
		return `{"agents":[],"complete":true,"availabilityComplete":true}`, false
	})
	add("agent.listToolbar", func(args map[string]any) (string, bool) { return `{"agents":[]}`, false })
	add("cliAvailability.get", func(args map[string]any) (string, bool) { return `{"available":true}`, false })

	// --- other host tools the assistant may reach for: failing stubs ---------
	//
	// The stub list is the assistant's own typed-wrapper names, NOT a copy of the
	// production host surface: a bench fake should advertise the tools its scenarios
	// exercise plus the ones the runtime can plausibly call, and let anything else be
	// genuinely absent. (This used to iterate internal/mcp's hand-maintained 59-name
	// baseline, which no longer exists — the client now learns the surface live.)
	for _, name := range mcpx.WrappedMCPToolNames() {
		if implemented[name] {
			continue
		}
		n := name
		add(n, func(args map[string]any) (string, bool) {
			return fmt.Sprintf(`{"error":"%s is not supported by the bench world"}`, n), true
		})
	}

	handler := sdkmcp.NewStreamableHTTPHandler(func(r *http.Request) *sdkmcp.Server {
		return server
	}, &sdkmcp.StreamableHTTPOptions{})
	srv := httptest.NewServer(handler)
	return &Server{World: w, URL: srv.URL + "/mcp", srv: srv}
}

// --- small arg/format helpers -----------------------------------------------

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func intArg(v any, def int) int {
	if f, ok := v.(float64); ok && f > 0 {
		return int(f)
	}
	return def
}

func lastLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
