package storage

import (
	"database/sql"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// ---- audit_log ----

// AuditFilters is the optional AND-combined filter set for QueryAudit. ms bounds
// are inclusive.
type AuditFilters struct {
	Actor    *domain.ToolActor
	ToolName *string
	Outcome  *string
	TsFrom   *int64
	TsTo     *int64
	Limit    *int
}

const auditCols = `id,ts,actor,toolName,argsJson,outcome,durationMs,summary,resultJson,grantSource,grantId,runId`

func scanAudit(sc scanner) (domain.AuditRecord, error) {
	var a domain.AuditRecord
	var actor, outcome string
	var resultJson, grantSource, grantID, runID sql.NullString
	if err := sc.Scan(&a.ID, &a.Ts, &actor, &a.ToolName, &a.ArgsJson, &outcome,
		&a.DurationMs, &a.Summary, &resultJson, &grantSource, &grantID, &runID); err != nil {
		return domain.AuditRecord{}, err
	}
	a.Actor = domain.ToolActor(actor)
	a.Outcome = outcome
	a.ResultJson = strFromNull(resultJson)
	if grantSource.Valid {
		gs := domain.AutomationGrantSource(grantSource.String)
		a.GrantSource = &gs
	}
	a.GrantID = strFromNull(grantID)
	a.RunID = strFromNull(runID)
	return a, nil
}

// InsertAudit appends an audit row (id aud_, ts now when zero).
func (s *Store) InsertAudit(rec domain.AuditRecord) (domain.AuditRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixAudit)
	}
	if rec.Ts == 0 {
		rec.Ts = s.now()
	}
	var grantSource any
	if rec.GrantSource != nil {
		grantSource = string(*rec.GrantSource)
	}
	_, err := s.db.Exec(`
		INSERT INTO audit_log
		  (id,ts,actor,toolName,argsJson,outcome,durationMs,summary,resultJson,
		   grantSource,grantId,runId)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.Ts, string(rec.Actor), rec.ToolName, rec.ArgsJson, rec.Outcome,
		rec.DurationMs, rec.Summary, nullStr(rec.ResultJson), grantSource,
		nullStr(rec.GrantID), nullStr(rec.RunID))
	if err != nil {
		return domain.AuditRecord{}, fmt.Errorf("insert audit: %w", err)
	}
	return rec, nil
}

// ListAudit returns the most recent rows, ts DESC.
func (s *Store) ListAudit(limit int) ([]domain.AuditRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	return queryAuditRows(s.db,
		fmt.Sprintf("SELECT %s FROM audit_log ORDER BY ts DESC LIMIT %d", auditCols, limit))
}

// QueryAudit applies optional AND-combined filters via the (? IS NULL OR col = ?)
// pattern (each filter bound twice). limit default 200, ts DESC. Nil filters are
// bound as NULL.
func (s *Store) QueryAudit(f AuditFilters) ([]domain.AuditRecord, error) {
	limit := 200
	if f.Limit != nil && *f.Limit > 0 {
		limit = *f.Limit
	}
	var actor, tool, outcome any
	if f.Actor != nil {
		actor = string(*f.Actor)
	}
	if f.ToolName != nil {
		tool = *f.ToolName
	}
	if f.Outcome != nil {
		outcome = *f.Outcome
	}
	q := fmt.Sprintf(`
		SELECT %s FROM audit_log
		 WHERE (? IS NULL OR actor = ?)
		   AND (? IS NULL OR toolName = ?)
		   AND (? IS NULL OR outcome = ?)
		   AND (? IS NULL OR ts >= ?)
		   AND (? IS NULL OR ts <= ?)
		 ORDER BY ts DESC LIMIT %d`, auditCols, limit)
	return queryAuditRows(s.db, q,
		actor, actor, tool, tool, outcome, outcome,
		nullI64(f.TsFrom), nullI64(f.TsFrom), nullI64(f.TsTo), nullI64(f.TsTo))
}

// ListAuditByRunID returns a run's audit rows oldest-first.
func (s *Store) ListAuditByRunID(runID string) ([]domain.AuditRecord, error) {
	return queryAuditRows(s.db,
		"SELECT "+auditCols+" FROM audit_log WHERE runId = ? ORDER BY ts ASC", runID)
}

func queryAuditRows(db *sql.DB, q string, args ...any) ([]domain.AuditRecord, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit: %w", err)
	}
	defer rows.Close()
	var out []domain.AuditRecord
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- run_events ----

// InsertRunEvent appends a per-run replay event (id rne_, ts now when zero).
func (s *Store) InsertRunEvent(rec domain.RunEventRecord) (domain.RunEventRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixRunEvent)
	}
	if rec.Ts == 0 {
		rec.Ts = s.now()
	}
	_, err := s.db.Exec(`
		INSERT INTO run_events (id,runId,seq,ts,type,payload) VALUES (?,?,?,?,?,?)`,
		rec.ID, rec.RunID, rec.Seq, rec.Ts, rec.Type, nullStr(rec.Payload))
	if err != nil {
		return domain.RunEventRecord{}, fmt.Errorf("insert run event: %w", err)
	}
	return rec, nil
}

// ListRunEvents returns a run's events in replay order (seq ASC).
func (s *Store) ListRunEvents(runID string) ([]domain.RunEventRecord, error) {
	rows, err := s.db.Query(
		"SELECT id,runId,seq,ts,type,payload FROM run_events WHERE runId = ? ORDER BY seq ASC",
		runID)
	if err != nil {
		return nil, fmt.Errorf("list run events: %w", err)
	}
	defer rows.Close()
	var out []domain.RunEventRecord
	for rows.Next() {
		var r domain.RunEventRecord
		var payload sql.NullString
		if err := rows.Scan(&r.ID, &r.RunID, &r.Seq, &r.Ts, &r.Type, &payload); err != nil {
			return nil, err
		}
		r.Payload = strFromNull(payload)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRuns aggregates run_events into per-run summaries (MIN/MAX ts, count),
// newest-last-activity first. There is no run table.
func (s *Store) ListRuns(limit int) ([]domain.RunSummaryRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT runId, MIN(ts) AS firstTs, MAX(ts) AS lastTs, COUNT(*) AS eventCount
		  FROM run_events
		 GROUP BY runId
		 ORDER BY lastTs DESC
		 LIMIT %d`, limit))
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []domain.RunSummaryRecord
	for rows.Next() {
		var r domain.RunSummaryRecord
		if err := rows.Scan(&r.RunID, &r.FirstTs, &r.LastTs, &r.EventCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
