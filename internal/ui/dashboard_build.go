package ui

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// dashboard_build.go builds an operations snapshot off the loop (called from a
// tea.Cmd so Update stays cheap). It reads timers/watchers/launches/audit from the
// store and the inbox from the queue digest, drives terminal previews through the
// daemon's read-only MCP seam (throttled to PreviewPollMS), then folds watchers +
// the durable spawn roster into agent rows.

// dashboardBuildOptions carries the off-loop preview state the build needs but the
// pure Model/Update must not compute itself: the read-only MCP seam, the clock
// (NowMS), the last successful poll time, and the cached tails to reuse between polls.
type dashboardBuildOptions struct {
	MCP                  daemon.MCP
	NowMS                int64
	LastPreviewFetchedAt int64
	CachedPreviews       []daemon.TerminalPreview
}

// dashboardBuildResult is the snapshot plus the preview-throttle state handed back to
// the Model. Previews are the tails in effect this build (freshly polled OR reused);
// FetchedAt is opts.NowMS when a real MCP poll ran and 0 when the cache was reused, so
// Update can advance lastPreviewFetchedAt only on an actual fetch.
type dashboardBuildResult struct {
	Dashboard Dashboard
	Previews  []daemon.TerminalPreview
	FetchedAt int64
}

// buildDashboard reads the current operations state. Errors degrade to empty
// sections (the deck simply shows less) — a snapshot build must never break the UI.
func buildDashboard(ctx context.Context, a *app.App, opts dashboardBuildOptions) dashboardBuildResult {
	var d Dashboard

	if timers, err := a.Store.ListTimers("scheduled"); err == nil {
		d.Timers = timers
	}
	var watchers []domain.WatcherRecord
	if ws, err := a.Store.ListWatchers(""); err == nil {
		watchers = ws
		d.Watchers = ws
	}
	// The durable spawn roster keeps agents on the deck after their session-scoped
	// watcher is cancelled/completed (prior-session non-terminal rows are already
	// failed on DB open, so this stays within-session honest).
	if launches, err := a.Store.ListAgentLaunches(20); err == nil {
		d.Launches = launches
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

	// Terminal previews: build the target set from live terminal watchers plus any
	// roster terminal not already covered by a watcher, then EITHER poll MCP (first
	// build, or when the PreviewPollMS gate has elapsed and the link is up) OR reuse
	// the cached tails.
	targets := daemon.BuildPreviewTargets(previewWatchers(watchers, d.Launches))
	previews, fetchedAt := resolvePreviews(ctx, opts, targets)

	d.Agents = BuildAgentRows(watchers, previews, d.Launches)
	return dashboardBuildResult{Dashboard: d, Previews: previews, FetchedAt: fetchedAt}
}

// resolvePreviews returns the terminal previews in effect this build and the fetch
// timestamp. The deck ticks ~1s but previews refresh at PreviewPollMS, so it polls MCP
// on the first build (LastPreviewFetchedAt == 0) or when the gate has elapsed and the
// link is up; otherwise it reuses the cached tails filtered to the still-active targets.
// fetchedAt is opts.NowMS on a real poll and 0 on reuse, so the caller advances
// lastPreviewFetchedAt only on an actual fetch. The first-build fetch ensures fresh
// starts: prior-session dead terminals are detected immediately rather than lingering
// until the gate elapses.
// Split out from buildDashboard so the throttle gate is unit-testable without a Store.
func resolvePreviews(ctx context.Context, opts dashboardBuildOptions, targets []daemon.PreviewTarget) ([]daemon.TerminalPreview, int64) {
	if len(targets) == 0 {
		return nil, 0
	}
	// First build or gate elapsed: fetch fresh previews from MCP.
	if opts.MCP != nil && opts.MCP.Connected() &&
		(opts.LastPreviewFetchedAt == 0 || opts.NowMS-opts.LastPreviewFetchedAt >= daemon.PreviewPollMS) {
		return daemon.FetchPreviews(ctx, opts.MCP, targets, opts.NowMS), opts.NowMS
	}
	return filterPreviews(opts.CachedPreviews, targets), 0
}

// previewWatchers builds the preview-target source set: every live terminal watcher
// (owning its terminals' cards), plus any roster launch whose terminal is not already
// covered by a watcher — so a cancelled-watcher agent still on the roster keeps a live
// preview card. Cross-owner terminal dedupe + the MaxTerminals cap are applied by
// daemon.BuildPreviewTargets.
func previewWatchers(watchers []domain.WatcherRecord, launches []domain.AgentLaunchRecord) []daemon.PreviewWatcher {
	out := make([]daemon.PreviewWatcher, 0, len(watchers)+len(launches))
	covered := make(map[string]bool)
	for _, w := range watchers {
		// Only live terminal watchers own preview cards. A terminal-status watcher
		// (incl. a prior-session one cancelled on DB open) would otherwise burn a
		// MaxTerminals slot polling a dead terminal and starve a live launch's card.
		if w.Kind != "terminal" || !liveWatcherStatus(w.Status) {
			continue
		}
		ids := terminalIDs(w.TargetsJson)
		for _, id := range ids {
			if id != "" {
				covered[id] = true
			}
		}
		out = append(out, daemon.PreviewWatcher{ID: w.ID, Title: w.Title, TerminalIDs: ids})
	}
	for _, l := range launches {
		if l.TerminalID == nil || *l.TerminalID == "" || covered[*l.TerminalID] {
			continue
		}
		covered[*l.TerminalID] = true
		owner := l.ID
		if l.WatcherID != nil && *l.WatcherID != "" {
			owner = *l.WatcherID
		}
		out = append(out, daemon.PreviewWatcher{
			ID:          owner,
			Title:       firstNonEmpty(l.Title, l.Name),
			TerminalIDs: []string{*l.TerminalID},
		})
	}
	return out
}

// filterPreviews keeps only cached previews whose terminal is still an active target,
// so a card for a terminal that has since closed (dropped from the target set) stops
// showing immediately rather than lingering until the next poll.
func filterPreviews(cached []daemon.TerminalPreview, targets []daemon.PreviewTarget) []daemon.TerminalPreview {
	if len(cached) == 0 || len(targets) == 0 {
		return nil
	}
	live := make(map[string]bool, len(targets))
	for _, t := range targets {
		live[t.TerminalID] = true
	}
	out := make([]daemon.TerminalPreview, 0, len(cached))
	for _, p := range cached {
		if live[p.TerminalID] {
			out = append(out, p)
		}
	}
	return out
}
