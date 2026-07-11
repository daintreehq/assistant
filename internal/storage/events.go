package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

const eventCols = `id,source,severity,title,summary,targetJson,evidenceJson,recommendedActionsJson,dedupeKey,epistemicKind,createdAt,updatedAt,notifiedAt,expiresAt,resolvedAt,count`

// scanEvent rebuilds a QueueEvent, parsing the target/evidence/recommendedActions
// JSON columns (the store owns these, unlike the raw *Json passthrough columns).
func scanEvent(sc scanner) (domain.QueueEvent, error) {
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
	e.UpdatedAt = i64FromNull(updatedAt)
	e.ExpiresAt = i64FromNull(expiresAt)
	e.ResolvedAt = i64FromNull(resolvedAt)
	_ = notifiedAt // QueueEvent carries no NotifiedAt field; the column is scanned for completeness
	if targetJson.Valid && targetJson.String != "" {
		var t domain.EventTarget
		if err := json.Unmarshal([]byte(targetJson.String), &t); err == nil {
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

// UpsertEvent inserts a new attention-queue event, or — when ev.DedupeKey is set
// and an OPEN row (resolvedAt NULL AND (expiresAt NULL OR > now)) with the same
// key exists (newest first) — collapses into it. KEEP dedupe semantics:
// on a hit bump count, refresh title/summary/severity/
// epistemicKind, OVERWRITE recommendedActions outright (clear to NULL if none),
// fall back to existing evidence/epistemicKind when omitted, bump updatedAt and
// refresh expiresAt — but NEVER touch createdAt (the scheduler's "is new?" keys on
// createdAt).
func (s *Store) UpsertEvent(args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	now := s.now()
	var expiresAt *int64
	// A non-positive TTL is treated as "no expiry" (a 0/negative ttlMs would otherwise set
	// expiresAt <= now, making the event INSTANTLY invisible to ListEvents). A huge TTL is
	// clamped so now+ttl can't overflow int64 into a negative (also instantly-expired) value.
	if args.TTLMs != nil && *args.TTLMs > 0 {
		ttl := *args.TTLMs
		if ttl > math.MaxInt64-now {
			ttl = math.MaxInt64 - now
		}
		ea := now + ttl
		expiresAt = &ea
	}
	targetJson := marshalPtr(args.Target)
	evidenceJson := marshalSlice(args.Evidence)
	actionsJson := marshalActions(args.RecommendedActions)

	// Wrap the whole dedupe lookup + update/insert + reload in ONE transaction so a
	// concurrent first-publish can't double-insert and a concurrent ResolveEvent
	// can't bump a row we just resolved (the lookup is "newest OPEN row"). With
	// SetMaxOpenConns(1) the tx holds the sole connection for its full lifetime, so
	// it serializes like BEGIN IMMEDIATE — never call s.db / s.GetEvent inside it
	// (that would block on the same single connection and deadlock).
	tx, err := s.db.Begin()
	if err != nil {
		return domain.QueueEvent{}, fmt.Errorf("begin upsert event: %w", err)
	}

	if args.DedupeKey != "" {
		// Find newest open row with this dedupe key (tx-scoped).
		row := tx.QueryRow(
			"SELECT "+eventCols+` FROM events
			 WHERE dedupeKey = ? AND resolvedAt IS NULL
			   AND (expiresAt IS NULL OR expiresAt > ?)
			 ORDER BY COALESCE(updatedAt, createdAt) DESC LIMIT 1`,
			args.DedupeKey, now)
		existing, lerr := scanEvent(row)
		if lerr == nil {
			// Evidence/epistemicKind fall back to existing when omitted.
			newEvidence := evidenceJson
			if len(args.Evidence) == 0 && len(existing.Evidence) > 0 {
				newEvidence = marshalSlice(existing.Evidence)
			}
			newEpistemic := string(args.EpistemicKind)
			if args.EpistemicKind == "" {
				newEpistemic = string(existing.EpistemicKind)
			}
			if _, uerr := tx.Exec(`
				UPDATE events
				   SET count = count + 1,
				       title = ?, summary = ?, severity = ?,
				       epistemicKind = ?,
				       recommendedActionsJson = ?,
				       evidenceJson = ?,
				       targetJson = ?,
				       updatedAt = ?, expiresAt = ?
				 WHERE id = ?`,
				args.Title, args.Summary, string(args.Severity),
				nullIfEmpty(newEpistemic), actionsJson, newEvidence, targetJson,
				now, nullI64(expiresAt), existing.ID); uerr != nil {
				_ = tx.Rollback()
				return domain.QueueEvent{}, fmt.Errorf("dedupe-update event: %w", uerr)
			}
			updated, gerr := scanEvent(tx.QueryRow(
				"SELECT "+eventCols+" FROM events WHERE id = ?", existing.ID))
			if gerr != nil {
				_ = tx.Rollback()
				return domain.QueueEvent{}, fmt.Errorf("reload deduped event: %w", gerr)
			}
			if err := tx.Commit(); err != nil {
				_ = tx.Rollback()
				return domain.QueueEvent{}, fmt.Errorf("commit upsert event: %w", err)
			}
			return updated, nil
		}
		if !errors.Is(lerr, sql.ErrNoRows) {
			_ = tx.Rollback()
			return domain.QueueEvent{}, fmt.Errorf("dedupe lookup: %w", lerr)
		}
		// fallthrough to INSERT
	}

	id := domain.NewID(domain.PrefixEvent)
	if _, ierr := tx.Exec(`
		INSERT INTO events
		  (id,source,severity,title,summary,targetJson,evidenceJson,
		   recommendedActionsJson,dedupeKey,epistemicKind,createdAt,updatedAt,
		   notifiedAt,expiresAt,resolvedAt,count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,NULL,1)`,
		id, string(args.Source), string(args.Severity), args.Title, args.Summary,
		targetJson, evidenceJson, actionsJson, nullIfEmpty(args.DedupeKey),
		nullIfEmpty(string(args.EpistemicKind)), now, now, nullI64(expiresAt)); ierr != nil {
		_ = tx.Rollback()
		return domain.QueueEvent{}, fmt.Errorf("insert event: %w", ierr)
	}
	out, oerr := scanEvent(tx.QueryRow("SELECT "+eventCols+" FROM events WHERE id = ?", id))
	if oerr != nil {
		_ = tx.Rollback()
		return domain.QueueEvent{}, fmt.Errorf("reload inserted event: %w", oerr)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return domain.QueueEvent{}, fmt.Errorf("commit upsert event: %w", err)
	}
	return out, nil
}

// GetEvent returns an event by id, or (nil, nil) when absent.
func (s *Store) GetEvent(id string) (*domain.QueueEvent, error) {
	row := s.db.QueryRow("SELECT "+eventCols+" FROM events WHERE id = ?", id)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get event: %w", err)
	}
	return &e, nil
}

// ListEvents returns open (or, with IncludeResolved, all non-expired) events
// ordered by severity rank DESC then recency (COALESCE(updatedAt,createdAt)) DESC.
func (s *Store) ListEvents(opts domain.QueueDigestOptions) ([]domain.QueueEvent, error) {
	now := s.now()
	conds := []string{"(expiresAt IS NULL OR expiresAt > ?)"}
	args := []any{now}
	if !opts.IncludeResolved {
		conds = append(conds, "resolvedAt IS NULL")
	}
	if opts.NotifiedIsNull {
		conds = append(conds, "notifiedAt IS NULL")
	}
	if opts.SeverityAtLeast != nil {
		conds = append(conds, sevCase+" >= ?")
		args = append(args, domain.RankOf(*opts.SeverityAtLeast))
	}
	q := "SELECT " + eventCols + " FROM events WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY " + sevCase + " DESC, COALESCE(updatedAt, createdAt) DESC"
	if opts.MaxItems != nil {
		// Clamp like the memory/audit lists: a negative MaxItems becomes SQLite
		// LIMIT -1 (unlimited) and a huge one materializes a huge result, so bound
		// it to [1,1000].
		q += fmt.Sprintf(" LIMIT %d", clamp(*opts.MaxItems, 1, 1000))
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []domain.QueueEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkNotified stamps notifiedAt on each digested event (ts defaults to now when
// <=0) — VERSION-CONDITIONALLY: a row is stamped only while it still matches the
// snapshot that was actually delivered (same count, same
// COALESCE(updatedAt,createdAt), notifiedAt still NULL). A publisher that
// materially updates an event between the notifier's Digest read and this
// acknowledgement bumps count/updatedAt (and re-arms notifiedAt), so the
// condition fails, the row stays unnotified, and the NEXT notify pass re-digests
// and delivers the update. Without the condition the update would be stamped
// notified without the human ever seeing it.
func (s *Store) MarkNotified(evs []domain.QueueEvent, ts int64) error {
	// ts<=0 ⇒ "use the store clock". Epoch-ms 0 is 1970 and never a real stamp, so
	// the sentinel can't collide with a legitimate caller-supplied time.
	if ts <= 0 {
		ts = s.now()
	}
	for _, e := range evs {
		version := e.CreatedAt
		if e.UpdatedAt != nil {
			version = *e.UpdatedAt
		}
		if _, err := s.db.Exec(
			`UPDATE events SET notifiedAt = ?
			  WHERE id = ? AND notifiedAt IS NULL AND count = ?
			    AND COALESCE(updatedAt, createdAt) = ?`,
			ts, e.ID, e.Count, version); err != nil {
			return fmt.Errorf("mark notified: %w", err)
		}
	}
	return nil
}

// ClearNotified nulls notifiedAt for each id, re-arming the events so the next
// notify pass digests and delivers them again. Idempotent (an already-NULL row is
// a no-op). Two callers: the queue's material-change re-arm on a dedupe bump, and
// the host's shutdown-cancelled-wake re-arm (a burst handed to a dying process
// was already stamped notified; without the re-arm it would never be delivered).
func (s *Store) ClearNotified(ids []string) error {
	for _, id := range ids {
		if _, err := s.db.Exec("UPDATE events SET notifiedAt = NULL WHERE id = ?", id); err != nil {
			return fmt.Errorf("clear notified: %w", err)
		}
	}
	return nil
}

// ResolveOpenEventsByDedupeKey resolves every still-open event carrying dedupeKey
// (in practice at most one — UpsertEvent coalesces onto the newest open row per
// key). Used when a supervisor watcher is retired because the main turn consumed
// its terminal's completion directly: if the watcher had already published (the
// narrow race where its check beat the in-turn settle), that inbox item is now a
// stale duplicate of what the conversation already contains, so it is resolved
// the same way the model would resolve it by hand. Returns the count resolved.
func (s *Store) ResolveOpenEventsByDedupeKey(key string) (int, error) {
	if key == "" {
		return 0, nil // never mass-resolve keyless events
	}
	res, err := s.db.Exec("UPDATE events SET resolvedAt = ? WHERE dedupeKey = ? AND resolvedAt IS NULL",
		s.now(), key)
	if err != nil {
		return 0, fmt.Errorf("resolve events by dedupe key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("resolve events by dedupe key rows affected: %w", err)
	}
	return int(n), nil
}

// ResolveEvent stamps resolvedAt WHERE resolvedAt IS NULL; reports whether a row
// changed (an already-resolved event returns false).
func (s *Store) ResolveEvent(id string) (bool, error) {
	res, err := s.db.Exec("UPDATE events SET resolvedAt = ? WHERE id = ? AND resolvedAt IS NULL",
		s.now(), id)
	if err != nil {
		return false, fmt.Errorf("resolve event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("resolve event rows affected: %w", err)
	}
	return n > 0, nil
}

// ResolveAllOpenEvents resolves EVERY currently-open event (resolvedAt IS NULL),
// stamping `now`. This is the clean-slate inbox wipe: on session open and on /clear,
// every prior attention item is cleared so a run starts with an empty inbox (the !N
// badge at 0). Idempotent — an already-resolved event keeps its original stamp.
// Events published THIS session after the call are unaffected. Returns the count
// resolved.
func (s *Store) ResolveAllOpenEvents(now int64) (int, error) {
	res, err := s.db.Exec("UPDATE events SET resolvedAt = ? WHERE resolvedAt IS NULL", now)
	if err != nil {
		return 0, fmt.Errorf("resolve all open events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("resolve all open events rows affected: %w", err)
	}
	return int(n), nil
}

// ---- small JSON marshal helpers for the store-owned columns ----

func marshalPtr(t *domain.EventTarget) any {
	if t == nil {
		return nil
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil
	}
	return string(b)
}

func marshalSlice(v []string) any {
	if len(v) == 0 {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

func marshalActions(v []domain.RecommendedAction) any {
	if len(v) == 0 {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
