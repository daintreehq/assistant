package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode"

	"github.com/daintreehq/assistant/internal/tools"
)

// MCP-family error codes (model-facing).
const (
	codeMCPUnavailable  = "MCP_UNAVAILABLE"
	codeMCPToolError    = "MCP_TOOL_ERROR"
	codeCancelled       = "CANCELLED"
	codeUseTypedWrapper = "USE_TYPED_WRAPPER"
)

// passthrough forwards a call to a named Daintree MCP tool. Shared by every typed
// wrapper and structurally identical to daintree.call — but each wrapper carries
// an accurate risk class. The args map is forwarded verbatim, so wrappers stay
// agnostic to Daintree's exact per-tool argument schema. A non-empty requestKey
// is merged in as the dedicated idempotency parameter.
func passthrough(ctx context.Context, mcp MCPClient, mcpName string, args map[string]any, requestKey string) tools.ToolResult {
	if mcp == nil || !mcp.Connected() {
		return tools.Fail(codeMCPUnavailable,
			fmt.Sprintf("Daintree MCP is not connected; cannot call %s. Use /reconnect to retry once Daintree is available.", mcpName))
	}
	callArgs := make(map[string]any, len(args)+1)
	for k, v := range args {
		callArgs[k] = v
	}
	if requestKey != "" {
		callArgs["requestKey"] = requestKey
	}
	res, err := mcp.CallTool(ctx, mcpName, callArgs)
	if err != nil {
		// A user abort surfaces as a timeout-shaped MCP error; report it as a clean
		// cancellation rather than a tool failure.
		if ctx.Err() != nil {
			return tools.Fail(codeCancelled, fmt.Sprintf("Turn cancelled during %s.", mcpName), tools.Unrecoverable())
		}
		return tools.Fail(codeMCPToolError, fmt.Sprintf("Daintree call %s failed: %s", mcpName, err.Error()))
	}
	if res.IsError {
		// Carry Daintree's own refusal text into the failure summary so a denied
		// grant-authorized mutation surfaces *why* it was refused.
		msg := fmt.Sprintf("Daintree tool %s returned an error.", mcpName)
		if res.Text != "" {
			msg = fmt.Sprintf("Daintree refused %s: %s", mcpName, res.Text)
		}
		return tools.Fail(codeMCPToolError, msg,
			tools.WithDetails(map[string]any{"structuredContent": res.StructuredContent, "rawText": res.Text}))
	}
	return tools.Ok(fmt.Sprintf("Called %s.", mcpName),
		map[string]any{"text": res.Text, "structuredContent": res.StructuredContent})
}

// extractArmedSet pulls the `armed: string[]` set from an arming tool's result,
// reading structuredContent first then falling back to a JSON-encoded text body.
// Returns (set, true) only when a source carries a well-formed string array — so a
// legitimately empty set (after disarmAll) is preserved as [] while a
// missing/garbled set returns (nil, false) and can fail loudly.
func extractArmedSet(res map[string]any) ([]string, bool) {
	fromObj := func(o any) ([]string, bool) {
		m, ok := o.(map[string]any)
		if !ok {
			return nil, false
		}
		arr, ok := m["armed"].([]any)
		if !ok {
			return nil, false
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	if res == nil {
		return nil, false
	}
	if set, ok := fromObj(res["structuredContent"]); ok {
		return set, true
	}
	if text, ok := res["text"].(string); ok && text != "" {
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err == nil {
			if set, ok := fromObj(parsed); ok {
				return set, true
			}
		}
	}
	return nil, false
}

// terminalArmingPassthrough runs the shared passthrough then replaces its generic
// summary with the concrete armed-terminal list Daintree returns as
// {armed:string[]}. Arming must NEVER silently reroute the human's keystrokes
// (#136): if neither result source carries the set we FAIL rather than hide which
// terminals are now armed — an unknown arming state is the one thing these tools
// may not do quietly.
func terminalArmingPassthrough(ctx context.Context, mcp MCPClient, mcpName string, args map[string]any, action string) tools.ToolResult {
	res := passthrough(ctx, mcp, mcpName, args, "")
	if !res.Ok {
		return res
	}
	result, _ := res.Result.(map[string]any)
	armed, ok := extractArmedSet(result)
	if !ok {
		var sc, rawText any
		if result != nil {
			sc = result["structuredContent"]
			rawText = result["text"]
		}
		return tools.Fail(codeMCPToolError,
			fmt.Sprintf("%s did not report the resulting armed set, so the current arming state is unknown — re-check with terminal.getStatus before relying on it.", mcpName),
			tools.WithDetails(map[string]any{"structuredContent": sc, "rawText": rawText}))
	}
	list := "none"
	if len(armed) > 0 {
		list = join(armed, ", ")
	}
	return tools.Ok(fmt.Sprintf("%s Armed terminals now: %s.", action, list), result)
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// truncateCommand renders a command for a single-line, human-facing summary:
// newlines/tabs collapse to a space (so a heredoc or multi-line command can't
// break the inline render at render_operations.go) and the result is clipped to
// max runes with a "..." marker. Rune-aware so a multibyte command is never cut
// mid-codepoint.
func truncateCommand(s string, max int) string {
	// Collapse any whitespace run (incl. \r\n, tabs, and exotic Unicode line/paragraph
	// separators) to a single space and trim the ends — keeps the summary one line and
	// free of leading indentation. prevSpace starts true so a leading whitespace run is
	// dropped entirely.
	var b []rune
	prevSpace := true
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b = append(b, ' ')
				prevSpace = true
			}
			continue
		}
		b = append(b, r)
		prevSpace = false
	}
	// Trim a single trailing space left by the collapse.
	if len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// terminalSendCommandPassthrough runs the shared passthrough then replaces its
// generic "Called terminal.sendCommand." summary with a concrete, self-describing
// one: "Sent to terminal <id>: <command>." The assistant already knows the
// terminalId + command from the validated args, so the human-facing record (audit
// Summary + live transcript) is specific and correlated even though Daintree's
// response carries no acceptance/echo field to verify against. The original result
// payload (text + structuredContent) is preserved verbatim.
func terminalSendCommandPassthrough(ctx context.Context, mcp MCPClient, terminalID, command string, args map[string]any) tools.ToolResult {
	res := passthrough(ctx, mcp, "terminal.sendCommand", args, "")
	if !res.Ok {
		return res
	}
	result, _ := res.Result.(map[string]any)
	return tools.Ok(fmt.Sprintf("Sent to terminal %s: %s.", terminalID, truncateCommand(command, 80)), result)
}

// terminalClosePassthrough closes a batch of terminals through the Daintree
// terminal.close MCP tool (which takes ONE terminalId per call), looping so the
// model can retire a whole spawned cohort in a SINGLE confirmed wrapper call
// instead of N system-tier daintree.call confirmations. It reports faithfully (the
// "report tool outcomes faithfully" rule): the summary names every id that closed
// and every id that did not, and the result is a FAILURE if ANY close failed — a
// partial outcome is never narrated as a clean success. A connection/cancellation
// failure aborts the rest of the batch, since every remaining call would fail the
// same way and hammering a dead link is pointless.
func terminalClosePassthrough(ctx context.Context, mcp MCPClient, ids []string) tools.ToolResult {
	var closed, failed, notAttempted []string
	var abort tools.ToolResult // the fatal result (dead link / cancel) that stopped the batch
	aborted := false
	for i, id := range ids {
		res := passthrough(ctx, mcp, "terminal.close", map[string]any{"terminalId": id}, "")
		if res.Ok {
			closed = append(closed, id)
			continue
		}
		failed = append(failed, id)
		if res.Error != nil && (res.Error.Code == codeMCPUnavailable || res.Error.Code == codeCancelled) {
			// The link is gone or the turn was cancelled — every REMAINING id would fail
			// identically. Stop, record the rest as not-attempted (so none silently
			// vanishes from the report), and keep the abort's own code/recoverability
			// (CANCELLED is unrecoverable; MCP_UNAVAILABLE carries the /reconnect hint)
			// rather than flattening it to MCP_TOOL_ERROR below.
			abort = res
			aborted = true
			notAttempted = append(notAttempted, ids[i+1:]...)
			break
		}
	}
	if len(failed) == 0 {
		return tools.Ok(fmt.Sprintf("Closed %d terminal(s): %s.", len(closed), join(closed, ", ")),
			map[string]any{"closed": closed})
	}
	// Name EVERY unclosed id (errored + not-attempted) so a partial outcome is never
	// narrated as a clean success.
	unclosed := append(append([]string{}, failed...), notAttempted...)
	details := map[string]any{"closed": closed, "failed": failed}
	if len(notAttempted) > 0 {
		details["notAttempted"] = notAttempted
	}
	if aborted {
		msg := fmt.Sprintf("Closed %d of %d terminal(s) before the batch aborted; did not close: %s. %s",
			len(closed), len(ids), join(unclosed, ", "), abort.Error.Message)
		if !abort.Error.Recoverable {
			return tools.Fail(abort.Error.Code, msg, tools.WithDetails(details), tools.Unrecoverable())
		}
		return tools.Fail(abort.Error.Code, msg, tools.WithDetails(details))
	}
	msg := fmt.Sprintf("Closed %d of %d terminal(s); failed to close: %s.", len(closed), len(ids), join(unclosed, ", "))
	return tools.Fail(codeMCPToolError, msg, tools.WithDetails(details))
}

// copyTreeInjectPassthrough mirrors terminalSendCommandPassthrough for
// copyTree.injectToTerminal: the injected payload is a large, unnamed copy-tree
// digest, so the summary names the destination terminal rather than echoing
// content — "Injected copy tree into terminal <id>." replacing the generic
// "Called copyTree.injectToTerminal."
func copyTreeInjectPassthrough(ctx context.Context, mcp MCPClient, terminalID string, args map[string]any) tools.ToolResult {
	res := passthrough(ctx, mcp, "copyTree.injectToTerminal", args, "")
	if !res.Ok {
		return res
	}
	result, _ := res.Result.(map[string]any)
	return tools.Ok(fmt.Sprintf("Injected copy tree into terminal %s.", terminalID), result)
}

// moveFollowUp renders the clause that has to ride EVERY result naming a terminal
// that actually moved — success and partial failure alike. Filing a pane under a new
// worktree does not restart or notify the process in it, so a moved agent keeps
// working in its old directory until someone sends it this sentence. A partial batch
// is exactly where that gets dropped: the summary is about the failure, the model
// reads "failed", and the terminals that DID move are silently left half-relocated.
func moveFollowUp(moved []string, worktreeID string) string {
	if len(moved) == 0 {
		return ""
	}
	return fmt.Sprintf(
		" The process was NOT restarted — send each live agent among %s \"Please continue in the directory %s\"; that sentence, not this move, relocates the work.",
		join(moved, ", "), worktreeID)
}

// movedOrNone renders the moved set for a failure summary, which must name the ids
// that DID move — Details is not written to the audit row for a failed call, so the
// summary is the only durable record of a partial outcome.
func movedOrNone(moved []string) string {
	if len(moved) == 0 {
		return "none moved"
	}
	return "moved: " + join(moved, ", ")
}

// terminalMoveToWorktreePassthrough files a batch of terminals into ONE open
// worktree through the Daintree terminal.moveToWorktree MCP tool (which takes ONE
// terminalId per call), looping so the model can relocate a whole spawned cohort in
// a SINGLE confirmed wrapper call instead of N system-tier daintree.call
// confirmations — the cohort case is what motivated wrapping the action at all.
//
// Reporting mirrors terminalClosePassthrough deliberately: the summary names every
// id that moved and every id that did not, and the result is a FAILURE if ANY move
// failed, so a partial outcome is never narrated as a clean success. A
// connection/cancellation failure aborts the rest of the batch (every remaining call
// would fail identically) and keeps the abort's own code and recoverability.
//
// Each per-id refusal message is preserved verbatim in the details, because
// Daintree's two failure modes need OPPOSITE recoveries — an unknown terminalId
// means re-read the roster, an unknown worktreeId means the destination path itself
// is wrong and every id in the batch will fail the same way. We deliberately do NOT
// sniff the message text to classify the second case as globally fatal: matching on
// Daintree's prose would break silently the day it is reworded, and the faithful
// per-id report already tells the model both ids and the shared destination.
//
// The raw action returns void on success, so there is no structured payload to
// extract — the report is built entirely from the ids we attempted.
func terminalMoveToWorktreePassthrough(ctx context.Context, mcp MCPClient, ids []string, worktreeID string) tools.ToolResult {
	var moved, failed, notAttempted []string
	details := map[string]any{"worktreeId": worktreeID}
	refusals := map[string]string{}
	var abort tools.ToolResult // the fatal result (dead link / cancel) that stopped the batch
	aborted := false
	for i, id := range ids {
		// Re-check between moves rather than trusting the transport to reject a
		// cancelled context: each iteration is a separate confirmed mutation, and an
		// abandoned turn should stop at the last one it completed.
		if ctx.Err() != nil {
			failed = append(failed, id)
			abort = tools.Fail(codeCancelled, fmt.Sprintf("Turn cancelled during %s.", "terminal.moveToWorktree"), tools.Unrecoverable())
			refusals[id] = abort.Error.Message
			aborted = true
			notAttempted = append(notAttempted, ids[i+1:]...)
			break
		}
		res := passthrough(ctx, mcp, "terminal.moveToWorktree",
			map[string]any{"terminalId": id, "worktreeId": worktreeID}, "")
		if res.Ok {
			moved = append(moved, id)
			continue
		}
		failed = append(failed, id)
		if res.Error != nil {
			refusals[id] = res.Error.Message
			if res.Error.Code == codeMCPUnavailable || res.Error.Code == codeCancelled {
				// The link is gone or the turn was cancelled — every REMAINING id would
				// fail identically. Stop, record the rest as not-attempted (so none
				// silently vanishes from the report), and keep the abort's own
				// code/recoverability rather than flattening it to MCP_TOOL_ERROR below.
				abort = res
				aborted = true
				notAttempted = append(notAttempted, ids[i+1:]...)
				break
			}
		}
	}
	// Normalize to an empty slice so the payload serializes as [] rather than null —
	// the model reads this back, and `null` invites "the field is missing" rather than
	// "nothing moved".
	if moved == nil {
		moved = []string{}
	}
	details["moved"] = moved
	if len(failed) == 0 {
		return tools.Ok(fmt.Sprintf("Moved %d terminal(s) into worktree %s: %s.%s",
			len(moved), worktreeID, join(moved, ", "), moveFollowUp(moved, worktreeID)), details)
	}
	// Name EVERY terminal that did not move (errored + not-attempted) so a partial
	// outcome is never narrated as a clean success.
	unmoved := append(append([]string{}, failed...), notAttempted...)
	details["failed"] = failed
	details["refusals"] = refusals
	if len(notAttempted) > 0 {
		details["notAttempted"] = notAttempted
	}
	// A failed call is not proof the move did not happen — passthrough maps a Go-level
	// transport error and a Daintree refusal to the SAME code, so a response lost after
	// Daintree applied the move lands here as "failed". Say so, rather than let the
	// model treat the unmoved list as settled fact and retry blind.
	uncertain := " A failed move's outcome is not certain (a lost response looks the same as a refusal) — re-read the terminal roster before retrying."
	if aborted {
		msg := fmt.Sprintf("Moved %d of %d terminal(s) into worktree %s before the batch aborted; did not move: %s. %s%s%s",
			len(moved), len(ids), worktreeID, join(unmoved, ", "), abort.Error.Message,
			uncertain, moveFollowUp(moved, worktreeID))
		if !abort.Error.Recoverable {
			return tools.Fail(abort.Error.Code, msg, tools.WithDetails(details), tools.Unrecoverable())
		}
		return tools.Fail(abort.Error.Code, msg, tools.WithDetails(details))
	}
	msg := fmt.Sprintf("Moved %d of %d terminal(s) into worktree %s (%s); failed to move: %s.%s%s",
		len(moved), len(ids), worktreeID, movedOrNone(moved), join(unmoved, ", "),
		uncertain, moveFollowUp(moved, worktreeID))
	return tools.Fail(codeMCPToolError, msg, tools.WithDetails(details))
}
