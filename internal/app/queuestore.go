package app

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/storage"
)

// queueEventStore implements queue.EventStore over the storage layer's `events`
// table. The dedupe-or-insert decision is delegated to storage.Store.UpsertEvent,
// which performs the lookup + bump/insert in ONE transaction (with the store's
// single writer connection that serializes like BEGIN IMMEDIATE) — so concurrent
// publishes of the same dedupeKey can never both miss and double-insert. The
// remaining methods reuse storage's coarse helpers; only the prior-row read (for
// the Queue's notification re-arm) is direct SQL against the same table/columns.
type queueEventStore struct{ s *storage.Store }

// eventCols mirrors storage.eventCols (unexported there) — the column order
// scanQueueEvent relies on.
const eventCols = `id,source,severity,title,summary,targetJson,evidenceJson,recommendedActionsJson,dedupeKey,epistemicKind,createdAt,updatedAt,notifiedAt,expiresAt,resolvedAt,count`

// UpsertEvent delegates the atomic dedupe-or-insert to storage and, for a
// dedupe-eligible publish, snapshots the PRIOR open row first so the Queue can
// decide notification re-arm. The prior read is advisory only — it informs the
// re-arm decision, NOT dedupe correctness (that is atomic inside
// storage.UpsertEvent). A stale prior at worst causes a spurious or missed re-arm
// (both benign: ClearNotified is idempotent and the next material change re-arms
// again), never a double-insert.
func (a queueEventStore) UpsertEvent(_ context.Context, args domain.QueuePublishArgs, now int64) (domain.QueueEvent, *domain.QueueEvent, error) {
	var prior *domain.QueueEvent
	if args.DedupeKey != "" {
		q := "SELECT " + eventCols + " FROM events WHERE dedupeKey = ? AND resolvedAt IS NULL " +
			"AND (expiresAt IS NULL OR expiresAt > ?) ORDER BY COALESCE(updatedAt, createdAt) DESC LIMIT 1"
		row := a.s.DB().QueryRow(q, args.DedupeKey, now)
		ev, err := scanQueueEvent(row)
		if err == nil {
			cp := ev
			prior = &cp
		} else if err != sql.ErrNoRows {
			return domain.QueueEvent{}, nil, err
		}
	}
	ev, err := a.s.UpsertEvent(args)
	if err != nil {
		return domain.QueueEvent{}, nil, err
	}
	// Only report a prior when the upsert actually bumped that row (same id). A
	// fresh insert (or a prior that was concurrently resolved) carries no prior.
	if prior != nil && prior.ID != ev.ID {
		prior = nil
	}
	return ev, prior, nil
}

// ClearNotified re-arms an event for the next notify() pass by nulling notifiedAt.
func (a queueEventStore) ClearNotified(_ context.Context, id string) error {
	_, err := a.s.DB().Exec("UPDATE events SET notifiedAt = NULL WHERE id = ?", id)
	return err
}

func (a queueEventStore) ListEvents(_ context.Context, opts domain.QueueDigestOptions, _ int64) ([]domain.QueueEvent, error) {
	return a.s.ListEvents(opts)
}

func (a queueEventStore) MarkNotified(_ context.Context, ids []string, ts int64) error {
	return a.s.MarkNotified(ids, ts)
}

func (a queueEventStore) ResolveEvent(_ context.Context, id string, _ int64) (bool, error) {
	return a.s.ResolveEvent(id)
}

// scanQueueEvent rebuilds a QueueEvent from the eventCols column order (a private
// mirror of storage.scanEvent, which is unexported).
type rowScanner interface{ Scan(dest ...any) error }

func scanQueueEvent(sc rowScanner) (domain.QueueEvent, error) {
	var e domain.QueueEvent
	var source, severity string
	var targetJson, evidenceJson, actionsJson, dedupeKey, epistemic sql.NullString
	var updatedAt, notifiedAt, expiresAt, resolvedAt sql.NullInt64
	if err := sc.Scan(&e.ID, &source, &severity, &e.Title, &e.Summary, &targetJson,
		&evidenceJson, &actionsJson, &dedupeKey, &epistemic, &e.CreatedAt, &updatedAt,
		&notifiedAt, &expiresAt, &resolvedAt, &e.Count); err != nil {
		return domain.QueueEvent{}, err
	}
	e.Source = domain.EventSource(source)
	e.Severity = domain.Severity(severity)
	if dedupeKey.Valid {
		e.DedupeKey = dedupeKey.String
	}
	if epistemic.Valid {
		e.EpistemicKind = domain.EpistemicKind(epistemic.String)
	}
	e.UpdatedAt = nullToI64Ptr(updatedAt)
	e.ExpiresAt = nullToI64Ptr(expiresAt)
	e.ResolvedAt = nullToI64Ptr(resolvedAt)
	_ = notifiedAt
	if targetJson.Valid && targetJson.String != "" {
		var t domain.EventTarget
		if json.Unmarshal([]byte(targetJson.String), &t) == nil {
			e.Target = &t
		}
	}
	if evidenceJson.Valid && evidenceJson.String != "" {
		_ = json.Unmarshal([]byte(evidenceJson.String), &e.Evidence)
	}
	if actionsJson.Valid && actionsJson.String != "" {
		_ = json.Unmarshal([]byte(actionsJson.String), &e.RecommendedActions)
	}
	return e, nil
}

// --- SQL null helpers ---

func nullToI64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
