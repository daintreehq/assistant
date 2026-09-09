package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
)

// resultObject reads ONE MCP result body. Daintree's terminal tools have always
// carried their payload in the TEXT content block, with structuredContent either
// absent or an empty object — so a structured map is never trusted to be
// complete. This mirrors internal/daemon/mcpreads.go (parseMcpArray /
// parseMcpString): read structuredContent FIRST, then fill in every key it did
// not supply from the JSON text body. Returning the bare structured map — as
// this once did — silently lost the receipt, the submission record and hasPty on
// every host that answers structuredContent:{}. Never throws; an unparseable
// text body simply contributes nothing.
func resultObject(structured any, text string) map[string]any {
	body, _ := structured.(map[string]any)
	var fromText map[string]any
	if strings.TrimSpace(text) != "" {
		_ = json.Unmarshal([]byte(text), &fromText)
	}
	if len(fromText) == 0 {
		return body
	}
	if len(body) == 0 {
		return fromText
	}
	// Structured wins a key collision (parseMcpString's precedence); the text
	// body only supplies what structuredContent left out.
	merged := make(map[string]any, len(body)+len(fromText))
	for k, v := range fromText {
		merged[k] = v
	}
	for k, v := range body {
		merged[k] = v
	}
	return merged
}

// SubmissionReceipt reads a successful send response without equating an old
// host's missing receipt with a correlated lookup's unknown phase.
func SubmissionReceipt(structured any, text, terminalID string) (domain.SubmissionReceipt, error) {
	body := resultObject(structured, text)
	raw, present := body["submissionToken"]
	if !present {
		// No token at all is the ONLY shape any Daintree in the field produces
		// today (and the shape the scripted world and the e2e fake return):
		// a bare {"ok":true} with neither `sent` nor a terminal echo. The send
		// has already happened by the time we parse this, so refusing it would
		// fail EVERY terminal.run.async against an un-upgraded host — after the
		// command really went out, and worded as a transport error. A receipt
		// that was never offered is exactly what "legacy_unknown" means.
		//
		// The one thing still worth refusing is an ack that names a DIFFERENT
		// terminal: that is a mis-correlated response, not an old host.
		if echoed, ok := body["terminalId"].(string); ok && echoed != terminalID {
			return domain.SubmissionReceipt{}, fmt.Errorf("Daintree acknowledged a different terminal than the one addressed; input may already be queued, so inspect the terminal before sending again")
		}
		return domain.SubmissionReceipt{Version: 1, Acceptance: "legacy_unknown"}, nil
	}
	// A token that IS present must be well-formed and correlated — a host that
	// ships receipts is held to the contract it advertises.
	token, ok := raw.(string)
	if !ok || token == "" || len(token) > 128 || body["terminalId"] != terminalID || body["sent"] != true {
		return domain.SubmissionReceipt{}, fmt.Errorf("Daintree returned an invalid submission receipt; input may already be queued, so inspect the terminal before sending again")
	}
	return domain.SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: token, Phase: "queued"}, nil
}

// ReadTerminalSubmission accepts only the exact terminal/token pair requested.
// Missing, malformed or unreadable records are not synthesized as unknown.
func ReadTerminalSubmission(structured any, text, terminalID, token string) (domain.TerminalSubmission, map[string]any, bool) {
	body := resultObject(structured, text)
	rows, _ := body["terminals"].([]any)
	for _, row := range rows {
		entry, ok := row.(map[string]any)
		if !ok || entry["terminalId"] != terminalID {
			continue
		}
		record, ok := entry["submission"].(map[string]any)
		if !ok || record["token"] != token {
			return domain.TerminalSubmission{}, entry, false
		}
		phase, _ := record["phase"].(string)
		if !domain.ValidSubmissionPhase(phase) {
			return domain.TerminalSubmission{}, entry, false
		}
		return domain.TerminalSubmission{Token: token, Phase: phase}, entry, true
	}
	return domain.TerminalSubmission{}, nil, false
}

// TerminalHasPty preserves field availability per response, including an
// explicit unavailable field even if a contradictory entry supplies a value.
func TerminalHasPty(structured any, text string, entry map[string]any) *bool {
	body := resultObject(structured, text)
	fields, _ := body["unavailableFields"].([]any)
	for _, f := range fields {
		if f == "hasPty" {
			return nil
		}
	}
	value, ok := entry["hasPty"].(bool)
	if !ok {
		return nil
	}
	return &value
}

// ToolRefusalPermanent preserves the host's explicit non-retriable verdict.
// Missing/malformed retry metadata does not manufacture a permanent refusal.
func ToolRefusalPermanent(structured any, text string) bool {
	body := resultObject(structured, text)
	retry, ok := body["retriable"].(bool)
	return ok && !retry
}
