// Package queue implements the attention queue: every sub-thread (timers,
// watchers, workflows, model workers) reports here instead of interrupting the
// main thread. Events are deduplicated by dedupeKey and surfaced via the /inbox
// digest. Port of src/queue.ts plus the queue-relevant pieces of
// src/storage/db.ts (upsertEvent / listEvents / resolveEvent / markNotified and
// the severity ordering). Spec: docs/port/domain-config.md §6.
//
// The persistence of the `events` table is delegated to a consumer-defined
// EventStore interface (declared here, in the consumer, NOT in internal/ports —
// the package must compile in isolation against the storage seam without an
// import cycle). All the dedupe/expiry/severity/ordering decisions live in this
// package; EventStore only does row-level CRUD.
package queue

import (
	"context"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ports"
)

// Queue implements the ports.Queue seam (Publish/Digest/Resolve) so the agent
// loop can depend on the interface without importing this package.
var _ ports.Queue = (*Queue)(nil)

// EventStore is the consumer-defined persistence seam for the `events` table.
// It is intentionally small: the Queue owns all surfacing/re-arm policy and
// delegates the dedupe-or-insert decision to a single ATOMIC primitive. A
// concrete internal/storage implementation satisfies it; tests use an in-memory
// fake.
//
// `now` is threaded in (rather than read from the clock inside the store) so the
// Queue's expiry/digest decisions are deterministic and testable.
type EventStore interface {
	// UpsertEvent atomically dedupes-or-inserts an event. When args.DedupeKey is
	// set and an OPEN (resolvedAt NULL AND (expiresAt NULL OR > now)) row with
	// that key exists, it BUMPS the newest such row in a SINGLE transaction:
	// count+=1, refresh title/summary/severity, OVERWRITE recommendedActions (nil
	// clears to NULL), fall back evidence/epistemicKind to the existing row when
	// the publish omits them, advance updatedAt, refresh expiresAt — but NEVER
	// touch createdAt. Otherwise it inserts a fresh row. It returns the resulting
	// event AND the PRIOR row (the pre-bump snapshot, nil on a fresh insert) so the
	// Queue can decide notification re-arm without a second non-atomic read. The
	// dedupe lookup + update/insert MUST be atomic so concurrent publishes of the
	// same key can never double-insert.
	UpsertEvent(ctx context.Context, args domain.QueuePublishArgs, now int64) (ev domain.QueueEvent, prior *domain.QueueEvent, err error)

	// ClearNotified resets notifiedAt to NULL for the id, re-arming it for the
	// next notify() pass. Idempotent: clearing an already-NULL row is a no-op.
	ClearNotified(ctx context.Context, id string) error

	// ListEvents returns rows matching the digest filter, already ORDER BY
	// severity-rank DESC, COALESCE(updatedAt,createdAt) DESC and LIMIT-applied.
	// `now` bounds the expiry predicate. The store is responsible for the SQL
	// (the SEV_CASE expression is mirrored by SeverityCaseSQL in this package).
	ListEvents(ctx context.Context, opts domain.QueueDigestOptions, now int64) ([]domain.QueueEvent, error)

	// MarkNotified stamps notifiedAt=ts on each id (no-op on empty input).
	MarkNotified(ctx context.Context, ids []string, ts int64) error

	// ResolveEvent stamps resolvedAt=now WHERE id=? AND resolvedAt IS NULL and
	// reports whether a row changed (false ⇒ already resolved / absent).
	ResolveEvent(ctx context.Context, id string, now int64) (bool, error)
}

// Clock returns the current epoch-ms. Injected for deterministic tests; defaults
// to domain.NowMS.
type Clock func() int64

// Queue is the attention queue over an EventStore.
type Queue struct {
	store EventStore
	now   Clock
}

// New builds a Queue over the given store. A nil clock defaults to the wall
// clock (domain.NowMS).
func New(store EventStore, now Clock) *Queue {
	if now == nil {
		now = domain.NowMS
	}
	return &Queue{store: store, now: now}
}

// Publish adds or dedupes an event and returns the resulting QueueEvent.
//
// The dedupe-or-insert decision is delegated to the store's ATOMIC UpsertEvent so
// concurrent publishes of the same dedupeKey can never both miss the lookup and
// double-insert (the old FindOpenByDedupe-then-Insert/Bump split had exactly that
// race). The store keeps createdAt pinned on a bump — the scheduler's "is this
// new?" check keys on createdAt, so refreshing it would re-notify a recurring
// event every tick. evidence/epistemicKind fall back to the existing row inside
// the store when the publish omits them; recommendedActions are overwritten
// outright (cleared to NULL when none).
//
// Notification RE-ARM (§ stable-dedupe): a deduped event keeps its notifiedAt on a
// bump, so once delivered it never re-surfaces — correct for an identical repeat
// (a "still working" tick must not re-interrupt), WRONG for a material change (a
// severity escalation or a classification/title change the human needs to see).
// On a MATERIAL transition we clear notifiedAt so the next notify() re-delivers;
// an identical bump leaves it stamped. The prior snapshot comes back from the same
// atomic upsert, so the decision needs no extra read.
func (q *Queue) Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	now := q.now()

	ev, prior, err := q.store.UpsertEvent(ctx, args, now)
	if err != nil {
		return domain.QueueEvent{}, err
	}

	// Re-arm only on a bump (prior != nil) that materially changed the event.
	if prior != nil && materialNotificationChange(*prior, ev) {
		if err := q.store.ClearNotified(ctx, ev.ID); err != nil {
			return domain.QueueEvent{}, err
		}
	}
	return ev, nil
}

// materialNotificationChange reports whether a dedupe bump changed the event in a
// way that warrants re-notifying the human. A repeated identical bump (the common
// "still working" / unchanged-watcher case) is NOT material — re-arming on every
// bump would defeat dedupe and re-interrupt every tick. Material transitions:
//   - severity ESCALATED (rank rose) — a watcher going attention→blocked/urgent;
//   - the classification's title changed (the watcher Title encodes the
//     classification, e.g. "X: waiting for input" → "X: tests failed");
//   - the summary changed materially (new evidence the human hasn't seen).
//
// A severity DE-escalation is deliberately not material: a thing getting less
// urgent need not re-interrupt.
func materialNotificationChange(prior, next domain.QueueEvent) bool {
	if domain.RankOf(next.Severity) > domain.RankOf(prior.Severity) {
		return true
	}
	if next.Title != prior.Title {
		return true
	}
	if next.Summary != prior.Summary {
		return true
	}
	return false
}

// Digest returns the ordered, filtered open events (§6.5).
func (q *Queue) Digest(ctx context.Context, opts domain.QueueDigestOptions) ([]domain.QueueEvent, error) {
	return q.store.ListEvents(ctx, opts, q.now())
}

// Resolve stamps an event resolved, reporting whether a row changed (false ⇒ it
// was already resolved or does not exist).
func (q *Queue) Resolve(ctx context.Context, id string) (bool, error) {
	return q.store.ResolveEvent(ctx, id, q.now())
}

// MarkNotified records that the given events have been pushed to the attention
// notifier so they are not re-notified. No-op on empty input.
func (q *Queue) MarkNotified(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return q.store.MarkNotified(ctx, ids, q.now())
}

// severityIcon maps a severity to its /inbox glyph. Unknown severities fall back
// to the info glyph (mirroring the rank ELSE-1 fallback).
var severityIcon = map[domain.Severity]string{
	domain.SeverityDebug:     "·",
	domain.SeverityInfo:      "ℹ",
	domain.SeverityDone:      "✓",
	domain.SeverityAttention: "!",
	domain.SeverityBlocked:   "⛔",
	domain.SeverityUrgent:    "‼",
	domain.SeverityError:     "✗",
}

// Format renders a compact, human/LLM-facing /inbox digest. Glyphs and layout
// are a product contract — preserve them exactly (§6.1). Empty ⇒ "Inbox is
// empty.".
func (q *Queue) Format(events []domain.QueueEvent) string {
	return Format(events)
}

// Format is the package-level digest formatter (also usable without a Queue).
func Format(events []domain.QueueEvent) string {
	if len(events) == 0 {
		return "Inbox is empty."
	}
	lines := make([]string, 0, len(events))
	for _, e := range events {
		// Target suffix: terminal wins over worktree; else nothing.
		target := ""
		if e.Target != nil {
			if e.Target.TerminalID != "" {
				target = " [term " + e.Target.TerminalID + "]"
			} else if e.Target.WorktreeID != "" {
				target = " [wt " + e.Target.WorktreeID + "]"
			}
		}
		dup := ""
		if e.Count > 1 {
			dup = fmt.Sprintf(" (×%d)", e.Count)
		}
		ev := ""
		if len(e.Evidence) > 0 {
			ev = "\n     evidence: " + strings.Join(e.Evidence, " | ")
		}
		icon, ok := severityIcon[e.Severity]
		if !ok {
			icon = severityIcon[domain.SeverityInfo]
		}
		lines = append(lines,
			fmt.Sprintf("  %s %s %s%s%s\n     %s%s", icon, e.ID, e.Title, target, dup, e.Summary, ev))
	}
	return strings.Join(lines, "\n")
}
