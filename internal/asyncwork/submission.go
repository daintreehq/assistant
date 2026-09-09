package asyncwork

import (
	"context"
	"fmt"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// probeBudget is ONE pass's whole allowance for submission probes: at most
// maxSubmissionProbesPerPass reads, all of them sharing a single
// submissionProbeBudgetMS deadline. Built lazily so a pass that probes nothing
// allocates nothing, and bounded by CANCELLING rather than by a context
// deadline (the MCP client degrades a connection on DeadlineExceeded, and these
// are best-effort reads). release() must run once the pass is done with it.
type probeBudget struct {
	parent context.Context
	ctx    context.Context
	cancel context.CancelFunc
	timer  *time.Timer
	used   int
}

// take reserves one probe and hands back the SHARED bounded context. ok=false
// once the allowance is spent or the shared deadline has already elapsed — the
// remaining invocations simply retry on the next tick instead of extending the
// pass.
func (b *probeBudget) take() (context.Context, bool) {
	if b.used >= maxSubmissionProbesPerPass {
		return nil, false
	}
	if b.ctx == nil {
		b.ctx, b.cancel = context.WithCancel(b.parent)
		b.timer = time.AfterFunc(time.Duration(submissionProbeBudgetMS)*time.Millisecond, b.cancel)
	}
	if b.ctx.Err() != nil {
		return nil, false
	}
	b.used++
	return b.ctx, true
}

func (b *probeBudget) release() {
	if b.timer != nil {
		b.timer.Stop()
	}
	if b.cancel != nil {
		b.cancel()
	}
}

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

// submissionReady gates the seenWorking/idle COMPLETION verdict until this
// exact send reached the PTY. That is transport evidence, not agent
// consumption. A false verdict withholds only that verdict: the caller still
// feeds the batched read so closure and PTY-loss detection keep running (see
// feedStatuses' completionAllowed).
func (c *Coordinator) submissionReady(t *tracked, batch StatusReadResult, now int64, budget *probeBudget) (StatusReadResult, bool) {
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
	if t.lastSubmissionReadAt != 0 && now-t.lastSubmissionReadAt < submissionReadMinIntervalMS {
		return StatusReadResult{}, false
	}
	reader, canRead := c.deps.Reader.(SubmissionReader)
	store, canSave := c.deps.Store.(SubmissionStore)
	if !canRead || !canSave || len(t.terminalIDs) != 1 {
		return StatusReadResult{}, false
	}
	probeCtx, granted := budget.take()
	if !granted {
		return StatusReadResult{}, false
	}
	t.lastSubmissionReadAt = now
	observation, snapshot, ok := reader.ReadSubmission(probeCtx, t.terminalIDs[0], receipt.Token)
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
