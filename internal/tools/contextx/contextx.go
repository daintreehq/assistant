// Package contextx is the read-only orchestration-helper family: compact
// main-thread snapshots (context.snapshot) and cheap terminal reads/summaries
// (terminal.read VERBATIM, terminal.summarize via the small model). All risk
// "read". These keep the main model's context clean by collapsing Daintree state
// and raw scrollback into terse digests instead of dumping everything inline.
// context.snapshot is best-effort and must NEVER throw even when Daintree MCP is
// down; terminal.summarize/read fail cleanly when MCP is unavailable.
package contextx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/tools/terminalid"
)

// MCPStatus mirrors the Daintree MCP connection status (mcp.status()).
type MCPStatus struct {
	Connected bool   `json:"connected"`
	Transport string `json:"transport,omitempty"`
	ToolCount *int   `json:"toolCount,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MCPCallResult is the Daintree MCP call envelope.
type MCPCallResult struct {
	Text              string `json:"text"`
	StructuredContent any    `json:"structuredContent,omitempty"`
	IsError           bool   `json:"isError"`
}

// MCPClient is the slice of the Daintree MCP transport this family reaches.
type MCPClient interface {
	Connected() bool
	Status() MCPStatus
	CallTool(ctx context.Context, name string, args map[string]any) (MCPCallResult, error)
}

// Router is the model access this family uses. Summarize maps to the backend's
// terminal_summarize.v1 task (the backend owns the prompt); the CLI sends only the
// purpose + terminal tail.
type Router interface {
	Summarize(ctx context.Context, purpose, tail string) (string, error)
}

// Queue is the slice of the attention queue context.snapshot reads (the open
// inbox). Digest returns the events; Format renders them to a human digest.
type Queue interface {
	Digest(opts domain.QueueDigestOptions) []domain.QueueEvent
	Format(events []domain.QueueEvent) string
}

// Deps wires the context family.
type Deps struct {
	MCP    MCPClient
	Router Router
	Queue  Queue
}

const (
	codeMCPUnavailable   = "MCP_UNAVAILABLE"
	codeCancelled        = "CANCELLED"
	codeTerminalOutput   = "TERMINAL_OUTPUT"
	codeSummarize        = "SUMMARIZE"
	codeTerminalNotFound = "TERMINAL_NOT_FOUND"
)

// Tools returns the context family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newSnapshotTool(deps),
		newSummarizeTool(deps),
		newReadTool(deps),
	}
}

/* ------------------------------- helpers --------------------------------- */

// tryCall is a best-effort MCP read: returns (result, true) on a clean call, or
// (_, false) when disconnected / errored / aborted. context.snapshot must never
// throw, so every failure degrades this read to "unavailable".
func tryCall(ctx context.Context, mcp MCPClient, name string, args map[string]any) (MCPCallResult, bool) {
	if mcp == nil || !mcp.Connected() {
		return MCPCallResult{}, false
	}
	res, err := mcp.CallTool(ctx, name, args)
	if err != nil || res.IsError {
		return MCPCallResult{}, false
	}
	return res, true
}

// readTerminalTail reads a terminal's scrollback tail via terminal.getOutput (no
// model). Scrollback may arrive in structuredContent.content OR the raw text body
// (Daintree uses the latter) — read both, falling back to raw text.
//
// A "terminal not found" comes back as a NON-error response (IsError=false) whose body is
// a JSON envelope with content:null + a top-level "error" — so it must be detected and
// surfaced as an error, NOT handed back as if it were scrollback. Returning the error JSON
// as content is exactly the ses_f3fdeb08 bug: a truncated id read as a fake "Read 7 lines"
// success, giving the model no signal its id was wrong.
func readTerminalTail(ctx context.Context, mcp MCPClient, terminalID string, maxLines int) (string, error) {
	out, err := mcp.CallTool(ctx, "terminal.getOutput", map[string]any{"terminalId": terminalID, "maxLines": maxLines})
	if err != nil {
		return "", err
	}
	if out.IsError {
		msg := out.Text
		if msg == "" {
			msg = "terminal returned an error"
		}
		return "", fmt.Errorf("%s", msg)
	}
	if sc, ok := out.StructuredContent.(map[string]any); ok {
		// Check for an error sentinel BEFORE returning content: a blank content string ("")
		// alongside an error must surface as an error, not as a fake "no output" success.
		if msg := outputBodyError(sc["content"], sc["error"]); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		if content, ok := sc["content"].(string); ok {
			return content, nil
		}
	}
	if msg := notFoundFromText(out.Text); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	return out.Text, nil
}

// notFoundFromText returns Daintree's error message when text is a terminal.getOutput JSON
// envelope reporting a not-found / errored terminal (a non-empty top-level "error" with
// null/blank content), or "" when text is ordinary scrollback. Daintree returns the
// payload as JSON in the text body, so a not-found arrives IsError=false with an embedded
// error string we must not mistake for output.
//
// terminal.read promises VERBATIM scrollback, so the detection is deliberately narrow: it
// fires only when the body carries the structural markers of the getOutput envelope
// (a "terminalId" plus a "lineCount" or "truncated" field). Arbitrary scrollback that
// merely happens to be a JSON object with an "error" key — e.g. an agent that printed
// `{"error":"boom","content":null}` — lacks those markers and is returned verbatim.
func notFoundFromText(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	var env struct {
		TerminalID *string         `json:"terminalId"`
		Content    *string         `json:"content"`
		LineCount  json.RawMessage `json:"lineCount"`
		Truncated  json.RawMessage `json:"truncated"`
		Error      string          `json:"error"`
	}
	if json.Unmarshal([]byte(text), &env) != nil {
		return "" // not a JSON object → real scrollback
	}
	// Require the getOutput envelope shape so arbitrary JSON scrollback is never misread.
	if env.TerminalID == nil || (len(env.LineCount) == 0 && len(env.Truncated) == 0) {
		return ""
	}
	var content any
	if env.Content != nil {
		content = *env.Content
	}
	return outputBodyError(content, env.Error)
}

// outputBodyError returns the trimmed error string when a getOutput body reports an error
// with no usable content (content null/blank), else "". Shared by the structuredContent
// and text-body paths so both classify a not-found identically.
func outputBodyError(content, errField any) string {
	msg, _ := errField.(string)
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if s, ok := content.(string); ok && strings.TrimSpace(s) != "" {
		return "" // real content alongside a note — keep the content, not the error
	}
	return msg
}

// resolveTerminalID canonicalizes one caller-supplied terminal id against the live roster
// (terminal.list) so a truncated/prefix id — the model abbreviates Daintree's full
// terminal-<uuid> ids — still resolves, and an unknown id fails fast with the live list.
// FAILS OPEN (returns the id unchanged) when the roster is unreadable or empty, so a
// discovery hiccup never blocks a read. Returns (canonicalID, nil) or ("", *Fail).
//
// terminal.read/summarize are on a hot path, so a FULL canonical id skips the roster read
// entirely — only a truncated/odd id pays for terminal.list. A stale-but-full id is caught
// downstream by readTerminalTail's not-found detection, so skipping is safe. (The cohort
// path in extractionx deliberately does NOT skip — there, always-resolving also fails fast
// on a stale full id and avoids the await loop's absent-guard reporting it as "finished".)
func resolveTerminalID(ctx context.Context, mcp MCPClient, terminalID string) (string, *tools.ToolResult) {
	if terminalid.LooksCanonical(terminalID) {
		return terminalID, nil
	}
	res, ok := tryCall(ctx, mcp, "terminal.list", map[string]any{})
	if !ok {
		return terminalID, nil // fail open
	}
	live := terminalid.ParseListIDs(res.StructuredContent, res.Text)
	if len(live) == 0 {
		return terminalID, nil // fail open: empty/unreadable roster is also the hiccup symptom
	}
	r := terminalid.Resolve([]string{terminalID}, live)
	if r.OK() {
		return r.Resolved[0], nil
	}
	what := "matches no live terminal"
	if len(r.Ambiguous) > 0 {
		what = "is an ambiguous prefix matching several terminals"
	}
	fail := tools.Fail(codeTerminalNotFound, fmt.Sprintf(
		"terminal %q %s. Use the EXACT, FULL terminal id (e.g. terminal-5284bfef-3d11-424c-90cb-136f24046295) — never an abbreviated prefix. Live terminals: %s.",
		terminalID, what, strings.Join(live, ", ")),
		tools.WithDetails(map[string]any{"liveTerminals": live}))
	return "", &fail
}

func contentOrNil(res MCPCallResult, ok bool) any {
	if !ok {
		return nil
	}
	return map[string]any{"text": res.Text, "structuredContent": res.StructuredContent}
}
