package ui

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// dashboard_build.go builds an operations snapshot off the loop (called from a
// tea.Cmd so Update stays cheap). It reads timers/watchers/audit from the store and
// the inbox from the queue digest, then folds watchers into agent rows.

// buildDashboard reads the current operations state. Errors degrade to empty
// sections (the deck simply shows less) — a snapshot build must never break the UI.
func buildDashboard(ctx context.Context, a *app.App) Dashboard {
	var d Dashboard

	if timers, err := a.Store.ListTimers("scheduled"); err == nil {
		d.Timers = timers
	}
	if watchers, err := a.Store.ListWatchers(""); err == nil {
		d.Watchers = watchers
		d.Agents = BuildAgentRows(watchers)
	}
	if audit, err := a.Store.ListAudit(8); err == nil {
		d.Audit = audit
	}
	// Inbox: actionable severities only (severityAtLeast=attention), capped.
	atLeast := domain.SeverityAttention
	maxItems := 30
	if inbox, err := a.Queue.Digest(ctx, domain.QueueDigestOptions{
		SeverityAtLeast: &atLeast,
		MaxItems:        &maxItems,
	}); err == nil {
		d.Inbox = inbox
	}
	return d
}
