package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
)

// The retry budget can now span ~a minute of wall clock while the backend restarts.
// The cue is what keeps that from reading as a hang, so it must name the cause in the
// user's terms and show the position in the budget — not echo a dialer blob.
func TestRetryNotice(t *testing.T) {
	cases := []struct {
		name     string
		info     backend.RetryInfo
		contains []string
		absent   []string
	}{
		{
			name: "connect failure is a connectivity message",
			info: backend.RetryInfo{
				Attempt: 0, MaxAttempts: 10, Delay: 500 * time.Millisecond,
				Err: &backend.Error{Code: "connect", Message: "dial tcp 127.0.0.1:8473: connect: connection refused"},
			},
			contains: []string{"Can't reach the backend", "500ms", "up to 10 attempts"},
			absent:   []string{"dial tcp"},
		},
		{
			name: "rate limit is named as such",
			info: backend.RetryInfo{
				Attempt: 4, MaxAttempts: 10, Delay: 12 * time.Second,
				Err: &backend.Error{HTTPStatus: 429, Message: "slow down"},
			},
			contains: []string{"Model rate-limited", "12s", "up to 10 attempts"},
		},
		{
			name: "anything else stays generic",
			info: backend.RetryInfo{
				Attempt: 1, MaxAttempts: 10, Delay: 2 * time.Second,
				Err: &backend.Error{Code: "upstream_error", Message: "boom", Stream: true},
			},
			contains: []string{"Backend error", "2s", "up to 10 attempts"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := retryNotice(tc.info)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("notice %q missing %q", got, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(got, unwanted) {
					t.Errorf("notice %q leaks transport noise %q", got, unwanted)
				}
			}
		})
	}
}

// The countdown must not lie: a jittered ramp value rounded to whole seconds showed a
// 1.4s wait as "1s", while the steady-state poll stays clean whole seconds.
func TestFormatRetryDelay(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{450 * time.Millisecond, "450ms"},
		{time.Second, "1s"},
		{1400 * time.Millisecond, "1.4s"},
		{2 * time.Second, "2s"},
		{7500 * time.Millisecond, "7.5s"},
		{11700 * time.Millisecond, "12s"},
		{15 * time.Second, "15s"},
	}
	for _, tc := range cases {
		if got := formatRetryDelay(tc.in); got != tc.want {
			t.Errorf("formatRetryDelay(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
