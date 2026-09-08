package mcp

import "encoding/json"

// TerminalStatusReadUnavailable recognizes the reduced PTY response's ambiguous
// lookup failures. Unlike a renderer's missing panel, these rows can mean an RPC
// failed while the terminal is still alive. Fail the whole polling batch so waits
// and async deadlines use their existing read-outage path, never a false exit.
// Healthy siblings are sampled again on the next successful batch.
func TerminalStatusReadUnavailable(structured any, text string) bool {
	unavailable := func(body map[string]any) bool {
		if body["source"] != "pty" {
			return false
		}
		entries, _ := body["terminals"].([]any)
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			errText, _ := entry["error"].(string)
			state, _ := entry["agentState"].(string)
			if errText != "" && state == "" {
				return true
			}
		}
		return false
	}
	structuredBody, _ := structured.(map[string]any)
	if unavailable(structuredBody) {
		return true
	}
	var body map[string]any
	return json.Unmarshal([]byte(text), &body) == nil && unavailable(body)
}
