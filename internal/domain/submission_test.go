package domain

import (
	"strings"
	"testing"
)

// Valid is the gate between a durable receipt and the legacy completion
// heuristic: a record that fails it is discarded rather than half-trusted, so
// every field combination the storage layer can round-trip is pinned here.
func TestSubmissionReceiptValid(t *testing.T) {
	cases := []struct {
		name    string
		receipt SubmissionReceipt
		want    bool
	}{
		{"tracked queued", SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: "t", Phase: "queued"}, true},
		{"tracked decisive with an observation time", SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: "t", Phase: "pty_written", ObservedAt: 5}, true},
		// A decisive phase with no observation time cannot be placed in time, and
		// submissionGraceStart would start the grace at zero.
		{"tracked decisive without one", SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: "t", Phase: "pty_written"}, false},
		{"tracked without a token", SubmissionReceipt{Version: 1, Acceptance: "tracked", Phase: "queued"}, false},
		{"tracked with an over-long token", SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: strings.Repeat("t", 129), Phase: "queued"}, false},
		{"tracked with an unknown phase", SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: "t", Phase: "beamed"}, false},
		{"legacy_unknown", SubmissionReceipt{Version: 1, Acceptance: "legacy_unknown"}, true},
		{"unreadable", SubmissionReceipt{Version: 1, Acceptance: "unreadable"}, true},
		// A receipt with no token to correlate must not carry phase evidence.
		{"legacy_unknown carrying a phase", SubmissionReceipt{Version: 1, Acceptance: "legacy_unknown", Phase: "pty_written"}, false},
		{"unreadable carrying a token", SubmissionReceipt{Version: 1, Acceptance: "unreadable", Token: "t"}, false},
		{"unknown acceptance", SubmissionReceipt{Version: 1, Acceptance: "maybe"}, false},
		// A newer writer's record is rejected, never reinterpreted under v1 rules.
		{"future version", SubmissionReceipt{Version: 2, Acceptance: "legacy_unknown"}, false},
		{"zero value", SubmissionReceipt{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.receipt.Valid(); got != tc.want {
				t.Fatalf("Valid() = %v, want %v for %+v", got, tc.want, tc.receipt)
			}
		})
	}
}

func TestSubmissionReceiptDecisive(t *testing.T) {
	for phase, want := range map[string]bool{
		"pty_written": true, "failed": true, "cancelled": true,
		"queued": false, "writing": false, "unknown": false, "": false,
	} {
		if got := (SubmissionReceipt{Phase: phase}).Decisive(); got != want {
			t.Errorf("Decisive(%q) = %v, want %v", phase, got, want)
		}
	}
}
