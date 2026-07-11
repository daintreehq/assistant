package mcp

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
)

// Transport errors (Go *url.Error and the SDK errors wrapping them) format the
// FULL request URL — including the query string, which for an MCP endpoint can
// carry credentials (?session=<token>). Every path that stores or logs a
// transport error must strip userinfo/query/fragment first.

func TestSanitizeErrTextStripsCredentialURLParts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		leak []string // substrings that must be gone
		keep []string // substrings that must survive
	}{
		{
			name: "query string",
			in:   `Post "http://127.0.0.1:8123/mcp?session=tok_supersecret123": connection refused`,
			leak: []string{"tok_supersecret123", "session="},
			keep: []string{"127.0.0.1:8123", "/mcp", "connection refused"},
		},
		{
			name: "userinfo",
			in:   `Get "https://user:hunter2@example.com/mcp": EOF`,
			leak: []string{"hunter2", "user:"},
			keep: []string{"example.com", "EOF"},
		},
		{
			name: "fragment",
			in:   `dial https://example.com/mcp#access_token=abcdef: timeout`,
			leak: []string{"access_token=abcdef"},
			keep: []string{"example.com", "timeout"},
		},
		{
			name: "no url untouched",
			in:   "plain transport failure",
			keep: []string{"plain transport failure"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeErrText(tc.in)
			for _, leak := range tc.leak {
				if strings.Contains(got, leak) {
					t.Fatalf("sanitized text still contains %q:\n%s", leak, got)
				}
			}
			for _, keep := range tc.keep {
				if !strings.Contains(got, keep) {
					t.Fatalf("sanitized text lost %q:\n%s", keep, got)
				}
			}
		})
	}
}

// A failing transport call whose error embeds a token-bearing URL must not leak
// the token into the stored connection error (Status().Error / lastError) nor
// into the mcp.call debug-log trace.
func TestTransportErrorWithTokenQueryNeverReachesStatusOrDebugLog(t *testing.T) {
	const token = "tok_supersecret_bearer_98765"
	transportErr := &url.Error{
		Op:  "Post",
		URL: "http://127.0.0.1:8123/mcp?session=" + token,
		Err: errors.New("connection refused"),
	}

	logDir := t.TempDir()
	low := &fakeLow{callErrs: []error{transportErr}}
	c := New(config.AppConfig{
		McpURL: "http://127.0.0.1:8123/mcp", McpToken: "t",
		DebugLog: true, LogDir: logDir,
	}, Options{ClientOverride: low})

	if _, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{}); err == nil {
		t.Fatal("CallTool should have failed with the transport error")
	}

	// 1. The degraded connection's stored error (Status().Error) carries no token.
	st := c.Status()
	if st.Error == "" {
		t.Fatal("transport failure did not record a connection error")
	}
	if strings.Contains(st.Error, token) || strings.Contains(st.Error, "session=") {
		t.Fatalf("Status().Error leaks the token-bearing query: %q", st.Error)
	}
	if !strings.Contains(st.Error, "127.0.0.1:8123") {
		t.Fatalf("sanitized connection error lost the host: %q", st.Error)
	}

	// 2. The mcp.call debug trace carries no token either.
	entries, err := os.ReadDir(logDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no debug log written (err=%v entries=%d)", err, len(entries))
	}
	var content strings.Builder
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(logDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		content.Write(b)
	}
	logged := content.String()
	if !strings.Contains(logged, "mcp.call") {
		t.Fatalf("mcp.call trace missing from debug log:\n%s", logged)
	}
	if strings.Contains(logged, token) {
		t.Fatalf("debug log leaks the transport error's token query:\n%s", logged)
	}
}
