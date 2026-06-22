package agenttaskx

import (
	"context"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// bootReconcileLimit caps how many confirmed sagas boot reconciliation inspects.
// Comfortably above any realistic count of concurrently-supervised agents in one
// prior session, but bounded so the join can never run away.
const bootReconcileLimit = 50

// BootStore is the slice of storage boot reconciliation reads: the confirmed spawn
// sagas that bound a terminal in a prior session (the survivors of the session-end
// sweep). *storage.Store satisfies it by structural match.
type BootStore interface {
	ListConfirmedAgentLaunchesWithTerminal(limit int) ([]domain.AgentLaunchRecord, error)
}

// BootQueue is the attention-queue slice boot reconciliation publishes to. It mirrors
// ports.Queue.Publish so *queue.Queue satisfies it without a wrapper.
type BootQueue interface {
	Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
}

// BootReconcile re-surfaces prior-session agents whose supervision did not survive
// the session boundary. On open the storage sweep cancels every watcher and fails
// every non-terminal saga — but a 'confirmed' saga that bound a terminalId means a
// VISIBLE agent was launched last session and may still be running, now unsupervised.
// This cross-joins those sagas against the LIVE terminal.list and publishes ONE
// deduped attention event per still-running orphan, recommending re-attach via
// agentTask.superviseTerminal. The dedupe key is per-terminal, so a relaunch before
// the user acts collapses to a single open event rather than spamming.
//
// MUST be called on interactive boot ONLY (REPL / cockpit) — never RunOneShot, which
// has no scheduler to deliver the events. Read-only and strictly best-effort: a
// disconnected MCP, an empty terminal list, a terminal.list error, or an absent
// dependency all return nil (boot must never block on or fail from reconciliation);
// only a store read or a queue publish surfaces an error, for logging.
func BootReconcile(ctx context.Context, mcp MCPClient, store BootStore, q BootQueue) error {
	if mcp == nil || store == nil || q == nil || !mcp.Connected() {
		return nil
	}

	launches, err := store.ListConfirmedAgentLaunchesWithTerminal(bootReconcileLimit)
	if err != nil {
		return err
	}
	if len(launches) == 0 {
		return nil
	}

	// Liveness gate: only surface terminals Daintree still reports as present, so we
	// never recommend adopting an agent that already exited.
	res, err := mcp.CallTool(ctx, "terminal.list", map[string]any{})
	if err != nil || res.IsError {
		return nil
	}
	live := map[string]bool{}
	for _, t := range parseTerminalList(res) {
		live[t.id] = true
	}
	if len(live) == 0 {
		return nil
	}

	var firstErr error
	for i := range launches {
		l := &launches[i]
		if l.TerminalID == nil || *l.TerminalID == "" {
			continue
		}
		tid := *l.TerminalID
		if !live[tid] {
			continue
		}
		args := domain.QueuePublishArgs{
			Source:   domain.SourceSystem,
			Severity: domain.SeverityAttention,
			Title:    "Unsupervised agent from a previous session: " + l.Title,
			Summary: fmt.Sprintf("Terminal %s (%s) is still running, but its supervision ended when the previous "+
				"session closed. Re-attach a supervisor to resume verification, or close the terminal if the work is done.",
				tid, l.AgentID),
			Target:        &domain.EventTarget{TerminalID: tid, WorktreeID: orStr(l.WorktreeID, "")},
			DedupeKey:     "orphan-terminal:" + tid,
			EpistemicKind: domain.EpistemicInferred,
			RecommendedActions: []domain.RecommendedAction{{
				Label:                "Re-attach supervision",
				ToolName:             "agentTask.superviseTerminal",
				Args:                 map[string]any{"terminalId": tid},
				Risk:                 domain.RiskTerminal,
				RequiresConfirmation: true,
			}},
		}
		if _, perr := q.Publish(ctx, args); perr != nil && firstErr == nil {
			firstErr = perr
		}
	}
	return firstErr
}
