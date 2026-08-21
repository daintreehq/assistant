package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

/* ----------------- errors.recent / notifications.recent ------------------- */

// Daintree's shared paging bounds for both diagnostic stores (logActions.ts).
const (
	diagnosticsDefaultLimit = 20
	diagnosticsMaxLimit     = 50
)

// TWO STORES, ONE QUESTION. The diagnostics error log and the notification inbox are
// separate stores on Daintree's side, and "what went wrong?" is usually answered by
// BOTH — a failure can be recorded in one and not the other. Each description says so,
// because a model that reads one and stops gets a confidently incomplete answer, and
// nothing in either payload hints that the other exists.

/* ------------------------------ errors.recent ----------------------------- */

// errorsRecentArgs mirrors the host's args. IncludesDismissed is a pointer so an
// explicit `false` is distinguishable from omission: they mean the same thing today,
// but forwarding a value the caller did not send would pin a default that is Daintree's
// to choose.
type errorsRecentArgs struct {
	Limit             *int  `json:"limit,omitempty"`
	IncludesDismissed *bool `json:"includesDismissed,omitempty"`
}

func (a errorsRecentArgs) Validate() error {
	return validateDiagnosticsLimit("errors.recent", a.Limit)
}

// validateDiagnosticsLimit re-states the shared 1–50 bound both stores declare.
func validateDiagnosticsLimit(name string, limit *int) error {
	if limit != nil && (*limit < 1 || *limit > diagnosticsMaxLimit) {
		return fmt.Errorf("%s: limit is %d; Daintree accepts 1–%d (omit it for the default of %d)",
			name, *limit, diagnosticsMaxLimit, diagnosticsDefaultLimit)
	}
	return nil
}

var errorsRecentSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "limit": { "type": "integer", "minimum": 1, "maximum": 50, "default": 20, "description": "Max errors to return, newest first (default 20, max 50)." },
    "includesDismissed": { "type": "boolean", "default": false, "description": "Include errors the user dismissed (default false — active errors only)." }
  }
}`)

func newErrorsRecentTool() *tools.Tool {
	return &tools.Tool{
		Name: "errors.recent",
		Description: "List recent entries from Daintree's DIAGNOSTICS error log — runtime and inter-process failures, newest " +
			"first. SEPARATE store from the user's notification inbox (notifications.recent): a full picture of what went " +
			"wrong usually means reading both. An empty list means nothing was recorded OR all of it was dismissed. " +
			"PARALLEL: pair the two in ONE batch and both run concurrently.",
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         errorsRecentSchema,
		Decode:         tools.StrictDecoder(func() any { return &errorsRecentArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a errorsRecentArgs
			if res, ok := strictDecode(args, "errors.recent", &a); !ok {
				return res
			}
			fwd := map[string]any{}
			if a.Limit != nil {
				fwd["limit"] = *a.Limit
			}
			if a.IncludesDismissed != nil {
				fwd["includesDismissed"] = *a.IncludesDismissed
			}
			res := passthrough(ctx, tctx, "errors.recent", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("errors.recent")
			}
			errs, _ := obj["errors"].([]any)
			return tools.Ok(
				fmt.Sprintf("Read %d recent error(s) from the diagnostics log (the notification inbox is a separate store).", len(errs)),
				map[string]any{"errors": errs})
		},
	}
}

/* --------------------------- notifications.recent ------------------------- */

// notificationTypes is the closed set the host's enum accepts.
var notificationTypes = map[string]bool{
	"success": true, "error": true, "info": true, "warning": true,
}

type notificationsRecentArgs struct {
	Limit      *int   `json:"limit,omitempty"`
	Type       string `json:"type,omitempty"`
	UnreadOnly *bool  `json:"unreadOnly,omitempty"`
}

func (a notificationsRecentArgs) Validate() error {
	if err := validateDiagnosticsLimit("notifications.recent", a.Limit); err != nil {
		return err
	}
	if a.Type != "" && !notificationTypes[a.Type] {
		return fmt.Errorf("type %q is not one of success, error, info, warning", a.Type)
	}
	return nil
}

var notificationsRecentSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "limit": { "type": "integer", "minimum": 1, "maximum": 50, "default": 20, "description": "Max notifications to return, newest first (default 20, max 50)." },
    "type": { "type": "string", "enum": ["success", "error", "info", "warning"], "description": "Return only notifications of this type." },
    "unreadOnly": { "type": "boolean", "default": false, "description": "Only notifications the user has not yet seen as a toast (default false)." }
  }
}`)

func newNotificationsRecentTool() *tools.Tool {
	return &tools.Tool{
		Name: "notifications.recent",
		Description: "List recent entries from the user's NOTIFICATION INBOX — completion, waiting and informational messages, " +
			"including quiet ones that never surfaced as a toast — newest first. SEPARATE store from the diagnostics error " +
			"log (errors.recent): a full picture of what went wrong usually means reading both. PARALLEL: pair the two in " +
			"ONE batch.",
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         notificationsRecentSchema,
		Decode:         tools.StrictDecoder(func() any { return &notificationsRecentArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a notificationsRecentArgs
			if res, ok := strictDecode(args, "notifications.recent", &a); !ok {
				return res
			}
			fwd := map[string]any{}
			if a.Limit != nil {
				fwd["limit"] = *a.Limit
			}
			if a.Type != "" {
				fwd["type"] = a.Type
			}
			if a.UnreadOnly != nil {
				fwd["unreadOnly"] = *a.UnreadOnly
			}
			res := passthrough(ctx, tctx, "notifications.recent", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("notifications.recent")
			}
			notes, _ := obj["notifications"].([]any)
			return tools.Ok(
				fmt.Sprintf("Read %d recent notification(s) from the inbox (the diagnostics error log is a separate store).", len(notes)),
				map[string]any{"notifications": notes})
		},
	}
}
