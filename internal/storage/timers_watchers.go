package storage

import (
	"database/sql"
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

// ClaimDueWatcher atomically applies the daemon's per-check finalize patch ONLY while the
// watcher is still 'active'. Returns true iff a row matched. A false return means the main
// turn cancelled it during the check — the daemon must then NOT write it back (re-arming a
// cancelled watcher would RESURRECT it and keep it supervising forever).
func (s *Store) ClaimDueWatcher(id string, patch map[string]any) (bool, error) {
	n, err := s.applyUpdateGuarded("watchers", watcherUpdateCols, id, patch, " AND status = 'active'")
	return n > 0, err
}
