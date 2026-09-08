package mcp

import (
	"encoding/json"
	"testing"
)

// The discriminator is the RESPONSE SOURCE, not the row shape: an error+null-state
// row means "gone" from the renderer and only "cannot see it" from the PTY
// fallback, and nothing else in the body distinguishes the two.
func TestTerminalStatusViewless(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"pty lookup failure", `{"source":"pty","terminals":[{"terminalId":"t1","agentState":null,"error":"Terminal not found or status unavailable"}]}`, true},
		{"pty with healthy rows", `{"source":"pty","unavailableFields":["exitCode","armed","lastCheckResult"],"terminals":[{"terminalId":"t1","agentState":"working"}]}`, true},
		{"renderer missing panel", `{"source":"renderer","terminals":[{"terminalId":"t1","agentState":null,"error":"Terminal not found"}]}`, false},
		{"legacy body without source", `{"terminals":[{"terminalId":"t1","agentState":"working"}]}`, false},
		{"unparseable text", `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			structured := any(nil)
			if json.Unmarshal([]byte(tc.body), &body) == nil {
				structured = body
			}
			if got := TerminalStatusViewless(nil, tc.body); got != tc.want {
				t.Errorf("text: got %v, want %v", got, tc.want)
			}
			if got := TerminalStatusViewless(structured, ""); got != tc.want {
				t.Errorf("structured: got %v, want %v", got, tc.want)
			}
		})
	}
}
