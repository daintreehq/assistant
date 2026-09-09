package domain

// SubmissionReceipt is the durable observation for one async submission. It is
// correlation, never authority or proof that the agent consumed the input.
type SubmissionReceipt struct {
	Version    int    `json:"version"`
	Acceptance string `json:"acceptance"` // tracked | legacy_unknown | unreadable
	Token      string `json:"token,omitempty"`
	Phase      string `json:"phase,omitempty"`
	ObservedAt int64  `json:"observedAt,omitempty"` // local time a decisive phase was observed
}

// TerminalSubmission is Daintree's response to one token-specific status read.
type TerminalSubmission struct {
	Token string `json:"token"`
	Phase string `json:"phase"`
}

func ValidSubmissionPhase(phase string) bool {
	switch phase {
	case "queued", "writing", "pty_written", "failed", "cancelled", "unknown":
		return true
	}
	return false
}

func (r SubmissionReceipt) Decisive() bool {
	return r.Phase == "pty_written" || r.Phase == "failed" || r.Phase == "cancelled"
}

// PtyEndedWithoutOutcome observes lifecycle ending without inventing a task
// result. A true flag is not a health probe; nil means this surface did not know.
func PtyEndedWithoutOutcome(hasPty *bool, state string, exitCode *int) bool {
	return hasPty != nil && !*hasPty && state != "completed" && !(state == "exited" && exitCode != nil)
}

const PtyEndedUnverifiedReason = "PTY exited or a kill was requested; task success and process-tree teardown are unverified"

// Valid rejects corrupt/newer durable records instead of falling back to the
// legacy completion heuristic.
func (r SubmissionReceipt) Valid() bool {
	if r.Version != 1 {
		return false
	}
	switch r.Acceptance {
	case "legacy_unknown", "unreadable":
		return r.Token == "" && r.Phase == ""
	case "tracked":
		return r.Token != "" && len(r.Token) <= 128 && ValidSubmissionPhase(r.Phase) && (!r.Decisive() || r.ObservedAt > 0)
	}
	return false
}
