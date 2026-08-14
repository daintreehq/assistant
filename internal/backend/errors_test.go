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

// IsAuth and the provider-account codes share HTTP statuses (401 and 403) and mean
// opposite things: one is "the header you sent us is wrong", the other is "the account
// behind your key is". They are separated by CODE for exactly that reason, and this
// pins the separation — a regression here sends a tester whose key the provider revoked
// into a re-paste loop that cannot possibly succeed.
func TestErrorAuthSeparatesOurDoorFromTheProvider(t *testing.T) {
	cases := []struct {
		name            string
		err             Error
		auth            bool
		upstreamAuth    bool
		providerAccount bool
	}{
		{"our door, malformed bearer", Error{HTTPStatus: 401, Code: "invalid_api_key"}, true, false, false},
		{"our door, 403", Error{HTTPStatus: 403, Code: "forbidden"}, true, false, false},
		{"provider rejected the key", Error{HTTPStatus: 401, Code: CodeProviderInvalidAPIKey}, false, true, true},
		{"account out of credit", Error{HTTPStatus: 402, Code: CodeProviderInsufficientCredit}, false, true, true},
		{"key not permitted", Error{HTTPStatus: 403, Code: CodeProviderKeyForbidden}, false, true, true},
		// Mid-stream the status is absent entirely, so the code has to carry it alone.
		{"provider rejected mid-stream", Error{Code: CodeProviderInvalidAPIKey, Stream: true}, false, true, true},
		// The pre-split blob still reads as an upstream-auth problem, but must NOT claim
		// to know which of the three it was.
		{"legacy catch-all", Error{HTTPStatus: 502, Code: CodeUpstreamError}, false, true, false},
		{"unrelated", Error{HTTPStatus: 500, Code: "internal_error"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.IsAuth(); got != tc.auth {
				t.Errorf("IsAuth() = %v, want %v", got, tc.auth)
			}
			if got := tc.err.IsUpstreamAuth(); got != tc.upstreamAuth {
				t.Errorf("IsUpstreamAuth() = %v, want %v", got, tc.upstreamAuth)
			}
			if got := tc.err.IsProviderAccount(); got != tc.providerAccount {
				t.Errorf("IsProviderAccount() = %v, want %v", got, tc.providerAccount)
			}
			// The per-code clause exists exactly when a specific account code was named.
			if got := tc.err.ProviderAccountReason() != ""; got != tc.providerAccount {
				t.Errorf("ProviderAccountReason() non-empty = %v, want %v", got, tc.providerAccount)
			}
		})
	}
}

// The three account clauses must be distinct. A copy-paste that gave two codes the same
// wording would pass every presence check above while telling a user with no credit to
// rotate a key that is working fine.
func TestProviderAccountReasonsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, code := range []string{CodeProviderInvalidAPIKey, CodeProviderInsufficientCredit, CodeProviderKeyForbidden} {
		reason := (&Error{Code: code}).ProviderAccountReason()
		if reason == "" {
			t.Errorf("%s has no advice clause", code)
			continue
		}
		if prev, dup := seen[reason]; dup {
			t.Errorf("%s and %s share the clause %q", prev, code, reason)
		}
		seen[reason] = code
	}
}

// IsReportable covers the two codes whose fix is a bug report, not an account or
// policy change. Getting this wrong in either direction is bad: a false positive tells
// a user to report their own expired key, a false negative leaves a real server bug
// looking like something they should fix themselves.
func TestErrorIsReportable(t *testing.T) {
	for _, tc := range []struct {
		err  Error
		want bool
	}{
		{Error{Code: CodeUpstreamRequestRejected}, true},
		{Error{Code: CodeUpstreamProtocolError}, true},
		{Error{Code: CodeUpstreamUnavailable}, false},
		{Error{Code: CodeProviderInvalidAPIKey}, false},
		{Error{Code: CodeUpstreamNoCompliantProvider}, false},
		{Error{}, false},
	} {
		if got := tc.err.IsReportable(); got != tc.want {
			t.Errorf("%q: IsReportable() = %v, want %v", tc.err.Code, got, tc.want)
		}
	}
}

// A routing dead end is neither an outage nor a bug — it is the caller's own policy
// matching nothing, and it is the only failure here they fix by changing that policy.
func TestErrorIsRoutingDeadEnd(t *testing.T) {
	if !(&Error{Code: CodeUpstreamNoCompliantProvider}).IsRoutingDeadEnd() {
		t.Error("the routing dead-end code is not recognised")
	}
	if (&Error{HTTPStatus: 503, Code: CodeUpstreamUnavailable}).IsRoutingDeadEnd() {
		t.Error("a plain provider outage was misread as a routing dead end — they share a 503")
	}
}
