package mcp

import "testing"

// IsCredentialTerminalStatus is the gate that decides whether a degraded MCP
// connection is worth retrying (a dropped/evicted session — transient) or must
// stop trying (a revoked bearer — permanent, and retrying risks tripping
// Daintree's abuse policy). App.ensureStartupForTurn's mid-session recovery
// leans on this classification being correct in both directions.
func TestIsCredentialTerminalStatus(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty string is not terminal", "", false},
		{"plain session-evicted text is not terminal", "session not found", false},
		{"offline mode is not terminal", "offline mode", false},
		{"transport refused is not terminal", "connection refused", false},
		{"401 status is terminal", "request failed: 401", true},
		{"unauthorized word is terminal", "Unauthorized", true},
		{"unauthenticated word is terminal", "unauthenticated request", true},
		{"403 status is terminal", "403 Forbidden", true},
		{"SESSION_BINDING_GONE marker is terminal", "tool result: SESSION_BINDING_GONE", true},
		{"BINDING_STALE marker is terminal", "code=BINDING_STALE", true},
		{"lowercase binding marker is terminal", "session_binding_gone", true},
		// A "401"/"403" substring embedded in an unrelated digit run (a port, an
		// id, a byte count) must NOT read as an HTTP status — a connection-refused
		// error naming a local port is one of the most ordinary, non-terminal
		// failures this classifier has to pass through cleanly.
		{"port number containing 401 is not terminal", "connection refused: 127.0.0.1:54010", false},
		{"port number containing 403 is not terminal", "dial tcp 127.0.0.1:64031: connect: connection refused", false},
		{"longer digit run containing 401 is not terminal", "read 1234015 bytes", false},
		{"standalone 401 amid punctuation is still terminal", "upstream returned status=401", true},
		{"standalone 403 in parentheses is still terminal", "request failed (403)", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCredentialTerminalStatus(tc.in); got != tc.want {
				t.Errorf("IsCredentialTerminalStatus(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
