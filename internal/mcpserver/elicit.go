package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// elicit.go PUSHES an approval to the client instead of waiting to be polled, when the
// client supports MCP elicitation.
//
// It is strictly an accelerant, never the mechanism. Elicitation support varies by
// client, a client may answer slowly or not at all, and a server that depended on it
// would have made approvals unusable for everyone else. So every failure — unsupported,
// errored, declined, cancelled, timed out — simply leaves the approval parked for
// daintree.approvals/daintree.approve to handle, exactly as if this file did not exist.

// approvalElicitSchema is the form the client renders: one boolean. Deliberately
// minimal — MCP allows only top-level properties, and anything richer would be a
// decision the tool surface already carries in risk/consequence/args.
var approvalElicitSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"approve": {
			Type:        "boolean",
			Description: "Allow this tool call to run.",
		},
	},
	Required: []string{"approve"},
}

// elicitNotifier returns an Approvals notify hook that asks ss to decide.
//
// parent is the server lifetime; each ask is additionally bounded by the approval's own
// timeout, so a client that accepts the request and never answers costs one goroutine
// for at most that long rather than for the session's life.
func elicitNotifier(ss *mcp.ServerSession, approvals *Approvals, timeout time.Duration) func(PendingApproval) {
	if ss == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultApprovalTimeout
	}
	return func(pa PendingApproval) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		res, err := ss.Elicit(ctx, &mcp.ElicitParams{
			Message:         elicitMessage(pa),
			RequestedSchema: approvalElicitSchema,
		})
		if err != nil {
			// Unsupported, disconnected, or refused at the transport. The approval stays
			// parked; polling is the path.
			return
		}
		switch res.Action {
		case "accept":
			approved, _ := res.Content["approve"].(bool)
			d := DecisionRejected
			if approved {
				d = DecisionApproved
			}
			approvals.Resolve(pa.ID, d)
		case "decline":
			approvals.Resolve(pa.ID, DecisionRejected)
		default:
			// "cancel" is the viewer dismissing the prompt without choosing. That is NOT
			// a decision — resolving it either way would put words in their mouth — so
			// leave it parked for the explicit tools, and let the timer have the last
			// word if nobody follows up.
		}
	}
}

// elicitMessage is what the client shows. It leads with the consequence, because that is
// what a decision is actually made on; the tool name alone is not enough to judge.
func elicitMessage(pa PendingApproval) string {
	msg := fmt.Sprintf("The Daintree assistant wants to run %s (%s risk).", pa.Tool, pa.Risk)
	if pa.Consequence != "" {
		msg += "\n\nWhat it will do: " + pa.Consequence
	}
	if pa.Args != "" {
		msg += "\n\nArguments: " + pa.Args
	}
	return msg
}
