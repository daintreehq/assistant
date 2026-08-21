package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

/* ---------------------- browser.getConsoleMessages ------------------------ */

// browserConsoleLevels is the closed set the host's enum accepts. Restated so a wrong
// level fails here, naming the four legal values, instead of arriving as an opaque
// renderer validation error a round later.
var browserConsoleLevels = map[string]bool{
	"log": true, "info": true, "warning": true, "error": true,
}

// browserGetConsoleMessagesArgs mirrors getConsoleMessagesArgsSchema. Limit is a pointer
// because the host treats OMITTED as "all captured" — a defaulted number here would
// silently cap a read the caller asked to be complete.
// TerminalID is a pointer with NO minLength in the schema, mirroring the host: it
// declares a plain optional string, and its `terminalId ?? focusedId` fallback keeps an
// explicit "" (only null/undefined fall through), which then fails the dev-preview check
// with a clear message. Dropping an explicit "" here would instead retarget the read at
// the FOCUSED panel — a different panel than the one that was named.
type browserGetConsoleMessagesArgs struct {
	TerminalID *string `json:"terminalId,omitempty"`
	Level      *string `json:"level,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
}

func (a browserGetConsoleMessagesArgs) Validate() error {
	// An explicit "" is an enum violation on the host, so it is rejected here rather
	// than dropped — dropping it would silently widen the read to every level.
	if a.Level != nil && !browserConsoleLevels[*a.Level] {
		return fmt.Errorf("level %q is not one of log, info, warning, error", *a.Level)
	}
	if a.Limit != nil && (*a.Limit < 1 || *a.Limit > 500) {
		return fmt.Errorf("limit is %d; Daintree accepts 1–500 (omit it for every captured message)", *a.Limit)
	}
	return nil
}

var browserGetConsoleMessagesSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "The dev preview panel to read. Omit for the focused panel." },
    "level": { "type": "string", "enum": ["log", "info", "warning", "error"], "description": "Return only messages at this level." },
    "limit": { "type": "integer", "minimum": 1, "maximum": 500, "description": "Max messages, newest kept. Omit for every captured message." }
  }
}`)

// newBrowserGetConsoleMessagesTool reads a dev preview panel's captured console output.
//
// The constraint worth stating up front is the TARGET: only dev preview panels capture
// console output, and pointing this at an ordinary terminal fails rather than returning
// nothing. That distinction is invisible in a terminal roster, so a model that treats
// every terminalId as a candidate will hit an error it cannot diagnose from the payload.
//
// `counts` covers everything captured for the pane, not the filtered page — so an
// errorCount above the number of returned rows means the filter or limit hid some, not
// that rows went missing.
func newBrowserGetConsoleMessagesTool() *tools.Tool {
	return &tools.Tool{
		Name: "browser.getConsoleMessages",
		Description: "Read captured console output (logs, warnings, errors, stack traces) from a DEV PREVIEW panel — the only " +
			"panel kind that captures it; any other target fails rather than returning nothing. Omit terminalId for the " +
			"focused panel. `counts` covers the whole pane, not the rows returned, so a higher errorCount means the level " +
			"filter or limit hid some.",
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         browserGetConsoleMessagesSchema,
		Decode:         tools.StrictDecoder(func() any { return &browserGetConsoleMessagesArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a browserGetConsoleMessagesArgs
			if res, ok := strictDecode(args, "browser.getConsoleMessages", &a); !ok {
				return res
			}
			fwd := map[string]any{}
			if a.TerminalID != nil {
				fwd["terminalId"] = *a.TerminalID
			}
			if a.Level != nil {
				fwd["level"] = *a.Level
			}
			if a.Limit != nil {
				fwd["limit"] = *a.Limit
			}
			res := passthrough(ctx, tctx, "browser.getConsoleMessages", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("browser.getConsoleMessages")
			}
			// Presence-checked: a missing `messages` reported as "Read 0 console
			// message(s)" would read as "the page logged nothing", which is the opposite
			// of "we could not tell".
			messages, ok2 := obj["messages"].([]any)
			if !ok2 {
				return failMalformed("browser.getConsoleMessages")
			}
			out := map[string]any{"messages": messages}
			for _, k := range []string{"paneId", "counts"} {
				if v, present := obj[k]; present {
					out[k] = v
				}
			}
			summary := fmt.Sprintf("Read %d console message(s)", len(messages))
			if counts, ok := obj["counts"].(map[string]any); ok {
				errs, haveErrs := numberFrom(counts["errorCount"])
				warns, haveWarns := numberFrom(counts["warnCount"])
				if haveErrs || haveWarns {
					summary += fmt.Sprintf(" (pane totals: %d error(s), %d warning(s))", errs, warns)
				}
			}
			return tools.Ok(summary+".", out)
		},
	}
}
