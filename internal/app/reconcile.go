package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
)

// reconcileReadTimeout bounds the single terminal.list read the boot ledger reconcile
// makes. Like the startup-context reads it is CANCEL-based, never context.WithTimeout:
// mcp.Client degrades the connection on a non-abort CallTool error and a
// DeadlineExceeded is NOT an abort (only a Canceled is), so a slow read must surface
// as a cancel rather than tearing down the freshly-connected transport.
const reconcileReadTimeout = 5 * time.Second

// reconcileCandidateCap bounds how many ledger rows the boot reconcile inspects. It
// only needs to expire the handful a prior session could have left live; an unbounded
// scan at every connect would be wasteful.
const reconcileCandidateCap = 50

// ledgerReconcileMCP is the narrow MCP seam the boot reconcile needs: one
// terminal.list read. Satisfied by *mcp.Client; a fake in tests returns a scripted
// inventory.
type ledgerReconcileMCP interface {
	CallTool(ctx context.Context, name string, args map[string]any, opts mcp.CallOptions) (mcp.CallResult, error)
}

// ledgerReconcileStore is the narrow store seam: read the open ledger rows and the
// confirmed-with-terminal launch sagas, look up a backing run, and cancel a stale run.
// Satisfied by *storage.Store.
type ledgerReconcileStore interface {
	ListNonTerminalWorkflowRuns(limit int) ([]domain.WorkflowRunRecord, error)
	ListConfirmedAgentLaunchesWithTerminal(limit int) ([]domain.AgentLaunchRecord, error)
	GetWorkflowRun(id string) (*domain.WorkflowRunRecord, error)
	UpdateWorkflowRun(id string, patch map[string]any) error
	// The adopted-watcher pass. Retirement is ONE transactional, status-guarded store
	// operation rather than a revoke plus an update from here: the teardown is three
	// writes that have to land together (grants, the row, and the events the watcher
	// already raised), and a half-applied one is worse than none.
	ListLiveWatchers() ([]domain.WatcherRecord, error)
	RetireWatcherOnResume(id string, now int64) (bool, error)
}

// ReconcileLedger cross-checks the assistant's own durable ledger against the live
// Daintree terminal inventory at boot and expires references a prior session left
// behind. A workflow run that bound one or more terminals but whose terminals are ALL
// gone from terminal.list is no longer live, so it is cancelled; likewise a confirmed
// agent-launch saga whose terminal vanished has its backing ledger run cancelled. It
// also RETIRES a terminal watcher whose every target is gone from that same inventory.
//
// That pass exists because supervision is durable now: BeginOwnership adopts every live
// watcher from the previous owner and checks nothing about the terminals they point at,
// which is right when only the assistant restarted (Daintree still owns those
// terminals) and wrong when DAINTREE restarted, because the terminals died with it. An
// adopted watcher then ticked, found its target absent, and classified that as
// `terminal_exited` — SeverityUrgent, publishable, and an autonomous wake — so
// restarting the app spent a model turn reporting the exit that the restart itself had
// caused, on a terminal it could no longer read. Retiring the row here, before the
// scheduler ever sees it, is the difference between stale bookkeeping and an event. The
// distinction it draws is whether the watcher was ever running while its terminal was
// alive: a terminal that dies under a live watcher is real news and still publishes.
//
// It NEVER re-arms watchers, touches live terminals, or promotes agents/worktrees to
// local entities — it only reconciles the assistant's own bookkeeping.
//
// Best-effort and bounded: a terminal.list failure or error result skips reconciliation
// entirely (it never aborts a connect or guesses an empty inventory from a failed read),
// and every DB error is swallowed to the debug log. Runs whose terminals are still
// live, and runs that never bound a terminal (e.g. pending pre-spawn), are left
// untouched. Returns the number of ledger rows cancelled (0 on any skip) for
// observability and tests.
func ReconcileLedger(ctx context.Context, store ledgerReconcileStore, client ledgerReconcileMCP, dbg debuglog.Config) int {
	cancelled, _ := reconcileLedger(ctx, store, client, dbg)
	return cancelled
}

// reconcileLedger's second result says the terminal inventory was successfully read and
// the attempt ran to completion. App uses it to commit its once-only state only after a
// real attempt; the public count-only wrapper remains stable for existing callers/tests.
func reconcileLedger(ctx context.Context, store ledgerReconcileStore, client ledgerReconcileMCP, dbg debuglog.Config) (int, bool) {
	// Boot reconcile is Refresh-class MCP traffic: worth doing promptly, but it
	// must queue behind in-turn tool calls (and it degrades to a skip anyway).
	ctx = mcp.WithPriority(ctx, mcp.PriorityRefresh)
	live, ok := readLiveTerminalIDs(ctx, client)
	if !ok {
		// A failed/error read must NOT be read as "every terminal gone" — skip, leaving
		// the ledger untouched, so a transient transport blip can't mass-cancel runs.
		return 0, false
	}

	cancelled := 0
	seen := map[string]struct{}{} // run ids already cancelled, so the two passes don't double-count

	cancelRun := func(id, reason string) {
		if id == "" {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		// Co-stamp completedAt alongside the terminal status, matching every other
		// cancellation path (watcher daemon, workflow.update) so a reconcile-cancelled run
		// has a valid completion timestamp rather than a NULL.
		if err := store.UpdateWorkflowRun(id, map[string]any{
			"status":      string(domain.WorkflowCancelled),
			"completedAt": domain.NowMS(),
		}); err != nil {
			debuglog.LogDebug(dbg, "reconcile.cancel.error", map[string]any{"runId": id, "error": err.Error()})
			return
		}
		seen[id] = struct{}{}
		cancelled++
		debuglog.LogDebug(dbg, "reconcile.run.cancelled", map[string]any{"runId": id, "reason": reason})
	}

	// Pass 1: open workflow runs whose every bound terminal is gone.
	if runs, err := store.ListNonTerminalWorkflowRuns(reconcileCandidateCap); err != nil {
		debuglog.LogDebug(dbg, "reconcile.list.workflows.error", map[string]any{"error": err.Error()})
	} else {
		for _, run := range runs {
			terms := parseIDList(run.TerminalIdsJson)
			if len(terms) == 0 {
				continue // never bound a terminal (e.g. pending pre-spawn) — nothing to expire
			}
			if anyLive(terms, live) {
				continue
			}
			cancelRun(run.ID, "all bound terminals gone on resume")
		}
	}

	// Pass 2: confirmed agent-launch sagas whose terminal vanished — cancel the backing
	// ledger run. This is the boot-reconcile seam's first production caller. A launch
	// whose terminal is STILL live is a running-but-unsupervised orphan: leave it (the
	// fresh-start invariant forbids re-arming its watcher).
	if launches, err := store.ListConfirmedAgentLaunchesWithTerminal(reconcileCandidateCap); err != nil {
		debuglog.LogDebug(dbg, "reconcile.list.launches.error", map[string]any{"error": err.Error()})
	} else {
		for _, l := range launches {
			if l.TerminalID == nil || *l.TerminalID == "" {
				continue
			}
			if _, isLive := live[*l.TerminalID]; isLive {
				continue
			}
			if l.WorkflowRunID == nil || *l.WorkflowRunID == "" {
				continue // no ledger row to reconcile
			}
			run, err := store.GetWorkflowRun(*l.WorkflowRunID)
			if err != nil || run == nil {
				continue
			}
			if isTerminalWorkflowStatus(run.Status) {
				continue // already done/cancelled/failed
			}
			cancelRun(run.ID, "agent-launch terminal gone on resume")
		}
	}

	// Pass 3: adopted terminal watchers with nothing left to watch.
	//
	// Scoped to kind "terminal": a pr_state watcher observes a forge, not a process,
	// and survives any number of Daintree restarts. A watcher carrying NO targets is
	// left alone too — there is nothing to check, and guessing would retire a row on
	// the strength of an empty list.
	retired := 0
	if watchers, err := store.ListLiveWatchers(); err != nil {
		debuglog.LogDebug(dbg, "reconcile.list.watchers.error", map[string]any{"error": err.Error()})
	} else {
		for _, w := range watchers {
			if w.Kind != "terminal" {
				continue
			}
			targets := parseIDList(&w.TargetsJson)
			if len(targets) == 0 || anyLive(targets, live) {
				continue
			}
			// One transaction, all three writes, and only while the row is still live.
			// A false here is the scheduler having finalized the same watcher with a
			// real verdict first — its answer is the better one, so leave it.
			won, err := store.RetireWatcherOnResume(w.ID, domain.NowMS())
			if err != nil {
				debuglog.LogDebug(dbg, "reconcile.watcher.retire.error",
					map[string]any{"watcherId": w.ID, "error": err.Error()})
				continue
			}
			if !won {
				continue
			}
			retired++
			debuglog.LogDebug(dbg, "reconcile.watcher.retired",
				map[string]any{"watcherId": w.ID, "title": w.Title, "targets": targets})
		}
	}

	if cancelled > 0 || retired > 0 {
		debuglog.LogDebug(dbg, "reconcile.summary", map[string]any{
			"cancelled": cancelled, "watchersRetired": retired, "liveTerminals": len(live)})
	}
	return cancelled, true
}

// readLiveTerminalIDs reads the live terminal inventory once, bounded by a cancel-based
// deadline. Returns (ids, true) ONLY on a successful call that actually carried a
// parseable terminals array (an empty array is a valid answer: every terminal is
// genuinely gone). Returns (nil, false) on a transport error, an error result, OR a
// "successful" response whose body has no parseable terminals array (e.g. a plaintext
// "temporarily unavailable" or a schema-changed payload) — so a garbage-but-non-error
// read can't be misread as "every terminal gone" and mass-cancel the ledger.
func readLiveTerminalIDs(ctx context.Context, client ledgerReconcileMCP) (map[string]struct{}, bool) {
	rctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(reconcileReadTimeout, cancel)
	res, err := client.CallTool(rctx, "terminal.list", map[string]any{}, mcp.CallOptions{})
	timer.Stop()
	cancel()
	if err != nil || res.IsError {
		return nil, false
	}
	ids, sawArray := parseTerminalIDs(res.StructuredContent, res.Text)
	if !sawArray {
		return nil, false
	}
	return ids, true
}

// parseTerminalIDs extracts the live terminal ids from a terminal.list result,
// unioning the structured payload and the JSON text body (Daintree returns results in
// text) so a divergence between the two can't drop an id — mirroring the other Daintree
// parsers. Each entry's id is "id" or, failing that, "terminalId". The second return is
// true iff a "terminals" array was actually present in either source (even if empty),
// so the caller can distinguish a genuine empty inventory from an unparseable response.
// Never throws.
func parseTerminalIDs(structured any, text string) (map[string]struct{}, bool) {
	out := map[string]struct{}{}
	sawArray := false
	collect := func(entries []any) {
		for _, e := range entries {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if id == "" {
				id, _ = m["terminalId"].(string)
			}
			if id != "" {
				out[id] = struct{}{}
			}
		}
	}
	if sc, ok := structured.(map[string]any); ok {
		if arr, ok := sc["terminals"].([]any); ok {
			sawArray = true
			collect(arr)
		}
	}
	if strings.TrimSpace(text) != "" {
		// *[]any (not []any): an absent "terminals" key leaves the pointer nil, while a
		// present array — even an empty one — yields a non-nil pointer, so we can tell
		// "present and empty" from "absent / not JSON".
		var parsed struct {
			Terminals *[]any `json:"terminals"`
		}
		if json.Unmarshal([]byte(text), &parsed) == nil && parsed.Terminals != nil {
			sawArray = true
			collect(*parsed.Terminals)
		}
	}
	return out, sawArray
}

// parseIDList decodes a JSON string-array column (terminalIdsJson) into a slice,
// dropping empties. Returns nil for a nil/blank/unparseable value.
func parseIDList(raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	var ids []string
	if json.Unmarshal([]byte(*raw), &ids) != nil {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}

// anyLive reports whether any id in the list is present in the live set.
func anyLive(ids []string, live map[string]struct{}) bool {
	for _, id := range ids {
		if _, ok := live[id]; ok {
			return true
		}
	}
	return false
}

// isTerminalWorkflowStatus reports whether a run status is terminal (done/cancelled/
// failed) and therefore not a reconcile candidate.
func isTerminalWorkflowStatus(s domain.WorkflowRunStatus) bool {
	switch s {
	case domain.WorkflowDone, domain.WorkflowCancelled, domain.WorkflowFailed:
		return true
	default:
		return false
	}
}

// maybeReconcileLedger runs the boot ledger reconcile exactly once per process — on
// the first successful MCP connect with a parseable live-terminal read. A mid-session
// /reconnect must NOT re-run it: it reads the LIVE terminal.list, so a re-run after the
// user ended a terminal in-session could wrongly expire a run still being worked. It is
// a no-op when not connected. The mutex serializes duplicate splash/bootstrap calls, and
// `done` is committed only after a completed attempt, so cancellation at any point in
// terminal.list remains retryable.
func (a *App) maybeReconcileLedger(ctx context.Context, connected bool) {
	if !connected {
		return
	}
	a.reconcileLedgerMu.Lock()
	defer a.reconcileLedgerMu.Unlock()
	if a.reconcileLedgerDone {
		return
	}
	_, completed := reconcileLedger(ctx, a.Store, a.MCP,
		debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir})
	if completed {
		a.reconcileLedgerDone = true
	}
}
