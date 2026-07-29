package backend

import "testing"

// IsRateLimited ORs three independent signals, so a table that supplies several at
// once cannot tell a live check from a dead one. Each case here isolates ONE signal.
//
// The "_error"-suffixed type is pinned deliberately: the backend speaks the OpenAI
// taxonomy (rate_limit_error), and an earlier revision compared against a bare
// "rate_limit" that the backend never emits — a silently dead branch. The negative
// case below is what keeps it from creeping back.
func TestErrorIsRateLimited(t *testing.T) {
	cases := []struct {
		name string
		err  Error
		want bool
	}{
		{"status only", Error{HTTPStatus: 429}, true},
		{"type only, mid-stream (no HTTP status)", Error{Type: "rate_limit_error", Stream: true}, true},
		{"code only, mid-stream", Error{Code: "upstream_rate_limited", Stream: true}, true},
		{"legacy unsuffixed type is NOT a rate limit", Error{Type: "rate_limit"}, false},
		{"other error", Error{HTTPStatus: 400, Type: "invalid_request_error", Code: "bad"}, false},
		{"zero value", Error{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.IsRateLimited(); got != tc.want {
				t.Fatalf("IsRateLimited() = %v, want %v (err %+v)", got, tc.want, tc.err)
			}
		})
	}
}
