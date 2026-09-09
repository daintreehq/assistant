package asyncwork

import (
	"context"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
)

func submissionGraceStart(t *tracked) int64 {
	if r := t.rec.Submission; r != nil && r.Acceptance == "tracked" && r.Phase == "pty_written" {
		return r.ObservedAt
	}
	return t.rec.CreatedAt
}

func submissionDeadlineReason(t *tracked) string {
	if r := t.rec.Submission; r != nil && r.Acceptance != "legacy_unknown" && r.Phase != "pty_written" {
		return "submission could not be verified before the deadline; input may remain queued or partially written — inspect the terminal before sending again"
	}
	return "still working when the deadline passed"
}

// submissionReady gates both seenWorking and idle detection until this exact
// send reached the PTY. That is transport evidence, not agent consumption.
func (c *Coordinator) submissionReady(ctx context.Context, t *tracked, batch StatusReadResult, now int64, probes *int) (StatusReadResult, bool) {
	receipt := t.rec.Submission
	if receipt == nil || receipt.Acceptance == "legacy_unknown" {
		return batch, true
	}
	if receipt.Acceptance != "tracked" {
		return StatusReadResult{}, false
	}
	if receipt.Phase == "pty_written" {
		return batch, true
	}
	if receipt.Phase == "failed" || receipt.Phase == "cancelled" {
		c.failSubmission(t, receipt.Phase)
		return StatusReadResult{}, false
	}
	if *probes >= 4 || (t.lastSubmissionReadAt != 0 && now-t.lastSubmissionReadAt < 5000) {
		return StatusReadResult{}, false
	}
	reader, canRead := c.deps.Reader.(SubmissionReader)
	store, canSave := c.deps.Store.(SubmissionStore)
	if !canRead || !canSave || len(t.terminalIDs) != 1 {
		return StatusReadResult{}, false
	}
	*probes++
	t.lastSubmissionReadAt = now
	observation, snapshot, ok := reader.ReadSubmission(ctx, t.terminalIDs[0], receipt.Token)
	if !ok || observation.Token != receipt.Token || !domain.ValidSubmissionPhase(observation.Phase) {
		return StatusReadResult{}, false
	}
	next := *receipt
	next.Phase = observation.Phase
	if !next.Decisive() {
		return StatusReadResult{}, false
	}
	next.ObservedAt = now
	// Persist before latching. Cancellation wins the live claim, and failed
	// storage leaves the invocation waiting for another observation.
	saved, err := store.CheckpointAsyncSubmission(t.rec.ID, next, next.ObservedAt)
	if err != nil || !saved {
		return StatusReadResult{}, false
	}
	t.rec.Submission = &next
	if next.Phase != "pty_written" {
		c.failSubmission(t, next.Phase)
		return StatusReadResult{}, false
	}
	return snapshot, true
}

func (c *Coordinator) failSubmission(t *tracked, phase string) {
	for _, st := range t.perTerminal {
		if st.outcome == nil {
			st.outcome = &domain.AsyncTerminalOutcome{Status: domain.SettleStatusFailed, Reason: fmt.Sprintf("submission %s before confirmed PTY write; partial input may remain in the composer — inspect before sending again; task execution is unverified", phase)}
		}
	}
}
