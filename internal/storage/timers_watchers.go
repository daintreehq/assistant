package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// ---- timers ----

// InsertTimer inserts a timer, defaulting id (tmr_), runCount 0, status
// 'scheduled', createdAt now (when caller leaves them zero/empty).
func (s *Store) InsertTimer(rec domain.TimerRecord) (domain.TimerRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixTimer)
	}
	if rec.Status == "" {
		rec.Status = "scheduled"
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	_, err := s.db.Exec(`
		INSERT INTO timers
		  (id,title,fireAt,repeatEveryMs,repeatUntil,maxRuns,runCount,payloadType,
		   payloadJson,targetJson,status,createdAt,lastFiredAt)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Title, rec.FireAt, nullI64(rec.RepeatEveryMs), nullI64(rec.RepeatUntil),
		nullInt(rec.MaxRuns), rec.RunCount, rec.PayloadType, rec.PayloadJson,
		nullStr(rec.TargetJson), rec.Status, rec.CreatedAt, nullI64(rec.LastFiredAt))
	if err != nil {
		return domain.TimerRecord{}, fmt.Errorf("insert timer: %w", err)
	}
	return rec, nil
}

const timerCols = `id,title,fireAt,repeatEveryMs,repeatUntil,maxRuns,runCount,payloadType,payloadJson,targetJson,status,createdAt,lastFiredAt`

func scanTimer(sc scanner) (domain.TimerRecord, error) {
	var t domain.TimerRecord
	var repeatEvery, repeatUntil, lastFired sql.NullInt64
	var maxRuns sql.NullInt64
	var targetJson sql.NullString
	if err := sc.Scan(&t.ID, &t.Title, &t.FireAt, &repeatEvery, &repeatUntil, &maxRuns,
		&t.RunCount, &t.PayloadType, &t.PayloadJson, &targetJson, &t.Status, &t.CreatedAt,
		&lastFired); err != nil {
		return domain.TimerRecord{}, err
	}
	t.RepeatEveryMs = i64FromNull(repeatEvery)
	t.RepeatUntil = i64FromNull(repeatUntil)
	t.MaxRuns = intFromNull(maxRuns)
	t.TargetJson = strFromNull(targetJson)
	t.LastFiredAt = i64FromNull(lastFired)
	return t, nil
}

// GetTimer returns a timer by id, or (nil, nil) when absent.
func (s *Store) GetTimer(id string) (*domain.TimerRecord, error) {
	row := s.db.QueryRow("SELECT "+timerCols+" FROM timers WHERE id = ?", id)
	t, err := scanTimer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get timer: %w", err)
	}
	return &t, nil
}

// ListTimers returns all timers (status=="" ) or those of a status, ORDER BY fireAt.
func (s *Store) ListTimers(status string) ([]domain.TimerRecord, error) {
	q := "SELECT " + timerCols + " FROM timers"
	var args []any
	if status != "" {
		q += " WHERE status = ?"
		args = append(args, status)
	}
	q += " ORDER BY fireAt"
	return queryTimers(s.db, q, args...)
}

// DueTimers returns scheduled timers with fireAt <= now, ORDER BY fireAt.
func (s *Store) DueTimers(now int64) ([]domain.TimerRecord, error) {
	return queryTimers(s.db,
		"SELECT "+timerCols+" FROM timers WHERE status='scheduled' AND fireAt <= ? ORDER BY fireAt",
		now)
}

func queryTimers(db *sql.DB, q string, args ...any) ([]domain.TimerRecord, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query timers: %w", err)
	}
	defer rows.Close()
	var out []domain.TimerRecord
	for rows.Next() {
		t, err := scanTimer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// timerUpdateCols are the mutable columns (id/createdAt immutable).
var timerUpdateCols = newColSet("title", "fireAt", "repeatEveryMs", "repeatUntil",
	"maxRuns", "runCount", "payloadType", "payloadJson", "targetJson", "status", "lastFiredAt")

// UpdateTimer applies an allowlisted patch (no-op when no allowed key is set).
func (s *Store) UpdateTimer(id string, patch map[string]any) error {
	return s.applyUpdate("timers", timerUpdateCols, id, patch)
}

// ClaimDueTimer atomically applies `patch` to a timer ONLY while it is still the due row the
// scheduler read — status 'scheduled' AND fireAt == expectFireAt. It returns true iff a row
// was updated. A false return means the main turn cancelled, rescheduled, or edited the timer
// since DueTimers: the scheduler must then SKIP firing it and never write it back (which would
// RESURRECT a just-cancelled timer). Finalizing via this claim BEFORE firing also closes the
// double-fire window — an overrunning tick can't re-select an already-advanced row.
func (s *Store) ClaimDueTimer(id string, expectFireAt int64, patch map[string]any) (bool, error) {
	n, err := s.applyUpdateGuarded("timers", timerUpdateCols, id, patch,
		" AND status = 'scheduled' AND fireAt = ?", expectFireAt)
	return n > 0, err
}

// ---- watchers ----

// InsertWatcher inserts a watcher. Defaults id (wch_), status 'active' (NB: the
// code path always supplies active even though the column DEFAULT was 'created'),
// createdAt now; supervisor cadence is floored to max(cadenceMs, schedulerTickMS)
// so a supervisor can never tick faster than the scheduler.
func (s *Store) InsertWatcher(rec domain.WatcherRecord) (domain.WatcherRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWatcher)
	}
	if rec.Status == "" {
		rec.Status = "active"
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	if rec.IsSupervisor != nil && *rec.IsSupervisor && rec.CadenceMs < schedulerTickMS {
		rec.CadenceMs = schedulerTickMS
	}
	// Stamp a default lifetime ceiling when the caller omitted one, so no watcher
	// (terminal, PR, or supervisor) can poll forever — the timeout check is gated on
	// stopAfterMs != nil and completed_unverified never terminates on its own. This is
	// the single creation chokepoint for every path (tools, agenttaskx, mcpwrap), so
	// the default lands once here rather than at each call site. An explicit stopAfterMs
	// always wins. The ceiling is measured from createdAt (timeout fires at
	// now-createdAt >= stopAfterMs), so a startAfterMs delay is added in: the default
	// grants a full 24h of *actual watching* past any start delay rather than letting a
	// large startAfterMs make the watcher time out before its first check.
	if rec.StopAfterMs == nil {
		def := domain.WatcherDefaultLifetimeMS
		if rec.StartAfterMs != nil && *rec.StartAfterMs > 0 {
			def += *rec.StartAfterMs
		}
		rec.StopAfterMs = ptrI64(def)
	}
	_, err := s.db.Exec(`
		INSERT INTO watchers
		  (id,kind,title,goal,targetsJson,cadenceMs,isSupervisor,modelTier,startAfterMs,
		   stopAfterMs,stopWhenJson,alertWhenJson,optionsJson,status,lastClassification,
		   lastEpistemicKind,lastCheckedAt,nextCheckAt,createdAt,endedReason,endedAt,workflowRunId)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Kind, rec.Title, rec.Goal, rec.TargetsJson, rec.CadenceMs,
		boolToInt(rec.IsSupervisor), string(rec.ModelTier), nullI64(rec.StartAfterMs),
		nullI64(rec.StopAfterMs), nullStr(rec.StopWhenJson), nullStr(rec.AlertWhenJson),
		nullStr(rec.OptionsJson), rec.Status, nullStr(rec.LastClassification),
		epistemicArg(rec.LastEpistemicKind), nullI64(rec.LastCheckedAt), rec.NextCheckAt,
		rec.CreatedAt, nullStr(rec.EndedReason), nullI64(rec.EndedAt), nullStr(rec.WorkflowRunID))
	if err != nil {
		return domain.WatcherRecord{}, fmt.Errorf("insert watcher: %w", err)
	}
	return rec, nil
}

// epistemicArg binds an optional EpistemicKind (NULL when nil).
func epistemicArg(p *domain.EpistemicKind) any {
	if p == nil {
		return nil
	}
	return string(*p)
}

const watcherCols = `id,kind,title,goal,targetsJson,cadenceMs,isSupervisor,modelTier,startAfterMs,stopAfterMs,stopWhenJson,alertWhenJson,optionsJson,status,lastClassification,lastEpistemicKind,lastCheckedAt,nextCheckAt,createdAt,endedReason,endedAt,workflowRunId`

// scanWatcher rebuilds a WatcherRecord, coercing isSupervisor 0/1 back to *bool.
func scanWatcher(sc scanner) (domain.WatcherRecord, error) {
	var w domain.WatcherRecord
	var isSup int
	var modelTier string
	var startAfter, stopAfter, lastChecked, endedAt sql.NullInt64
	var stopWhen, alertWhen, options, lastClass, lastEpis, endedReason, workflowRunID sql.NullString
	if err := sc.Scan(&w.ID, &w.Kind, &w.Title, &w.Goal, &w.TargetsJson, &w.CadenceMs,
		&isSup, &modelTier, &startAfter, &stopAfter, &stopWhen, &alertWhen, &options,
		&w.Status, &lastClass, &lastEpis, &lastChecked, &w.NextCheckAt, &w.CreatedAt,
		&endedReason, &endedAt, &workflowRunID); err != nil {
		return domain.WatcherRecord{}, err
	}
	b := isSup != 0
	w.IsSupervisor = &b
	w.ModelTier = domain.ModelTier(modelTier)
	w.StartAfterMs = i64FromNull(startAfter)
	w.StopAfterMs = i64FromNull(stopAfter)
	w.StopWhenJson = strFromNull(stopWhen)
	w.AlertWhenJson = strFromNull(alertWhen)
	w.OptionsJson = strFromNull(options)
	w.LastClassification = strFromNull(lastClass)
	w.LastCheckedAt = i64FromNull(lastChecked)
	w.EndedReason = strFromNull(endedReason)
	w.EndedAt = i64FromNull(endedAt)
	w.WorkflowRunID = strFromNull(workflowRunID)
	if lastEpis.Valid {
		ek := domain.EpistemicKind(lastEpis.String)
		w.LastEpistemicKind = &ek
	}
	return w, nil
}

// GetWatcher returns a watcher by id, or (nil, nil) when absent.
func (s *Store) GetWatcher(id string) (*domain.WatcherRecord, error) {
	row := s.db.QueryRow("SELECT "+watcherCols+" FROM watchers WHERE id = ?", id)
	w, err := scanWatcher(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get watcher: %w", err)
	}
	return &w, nil
}

// ListWatchers returns all watchers (status=="") or by status, ORDER BY createdAt.
func (s *Store) ListWatchers(status string) ([]domain.WatcherRecord, error) {
	q := "SELECT " + watcherCols + " FROM watchers"
	var args []any
	if status != "" {
		q += " WHERE status = ?"
		args = append(args, status)
	}
	q += " ORDER BY createdAt"
	return queryWatchers(s.db, q, args...)
}

// ListLiveWatchers returns only operational watcher rows, oldest first. Dashboard
// and ownership callers must not materialize the unbounded terminal-history set
// merely to discard it in Go.
func (s *Store) ListLiveWatchers() ([]domain.WatcherRecord, error) {
	return queryWatchers(s.db,
		"SELECT "+watcherCols+" FROM watchers WHERE status IN ('active','created','paused') ORDER BY createdAt")
}

// DueWatchers returns active watchers with nextCheckAt <= now, ORDER BY nextCheckAt.
func (s *Store) DueWatchers(now int64) ([]domain.WatcherRecord, error) {
	return queryWatchers(s.db,
		"SELECT "+watcherCols+" FROM watchers WHERE status='active' AND nextCheckAt <= ? ORDER BY nextCheckAt",
		now)
}

func queryWatchers(db *sql.DB, q string, args ...any) ([]domain.WatcherRecord, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query watchers: %w", err)
	}
	defer rows.Close()
	var out []domain.WatcherRecord
	for rows.Next() {
		w, err := scanWatcher(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

var watcherUpdateCols = newColSet("title", "goal", "targetsJson", "cadenceMs",
	"isSupervisor", "modelTier", "startAfterMs", "stopAfterMs", "stopWhenJson",
	"alertWhenJson", "optionsJson", "status", "lastClassification", "lastEpistemicKind",
	"lastCheckedAt", "nextCheckAt", "endedReason", "endedAt", "workflowRunId")

// UpdateWatcher applies an allowlisted patch.
func (s *Store) UpdateWatcher(id string, patch map[string]any) error {
	return s.applyUpdate("watchers", watcherUpdateCols, id, patch)
}

// CancelLiveWatcher flips a watcher to 'cancelled' (stamping endedReason/endedAt)
// ONLY while it is still live (active/created/paused); reports whether a row
// flipped. The guard is the authoritative gate behind watcher.cancel's advisory
// pre-read: a cancel racing a natural finalize or an in-turn consumption loses
// cleanly instead of clobbering the row's original end state (and its caller then
// knows NOT to close the linked workflow run as cancelled over a done one).
func (s *Store) CancelLiveWatcher(id, reason string, now int64) (bool, error) {
	if now <= 0 {
		now = s.now()
	}
	n, err := s.applyUpdateGuarded("watchers", watcherUpdateCols, id, map[string]any{
		"status": "cancelled", "endedReason": reason, "endedAt": now,
	}, " AND status IN ('active','created','paused')")
	return n > 0, err
}

// ClaimDueWatcher atomically applies the daemon's per-check finalize patch ONLY while the
// watcher is still 'active'. Returns true iff a row matched. A false return means the main
// turn cancelled it during the check — the daemon must then NOT write it back (re-arming a
// cancelled watcher would RESURRECT it and keep it supervising forever).
func (s *Store) ClaimDueWatcher(id string, patch map[string]any) (bool, error) {
	n, err := s.applyUpdateGuarded("watchers", watcherUpdateCols, id, patch, " AND status = 'active'")
	return n > 0, err
}

// ReasonConsumedInTurn is the endedReason stamped when the main turn directly
// observed a supervised terminal's completion (terminal.awaitAll, or a settled
// terminal.extract wait) and retired the now-redundant supervisor watcher. The
// watcher's whole job — "tell the model when this agent is done" — was fulfilled
// in-hand, so letting it run on would only re-announce a completion the
// conversation already contains (the "stale notification" the model then has to
// resolve by hand). Distinct from 'user_cancelled'/'session_cleared' so the UI
// and audit can tell a natural retirement from a teardown. The canonical value
// lives in domain — the daemon matches on it in its lost-claim cleanup.
const ReasonConsumedInTurn = domain.WatcherEndedConsumedInTurn

// ConsumeSupervisorWatchersForTerminal retires every live SINGLE-target supervisor
// watcher aimed at terminalID: status → 'condition_met' (the supervised completion
// WAS observed — just by the main turn instead of the daemon), endedReason
// 'consumed_in_turn', endedAt stamped. Returns the flipped records so the caller
// can finish the retirement side effects (grant revocation, ledger advance, open
// inbox-event resolution) exactly like the watcher.cancel tool does.
//
// Deliberately narrow: only isSupervisor rows (a user-created monitor may have its
// own goal beyond "is it done" and is never touched), and only single-target rows
// (one terminal settling says nothing about a multi-target watcher's other
// targets). Each flip is claim-guarded on the live statuses, so a concurrent
// daemon finalize or user cancel wins cleanly and the row is not double-ended;
// like watcher.cancel, a check already past its stop decision may still emit one
// final honest event (stop outcomes publish before the claim).
func (s *Store) ConsumeSupervisorWatchersForTerminal(terminalID string) ([]domain.WatcherRecord, error) {
	live, err := queryWatchers(s.db,
		"SELECT "+watcherCols+" FROM watchers WHERE status IN ('active','created','paused') AND isSupervisor = 1 ORDER BY createdAt")
	if err != nil {
		return nil, fmt.Errorf("consume supervisor watchers: %w", err)
	}
	now := s.now()
	var out []domain.WatcherRecord
	for _, rec := range live {
		var targets []string
		if json.Unmarshal([]byte(rec.TargetsJson), &targets) != nil ||
			len(targets) != 1 || targets[0] != terminalID {
			continue
		}
		res, err := s.db.Exec(
			`UPDATE watchers SET status = 'condition_met', endedReason = ?, endedAt = ?
			  WHERE id = ? AND status IN ('active','created','paused')`,
			ReasonConsumedInTurn, now, rec.ID)
		if err != nil {
			return out, fmt.Errorf("consume supervisor watcher %s: %w", rec.ID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // lost the claim to a concurrent finalize/cancel — leave it be
		}
		rec.Status = "condition_met"
		reason := ReasonConsumedInTurn
		rec.EndedReason = &reason
		endedAt := now
		rec.EndedAt = &endedAt
		out = append(out, rec)
	}
	return out, nil
}
