package mcp

import (
	"encoding/json"
	"testing"
)

func TestTerminalStatusReadUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"pty lookup failure", `{"source":"pty","terminals":[{"terminalId":"t1","agentState":null,"error":"Terminal not found or status unavailable"}]}`, true},
		{"partial pty batch", `{"source":"pty","terminals":[{"terminalId":"t1","agentState":"working"},{"terminalId":"t2","agentState":null,"error":"Terminal not found or status unavailable"}]}`, true},
		{"renderer missing panel", `{"source":"renderer","terminals":[{"terminalId":"t1","agentState":null,"error":"Terminal not found"}]}`, false},
		{"pty finished without code", `{"source":"pty","unavailableFields":["exitCode","armed","lastCheckResult"],"terminals":[{"terminalId":"t1","agentState":"completed"}]}`, false},
		{"output failure with known state", `{"source":"pty","terminals":[{"terminalId":"t1","agentState":"working","error":"output unavailable"}]}`, false},
		{"plain shell without state", `{"source":"pty","terminals":[{"terminalId":"t1","agentState":null}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatal(err)
			}
			for _, structured := range []bool{false, true} {
				var got bool
				if structured {
					got = TerminalStatusReadUnavailable(body, "")
				} else {
					got = TerminalStatusReadUnavailable(nil, tc.body)
				}
				if got != tc.want {
					t.Errorf("structured=%v: got %v, want %v", structured, got, tc.want)
				}
			}
		})
	}
}
