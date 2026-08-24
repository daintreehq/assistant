package app

import (
	"context"
	"fmt"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/storage"

	"github.com/daintreehq/assistant/internal/tools/memory"
	queuetools "github.com/daintreehq/assistant/internal/tools/queue"
	"github.com/daintreehq/assistant/internal/tools/runbook"
	"github.com/daintreehq/assistant/internal/tools/timer"
	"github.com/daintreehq/assistant/internal/tools/watcher"
	"github.com/daintreehq/assistant/internal/tools/workflow"
)

// This file bridges the per-family Store/Queue consumer interfaces onto the
// concrete *storage.Store and *queue.Queue. The recurring shape differences each
// adapter normalizes: the families carry a context.Context the no-ctx storage layer
// omits; the storage status-update methods are patch-map based while the families
// pass a status string or a full record; the storage revoke/list methods take an
// explicit now stamp (0 ⇒ store fills it). Inserts that return the persisted record
// are projected to the id the family wants.

/* -------------------------------- memory --------------------------------- */

type memoryStoreAdapter struct{ s *storage.Store }

func (a memoryStoreAdapter) RecallMemories(_ context.Context, query string, opts memory.RecallOptions) ([]domain.MemoryRecord, error) {
	return a.s.RecallMemories(query, storage.MemoryRecallOptions{
		Category: strPtrOrNil(opts.Category),
		Limit:    intPtrOrNil(opts.Limit),
	})
}

// memoryRecallerAdapter bridges the agent's narrow per-turn footer-recall seam
// (agent.MemoryRecaller — a no-ctx (query, limit) signature) onto *storage.Store's
// option-bearing RecallMemories. Kept beside memoryStoreAdapter so the memory
// adapters live together; the footer-recall path is ctx-free and best-effort, and
// the storage layer clamps the limit, so only the limit needs forwarding.
type memoryRecallerAdapter struct{ s *storage.Store }

func (a memoryRecallerAdapter) RecallMemories(query string, limit int) ([]domain.MemoryRecord, error) {
	return a.s.RecallMemories(query, storage.MemoryRecallOptions{Limit: &limit})
}

// pinnedMemoryListerAdapter bridges the agent's narrow per-round footer pinned-memory
// seam (agent.PinnedMemoryLister — a no-ctx (limit) signature) onto *storage.Store's
// option-bearing ListMemories with PinnedOnly set. Kept beside the other memory adapters;
// the footer pinned read is ctx-free and best-effort, so only the limit needs forwarding.
type pinnedMemoryListerAdapter struct{ s *storage.Store }

func (a pinnedMemoryListerAdapter) ListPinnedMemories(limit int) ([]domain.MemoryRecord, error) {
	return a.s.ListMemories(storage.MemoryListOptions{PinnedOnly: true, Limit: &limit})
}

func (a memoryStoreAdapter) ListMemories(_ context.Context, opts memory.ListOptions) ([]domain.MemoryRecord, error) {
	return a.s.ListMemories(storage.MemoryListOptions{
		Category:   strPtrOrNil(opts.Category),
		PinnedOnly: opts.PinnedOnly,
		Limit:      intPtrOrNil(opts.Limit),
	})
}

func (a memoryStoreAdapter) InsertMemory(_ context.Context, rec domain.MemoryRecord) (string, error) {
	out, err := a.s.InsertMemory(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a memoryStoreAdapter) ForgetMemory(_ context.Context, id string) (bool, error) {
	return a.s.ForgetMemory(id, domain.NowMS())
}

func (a memoryStoreAdapter) PinMemory(_ context.Context, id string) (*domain.MemoryRecord, error) {
	return a.s.PinMemory(id, domain.NowMS())
}

func (a memoryStoreAdapter) UnpinMemory(_ context.Context, id string) (*domain.MemoryRecord, error) {
	return a.s.UnpinMemory(id, domain.NowMS())
}

/* --------------------------------- queue --------------------------------- */

// queueToolAdapter forwards the queue family's tools to the live *queue.Queue. The
// concrete Queue already carries ctx-bearing Publish/Digest/Resolve/Format, so this
// is a thin pass-through (kept as an adapter so the family stays App-agnostic).
type queueToolAdapter struct{ app *App }

func (a queueToolAdapter) Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	return a.app.Queue.Publish(ctx, args)
}
func (a queueToolAdapter) Digest(ctx context.Context, opts domain.QueueDigestOptions) ([]domain.QueueEvent, error) {
	return a.app.Queue.Digest(ctx, opts)
}
func (a queueToolAdapter) Resolve(ctx context.Context, id string) (bool, error) {
	return a.app.Queue.Resolve(ctx, id)
}
func (a queueToolAdapter) Format(events []domain.QueueEvent) string {
	return a.app.Queue.Format(events)
}

var _ queuetools.Queue = queueToolAdapter{}

/* --------------------------------- runbook --------------------------------- */

type runbookStoreAdapter struct{ s *storage.Store }

func (a runbookStoreAdapter) GetRunbookRunState(_ context.Context, sessionID, runbookID string) (*domain.RunbookRunStateRecord, error) {
	return a.s.GetRunbookRunState(sessionID, runbookID)
}

func (a runbookStoreAdapter) InsertRunbookRunState(_ context.Context, rec domain.RunbookRunStateRecord) (string, error) {
	out, err := a.s.InsertRunbookRunState(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

// UpdateRunbookRunState projects the family's full-record update onto the storage
// patch-map update (the allowlisted run-state columns: currentStep/stepsJson/
// status/completedAt; updatedAt is store-forced).
func (a runbookStoreAdapter) UpdateRunbookRunState(_ context.Context, rec domain.RunbookRunStateRecord) error {
	patch := map[string]any{
		"currentStep": rec.CurrentStep,
		"stepsJson":   rec.StepsJson,
		"status":      string(rec.Status),
	}
	if rec.CompletedAt != nil {
		patch["completedAt"] = *rec.CompletedAt
	}
	return a.s.UpdateRunbookRunState(rec.ID, patch)
}

/* --------------------------------- timer --------------------------------- */

type timerStoreAdapter struct{ s *storage.Store }

func (a timerStoreAdapter) InsertTimer(_ context.Context, rec domain.TimerRecord) (string, error) {
	out, err := a.s.InsertTimer(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a timerStoreAdapter) ListTimers(_ context.Context, status string) ([]domain.TimerRecord, error) {
	return a.s.ListTimers(status)
}

func (a timerStoreAdapter) GetTimer(_ context.Context, id string) (*domain.TimerRecord, error) {
	return a.s.GetTimer(id)
}

func (a timerStoreAdapter) UpdateTimerStatus(_ context.Context, id, status string) error {
	return a.s.UpdateTimer(id, map[string]any{"status": status})
}

func (a timerStoreAdapter) RevokeGrantsByActor(_ context.Context, actorID string) (int, error) {
	return a.s.RevokeGrantsByActor(actorID, domain.NowMS())
}

/* -------------------------------- watcher -------------------------------- */

type watcherStoreAdapter struct{ s *storage.Store }

func (a watcherStoreAdapter) InsertWatcher(_ context.Context, rec domain.WatcherRecord) (string, error) {
	out, err := a.s.InsertWatcher(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a watcherStoreAdapter) ListWatchers(_ context.Context, status string) ([]domain.WatcherRecord, error) {
	return a.s.ListWatchers(status)
}

func (a watcherStoreAdapter) GetWatcher(_ context.Context, id string) (*domain.WatcherRecord, error) {
	return a.s.GetWatcher(id)
}

// CancelWatcher flips the watcher to 'cancelled' and stamps the reason + cancel time,
// so a user cancel is distinguishable from the session-boundary sweep's
// 'session_ended'. The flip is guarded on the live statuses (CancelLiveWatcher):
// the tool's pre-read terminal check is advisory, and a cancel that loses the race
// to a natural finalize or an in-turn consumption must NOT clobber that end state —
// the error return also stops the tool from closing the linked workflow run as
// cancelled over an already-done one.
func (a watcherStoreAdapter) CancelWatcher(_ context.Context, id, reason string) error {
	flipped, err := a.s.CancelLiveWatcher(id, reason, domain.NowMS())
	if err != nil {
		return err
	}
	if !flipped {
		return fmt.Errorf("watcher %s already ended (a concurrent finalize won)", id)
	}
	return nil
}

func (a watcherStoreAdapter) RevokeGrantsByActor(_ context.Context, actorID string) (int, error) {
	return a.s.RevokeGrantsByActor(actorID, domain.NowMS())
}

// UpdateWorkflowRun forwards the allowlisted patch so watcher.cancel can close a
// supervised run's ledger row (the daemon never re-checks a cancelled watcher).
func (a watcherStoreAdapter) UpdateWorkflowRun(_ context.Context, id string, patch map[string]any) error {
	return a.s.UpdateWorkflowRun(id, patch)
}

/* ------------------------------- workflow -------------------------------- */

type workflowStoreAdapter struct{ s *storage.Store }

func (a workflowStoreAdapter) InsertWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) (string, error) {
	out, err := a.s.InsertWorkflowRun(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a workflowStoreAdapter) GetWorkflowRun(_ context.Context, id string) (*domain.WorkflowRunRecord, error) {
	return a.s.GetWorkflowRun(id)
}

func (a workflowStoreAdapter) ListWorkflowRuns(_ context.Context, status string) ([]domain.WorkflowRunRecord, error) {
	return a.s.ListWorkflowRuns(status)
}

// UpdateWorkflowRun projects the family's full-record update onto the storage
// patch-map update over the allowlisted columns. Only non-nil optional fields are
// included; status is always written; updatedAt is store-forced.
func (a workflowStoreAdapter) UpdateWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) error {
	patch := map[string]any{"status": string(rec.Status)}
	if rec.IssueNumber != nil {
		patch["issueNumber"] = *rec.IssueNumber
	}
	if rec.IssueURL != nil {
		patch["issueUrl"] = *rec.IssueURL
	}
	if rec.IssueTitle != nil {
		patch["issueTitle"] = *rec.IssueTitle
	}
	if rec.Branch != nil {
		patch["branch"] = *rec.Branch
	}
	if rec.WorktreeID != nil {
		patch["worktreeId"] = *rec.WorktreeID
	}
	if rec.PRNumber != nil {
		patch["prNumber"] = *rec.PRNumber
	}
	if rec.PRURL != nil {
		patch["prUrl"] = *rec.PRURL
	}
	if rec.TerminalIdsJson != nil {
		patch["terminalIdsJson"] = *rec.TerminalIdsJson
	}
	if rec.WatcherIdsJson != nil {
		patch["watcherIdsJson"] = *rec.WatcherIdsJson
	}
	if rec.QueueEventIdsJson != nil {
		patch["queueEventIdsJson"] = *rec.QueueEventIdsJson
	}
	if rec.NextActionJson != nil {
		patch["nextActionJson"] = *rec.NextActionJson
	}
	if rec.NotesJson != nil {
		patch["notesJson"] = *rec.NotesJson
	}
	if rec.CompletedAt != nil {
		patch["completedAt"] = *rec.CompletedAt
	}
	return a.s.UpdateWorkflowRun(rec.ID, patch)
}

/* ------------------------------- ptr helpers ----------------------------- */

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func intPtrOrNil(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

var (
	_ memory.Store         = memoryStoreAdapter{}
	_ runbook.RunbookStore = runbookStoreAdapter{}
	_ timer.Store          = timerStoreAdapter{}
	_ watcher.Store        = watcherStoreAdapter{}
	_ workflow.Store       = workflowStoreAdapter{}
	// *storage.Store directly satisfies the agent distill-on-compact seam (no adapter
	// needed — its no-ctx, record-returning InsertMemory + MemoryExists match exactly).
	_ agent.MemoryStore = (*storage.Store)(nil)
	// The footer-recall seam needs a thin adapter — *storage.Store.RecallMemories takes
	// MemoryRecallOptions, not the agent seam's narrow (query, limit).
	_ agent.MemoryRecaller = memoryRecallerAdapter{}
	// The footer pinned-memory seam needs a thin adapter — *storage.Store.ListMemories
	// takes MemoryListOptions, not the agent seam's narrow (limit).
	_ agent.PinnedMemoryLister = pinnedMemoryListerAdapter{}
	// *storage.Store directly satisfies the agent artifact-persist seam (its no-ctx,
	// record-returning InsertArtifact matches the interface exactly).
	_ agent.ArtifactPersister = (*storage.Store)(nil)
	// *storage.Store directly satisfies the agent footer workflow-lister seam (its
	// no-ctx ListNonTerminalWorkflowRuns matches the interface exactly).
	_ agent.WorkflowRunLister = (*storage.Store)(nil)
)
