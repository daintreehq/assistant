package mcp

import (
	"fmt"
	"net/http"
	"testing"
)

// TestIsCredentialTerminalStatus pins BOTH halves of the fix:
//
//   - a 403 must now be recognised (it never was, so a revoked-by-403 bearer
//     reconnected forever against Daintree's abuse policy); and
//   - a bare "401"/"403" digit run must NOT be recognised, because the go-sdk
//     renders statuses as words and the old digit test could only ever fire on
//     unrelated text — permanently parking reconnects over a transient failure.
func TestIsCredentialTerminalStatus(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// The shapes the SDK actually produces, after client.go flattens both
		// transports into one Status.Error string.
		{"sdk unauthorized", "streamable-http: POST http://127.0.0.1/mcp: Unauthorized; sse: ", true},
		{"sdk forbidden", "streamable-http: POST http://127.0.0.1/mcp: Forbidden; sse: ", true},
		{"lowercase unauthorized", "unauthorized", true},
		{"unauthenticated", "request was unauthenticated", true},
		{"both transports forbidden", "streamable-http: Forbidden; sse: Forbidden", true},

		// Binding markers stay terminal (unchanged behavior).
		{"binding gone", "SESSION_BINDING_GONE", true},
		{"binding stale", "tool failed: BINDING_STALE", true},

		// The false-positive class the digit checks created. Every one of these is a
		// transient failure that must NOT permanently stop reconnecting.
		{"port 401 in url", "streamable-http: POST http://127.0.0.1:8401/mcp: connection refused; sse: ", false},
		{"403 inside a path", "streamable-http: GET http://h/w/terminal-403abc/mcp: fetch failed; sse: ", false},
		{"digits in a request id", "request 1403401 timed out", false},
		{"tool count", "server advertised 403 tools", false},

		// Ordinary transient failures.
		{"empty", "", false},
		{"connection refused", "streamable-http: ECONNREFUSED; sse: ECONNREFUSED", false},
		{"timeout", "context deadline exceeded", false},
		{"server error", "streamable-http: POST http://h/mcp: Internal Server Error; sse: ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCredentialTerminalStatus(tc.in); got != tc.want {
				t.Errorf("IsCredentialTerminalStatus(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCredentialTerminalMatchesSDKStatusText guards the assumption the fix rests on:
// the go-sdk formats a failed response with http.StatusText, so the words — not the
// numbers — are what reach this predicate. If a future SDK started embedding the
// numeric code, this test still passes (the words remain), but the companion
// negative cases above document why we do not match digits.
func TestCredentialTerminalMatchesSDKStatusText(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		// Mirrors streamableClientConn.checkResponse's format string.
		errText := fmt.Sprintf("POST http://127.0.0.1/mcp: %v", http.StatusText(code))
		if !IsCredentialTerminalStatus(errText) {
			t.Errorf("status %d renders as %q, which must be credential-terminal", code, errText)
		}
	}
	// A status that is NOT a credential problem must stay retryable.
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound} {
		errText := fmt.Sprintf("POST http://127.0.0.1/mcp: %v", http.StatusText(code))
		if IsCredentialTerminalStatus(errText) {
			t.Errorf("status %d (%q) must not be treated as a dead credential", code, errText)
		}
	}
}
