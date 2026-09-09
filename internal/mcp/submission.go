package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
)

func resultObject(structured any, text string) map[string]any {
	if body, ok := structured.(map[string]any); ok && body != nil {
		return body
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(text), &body)
	return body
}

// SubmissionReceipt reads a successful send response without equating an old
// host's missing receipt with a correlated lookup's unknown phase.
func SubmissionReceipt(structured any, text, terminalID string) (domain.SubmissionReceipt, error) {
	body := resultObject(structured, text)
	raw, present := body["submissionToken"]
	if !present {
		if body["sent"] == true && body["terminalId"] == terminalID {
			return domain.SubmissionReceipt{Version: 1, Acceptance: "legacy_unknown"}, nil
		}
		return domain.SubmissionReceipt{}, fmt.Errorf("Daintree's send acknowledgement was unreadable; input may already be queued, so inspect the terminal before sending again")
	}
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
