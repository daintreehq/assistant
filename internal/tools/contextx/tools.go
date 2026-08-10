package contextx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

/* ----------------------------- context.snapshot --------------------------- */

var snapshotSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

func newSnapshotTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "context.snapshot",
		Description: "Build a compact snapshot of the current workspace: the endpoints this assistant is wired to (Daintree MCP URL + " +
			"assistant backend URL), Daintree MCP status, and (when connected) action context, " +
			"worktrees, terminals, plus the open attention queue. Best-effort and read-only; degrades gracefully when Daintree is offline.",
		Risk:   domain.RiskRead,
		Schema: snapshotSchema,
		Handle: func(ctx context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			status := deps.MCP.Status()

			actionContext, acOK := tryCall(ctx, deps.MCP, "actions.getContext", map[string]any{})
			worktrees, wtOK := tryCall(ctx, deps.MCP, "worktree.list", map[string]any{})
			terminals, tOK := tryCall(ctx, deps.MCP, "terminal.list", map[string]any{})

			// Open attention queue (CLI-local, always available).
			sev := domain.SeverityAttention
			maxItems := 10
			inbox := deps.Queue.Digest(domain.QueueDigestOptions{SeverityAtLeast: &sev, MaxItems: &maxItems})
			inboxText := deps.Queue.Format(inbox)

			var lines []string
			head := fmt.Sprintf("Daintree MCP: %s", connectedWord(status.Connected))
			// Name the endpoint on both branches — "which server?" is precisely what a
			// broken link (and a user asking where this assistant is pointed) needs.
			if status.URL != "" {
				head += " at " + status.URL
			}
			if status.Transport != "" {
				head += fmt.Sprintf(" (%s)", status.Transport)
			}
			if status.ToolCount != nil {
				head += fmt.Sprintf(", %d tools", *status.ToolCount)
			}
			if !status.Connected && status.Error != "" {
				head += " — " + status.Error
			}
			lines = append(lines, head)
			backendURL := backendURLOf(deps)
			if backendURL != "" {
				lines = append(lines, "Assistant backend: "+backendURL)
			}
			if !status.Connected {
				lines = append(lines, "Degraded local mode: worktree/terminal/action context unavailable until Daintree connects.")
			} else {
				lines = append(lines, "Action context: "+availableWord(acOK))
				lines = append(lines, "Worktrees: "+availableWord(wtOK))
				lines = append(lines, "Terminals: "+availableWord(tOK))
			}
			plural := "s"
			if len(inbox) == 1 {
				plural = ""
			}
			lines = append(lines, fmt.Sprintf("Inbox (attention+): %d open event%s", len(inbox), plural))
			if len(inbox) > 0 {
				lines = append(lines, inboxText)
			}

			return tools.Ok(strings.Join(lines, "\n"), map[string]any{
				"mcp":           status,
				"backendUrl":    backendURL,
				"actionContext": contentOrNil(actionContext, acOK),
				"worktrees":     contentOrNil(worktrees, wtOK),
				"terminals":     contentOrNil(terminals, tOK),
				"inbox":         inbox,
			})
		},
	}
}

// backendURLOf reads the injected backend endpoint without ever letting that side
// channel break the call. context.snapshot promises it NEVER throws — it is the tool
// the model reaches for when everything else is broken — and Deps.BackendURL is an
// arbitrary caller-supplied func (production reads an atomic through backend.Swappable,
// but an injected or half-built one could panic). A failed read just omits the line.
func backendURLOf(deps Deps) (url string) {
	if deps.BackendURL == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			url = ""
		}
	}()
	return strings.TrimSpace(deps.BackendURL())
}

func connectedWord(c bool) string {
	if c {
		return "connected"
	}
	return "disconnected"
}

func availableWord(ok bool) string {
	if ok {
		return "available"
	}
	return "unavailable"
}

/* ---------------------------- terminal.summarize -------------------------- */

type summarizeArgs struct {
	TerminalID string `json:"terminalId"`
	Purpose    string `json:"purpose,omitempty"`
	TailBytes  *int   `json:"tailBytes,omitempty"`
}

// Validate enforces the required terminalId + the tailBytes bound (Zod:
// string + number.int.positive.max(100000)). A missing terminalId would read
// nothing; a negative/oversized tailBytes would make the lastRunes cap degenerate.
func (a *summarizeArgs) Validate() error {
	if strings.TrimSpace(a.TerminalID) == "" {
		return fmt.Errorf("terminalId is required")
	}
	if a.TailBytes != nil && (*a.TailBytes < 1 || *a.TailBytes > 100_000) {
		return fmt.Errorf("tailBytes must be between 1 and 100000")
	}
	return nil
}

// summarizeDefaultTailBytes caps the tail fed to the summarizer when the caller
// does not pass tailBytes — the same 12k default as the terminal.extract family.
// The 200-line read alone is unbounded in WIDTH (a wide, repainted TUI frame can
// run far past 100KB), and an over-long noisy tail is exactly what made the
// summarizer lose the answer sitting at the end (ses_49ca848d).
const summarizeDefaultTailBytes = 12_000

var summarizeSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Daintree terminal id to summarize." },
    "purpose": { "type": "string", "description": "What this summary is for (focuses the model)." },
    "tailBytes": { "type": "integer", "minimum": 1, "maximum": 100000, "default": 12000, "description": "Max characters of terminal tail to summarize (default 12000)." }
  },
  "required": ["terminalId"]
}`)

func newSummarizeTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.summarize",
		Description: "Read a bounded tail of a Daintree terminal's output and summarize it with the small model. This is the DEFAULT way to " +
			"relay what an agent said: a coding agent's raw scrollback is garbled, repainted TUI output, so summarizing it gives clean prose " +
			"and keeps it out of your context. Prefer this over terminal.read unless the user needs the exact literal text. PARALLEL: " +
			"summarize/extract calls batched in ONE reply run CONCURRENTLY — to relay a whole cohort, emit one summarize per terminal as one " +
			"batch of calls, not one per turn; the total wait is roughly the slowest single call. Read-only; requires Daintree MCP.",
		Risk: domain.RiskRead,
		// Independent per-call snapshot read + small-model call, the same cost profile
		// as terminal.extract (seconds each): a cohort relay (one summarize per agent)
		// runs concurrently instead of stacking N backend round-trips. Safe because each
		// call reads its own terminal tail and has no ordering dependency on siblings —
		// and it has no wait/barrier mode at all.
		Parallelizable: true,
		Schema:         summarizeSchema,
		Decode:         tools.StrictDecoder(func() any { return &summarizeArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a summarizeArgs
			_ = json.Unmarshal(raw, &a)

			if !deps.MCP.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
			if ctx.Err() != nil {
				return tools.Fail(codeCancelled, "Turn cancelled while reading terminal output.", tools.Unrecoverable())
			}
			resolvedID, idFail := resolveTerminalID(ctx, deps.MCP, a.TerminalID)
			if idFail != nil {
				return *idFail
			}
			a.TerminalID = resolvedID
			content, err := readTerminalTail(ctx, deps.MCP, a.TerminalID, 200)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, "Turn cancelled while reading terminal output.", tools.Unrecoverable())
				}
				return tools.Fail(codeTerminalOutput, fmt.Sprintf("Could not read output for terminal %s: %s", a.TerminalID, err.Error()))
			}
			tailLimit := summarizeDefaultTailBytes
			if a.TailBytes != nil {
				tailLimit = *a.TailBytes
			}
			tail := lastRunes(content, tailLimit)
			purpose := a.Purpose
			if purpose == "" {
				purpose = fmt.Sprintf("Summarize terminal %s for the supervisor.", a.TerminalID)
			}

			// The backend owns the summarizer prompt and any output cap; the CLI sends
			// the purpose + bounded tail, prefixed with a small provenance header (whose
			// terminal this is + chronological order — see summarizeHeader), and relays
			// the returned summary.
			summaryText, cerr := deps.Router.Summarize(ctx, purpose, summarizeHeader(ctx, deps.MCP, a.TerminalID)+tail)
			if cerr != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, "Turn cancelled while summarizing terminal.", tools.Unrecoverable())
				}
				return tools.Fail(codeSummarize, fmt.Sprintf("Failed to summarize terminal %s: %s", a.TerminalID, cerr.Error()))
			}
			body := strings.TrimSpace(summaryText)
			if body == "" {
				body = "(no summary produced)"
			}
			summary := body
			// Result carries ONLY what the model can't already know: the canonical id
			// (it may have called with a prefix) and the summary. The old purpose echo
			// and the hardcoded truncated=false (the CLI can no longer detect a
			// token-cap truncation — the backend owns the summarizer) were pure noise
			// repeated into the context on every call.
			return tools.Ok(summary, map[string]any{
				"terminalId": a.TerminalID, "summary": summary,
			})
		},
	}
}

/* ------------------------------- terminal.read ---------------------------- */

type readArgs struct {
	TerminalID string `json:"terminalId"`
	MaxLines   *int   `json:"maxLines,omitempty"`
	TailBytes  *int   `json:"tailBytes,omitempty"`
}

// Validate enforces the required terminalId + the maxLines/tailBytes bounds (Zod:
// maxLines int.positive.max(1000), tailBytes int.positive.max(100000)). Without
// this a missing terminalId reads nothing and a negative maxLines is forwarded
// straight into the MCP read.
func (a *readArgs) Validate() error {
	if strings.TrimSpace(a.TerminalID) == "" {
		return fmt.Errorf("terminalId is required")
	}
	if a.MaxLines != nil && (*a.MaxLines < 1 || *a.MaxLines > 1000) {
		return fmt.Errorf("maxLines must be between 1 and 1000")
	}
	if a.TailBytes != nil && (*a.TailBytes < 1 || *a.TailBytes > 100_000) {
		return fmt.Errorf("tailBytes must be between 1 and 100000")
	}
	return nil
}

var readSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Daintree terminal id to read." },
    "maxLines": { "type": "integer", "minimum": 1, "maximum": 1000, "default": 200, "description": "Max trailing lines of scrollback to return." },
    "tailBytes": { "type": "integer", "minimum": 1, "maximum": 100000, "description": "Further cap the returned text to the last N characters." }
  },
  "required": ["terminalId"]
}`)

func newReadTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.read",
		Description: "Read a terminal's raw scrollback tail VERBATIM — no model, no summarization, no token cap. Use this ONLY when you need " +
			"the exact literal text (the user asked for a precise quote, or you must inspect exact output). For the common 'tell me what the " +
			"agent said' case, prefer terminal.summarize: a coding agent's raw scrollback is garbled, repainted TUI output that bloats context " +
			"and reads as broken when pasted back. Request a bounded tail and never echo the whole frame to the user. Read-only; requires Daintree MCP.",
		Risk:   domain.RiskRead,
		Schema: readSchema,
		Decode: tools.StrictDecoder(func() any { return &readArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a readArgs
			_ = json.Unmarshal(raw, &a)
			maxLines := 200
			if a.MaxLines != nil {
				maxLines = *a.MaxLines
			}
			if !deps.MCP.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
			if ctx.Err() != nil {
				return tools.Fail(codeCancelled, "Turn cancelled while reading terminal output.", tools.Unrecoverable())
			}
			resolvedID, idFail := resolveTerminalID(ctx, deps.MCP, a.TerminalID)
			if idFail != nil {
				return *idFail
			}
			a.TerminalID = resolvedID
			content, err := readTerminalTail(ctx, deps.MCP, a.TerminalID, maxLines)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Fail(codeCancelled, "Turn cancelled while reading terminal output.", tools.Unrecoverable())
				}
				return tools.Fail(codeTerminalOutput, fmt.Sprintf("Could not read output for terminal %s: %s", a.TerminalID, err.Error()))
			}
			if a.TailBytes != nil {
				content = lastRunes(content, *a.TailBytes)
			}
			lineCount := 0
			if content != "" {
				lineCount = len(strings.Split(content, "\n"))
			}
			// Summary is a concise descriptor, NOT the scrollback itself. The raw
			// content lives in result.content (what the model reads when it wants the
			// verbatim text); echoing it into the summary too both doubled the model's
			// context and let the cockpit render a slice of garbled terminal bytes as
			// the activity-row detail. Keep the summary clean and bounded.
			summary := fmt.Sprintf("Read %d line(s) (%d chars) from terminal.", lineCount, len([]rune(content)))
			if content == "" {
				summary = "No output captured from terminal."
			}
			return tools.Ok(summary, map[string]any{
				"terminalId": a.TerminalID, "content": content, "lineCount": lineCount,
			})
		},
	}
}

// lastRunes returns the last n characters (runes) of s — the tail the model
// consumes.
func lastRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
