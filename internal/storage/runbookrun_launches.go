package storage

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
)

// ---- runbook_run_state ----

const runbookRunCols = `id,sessionId,runbookId,currentStep,stepsJson,status,startedAt,updatedAt,completedAt`

func scanRunbookRunState(sc scanner) (domain.RunbookRunStateRecord, error) {
	var r domain.RunbookRunStateRecord
	var status string
	var completedAt sql.NullInt64
	if err := sc.Scan(&r.ID, &r.SessionID, &r.RunbookID, &r.CurrentStep, &r.StepsJson,
		&status, &r.StartedAt, &r.UpdatedAt, &completedAt); err != nil {
		return domain.RunbookRunStateRecord{}, err
	}
	r.Status = domain.RunbookRunStatus(status)
	r.CompletedAt = i64FromNull(completedAt)
	return r, nil
}

// InsertRunbookRunState inserts runbook run state (id rrs_, currentStep 0, stepsJson
// '[]', status 'active', startedAt/updatedAt now when zero). Natural-key
// (sessionId, runbookId) is unique-indexed; a duplicate insert errors.
func (s *Store) InsertRunbookRunState(rec domain.RunbookRunStateRecord) (domain.RunbookRunStateRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixRunbookRun)
	}
	if rec.StepsJson == "" {
		rec.StepsJson = "[]"
	}
	if rec.Status == "" {
		rec.Status = domain.RunbookRunActive
	}
	now := s.now()
	if rec.StartedAt == 0 {
		rec.StartedAt = now
	}
	if rec.UpdatedAt == 0 {
		rec.UpdatedAt = now
	}
	_, err := s.db.Exec(`
		INSERT INTO runbook_run_state
		  (id,sessionId,runbookId,currentStep,stepsJson,status,startedAt,updatedAt,completedAt)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.SessionID, rec.RunbookID, rec.CurrentStep, rec.StepsJson,
		string(rec.Status), rec.StartedAt, rec.UpdatedAt, nullI64(rec.CompletedAt))
	if err != nil {
		return domain.RunbookRunStateRecord{}, fmt.Errorf("insert runbook run state: %w", err)
	}
	return rec, nil
}

// GetRunbookRunState looks up by the natural key (sessionId, runbookId).
func (s *Store) GetRunbookRunState(sessionID, runbookID string) (*domain.RunbookRunStateRecord, error) {
	row := s.db.QueryRow(
		"SELECT "+runbookRunCols+" FROM runbook_run_state WHERE sessionId = ? AND runbookId = ?",
		sessionID, runbookID)
	r, err := scanRunbookRunState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get runbook run state: %w", err)
	}
	return &r, nil
}

// ListRunbookRunStates returns all states (sessionID=="") or by session, ORDER BY
// updatedAt DESC.
func (s *Store) ListRunbookRunStates(sessionID string) ([]domain.RunbookRunStateRecord, error) {
	q := "SELECT " + runbookRunCols + " FROM runbook_run_state"
	var args []any
	if sessionID != "" {
		q += " WHERE sessionId = ?"
		args = append(args, sessionID)
	}
	q += " ORDER BY updatedAt DESC"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runbook run states: %w", err)
	}
	defer rows.Close()
	var out []domain.RunbookRunStateRecord
	for rows.Next() {
		r, err := scanRunbookRunState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var runbookRunUpdateCols = newColSet("currentStep", "stepsJson", "status", "completedAt")
var runbookRunUpdateColsWithTime = unionColSet(runbookRunUpdateCols, "updatedAt")

// UpdateRunbookRunState force-sets updatedAt = now and applies the allowlisted patch.
func (s *Store) UpdateRunbookRunState(id string, patch map[string]any) error {
	merged := make(map[string]any, len(patch)+1)
	for k, v := range patch {
		merged[k] = v
	}
	merged["updatedAt"] = s.now()
	return s.applyUpdate("runbook_run_state", runbookRunUpdateColsWithTime, id, merged)
}

// ---- agent_launches ----

const agentLaunchCols = `id,idempotencyKey,agentId,worktreeId,mode,title,name,terminalId,watcherId,stage,errorCode,errorMessage,createdAt,updatedAt,workflowRunId`

func scanAgentLaunch(sc scanner) (domain.AgentLaunchRecord, error) {
	var a domain.AgentLaunchRecord
	var stage string
	var worktreeID, terminalID, watcherID, errorCode, errorMessage, workflowRunID sql.NullString
	if err := sc.Scan(&a.ID, &a.IdempotencyKey, &a.AgentID, &worktreeID, &a.Mode,
		&a.Title, &a.Name, &terminalID, &watcherID, &stage, &errorCode, &errorMessage,
		&a.CreatedAt, &a.UpdatedAt, &workflowRunID); err != nil {
		return domain.AgentLaunchRecord{}, err
	}
	a.WorktreeID = strFromNull(worktreeID)
	a.TerminalID = strFromNull(terminalID)
	a.WatcherID = strFromNull(watcherID)
	a.Stage = domain.AgentLaunchStage(stage)
	a.ErrorCode = strFromNull(errorCode)
	a.ErrorMessage = strFromNull(errorMessage)
	a.WorkflowRunID = strFromNull(workflowRunID)
	return a, nil
}

// InsertAgentLaunch inserts a spawn saga (id agt_, stage 'launch_requested',
// createdAt/updatedAt now when zero).
func (s *Store) InsertAgentLaunch(rec domain.AgentLaunchRecord) (domain.AgentLaunchRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixAgentLaunch)
	}
	if rec.Stage == "" {
		rec.Stage = domain.LaunchRequested
	}
	now := s.now()
	if rec.CreatedAt == 0 {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt == 0 {
		rec.UpdatedAt = now
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_launches
		  (id,idempotencyKey,agentId,worktreeId,mode,title,name,terminalId,watcherId,
		   stage,errorCode,errorMessage,createdAt,updatedAt,workflowRunId)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.IdempotencyKey, rec.AgentID, nullStr(rec.WorktreeID), rec.Mode,
		rec.Title, rec.Name, nullStr(rec.TerminalID), nullStr(rec.WatcherID),
		string(rec.Stage), nullStr(rec.ErrorCode), nullStr(rec.ErrorMessage),
		rec.CreatedAt, rec.UpdatedAt, nullStr(rec.WorkflowRunID))
	if err != nil {
		return domain.AgentLaunchRecord{}, fmt.Errorf("insert agent launch: %w", err)
	}
	return rec, nil
}

// GetAgentLaunch returns a saga by id, or (nil, nil) when absent.
func (s *Store) GetAgentLaunch(id string) (*domain.AgentLaunchRecord, error) {
	row := s.db.QueryRow("SELECT "+agentLaunchCols+" FROM agent_launches WHERE id = ?", id)
	a, err := scanAgentLaunch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get agent launch: %w", err)
	}
	return &a, nil
}

// ListAgentLaunches returns the newest-first spawn sagas, bounded by limit
// (limit <= 0 defaults to 20 — the invariant lives here so callers can pass an
// unvalidated cap). In practice session-scoped: the dead spawn roster is cleared on
// DB open (resetStaleAgentLaunches keeps only confirmed-with-terminal sagas).
func (s *Store) ListAgentLaunches(limit int) ([]domain.AgentLaunchRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		"SELECT "+agentLaunchCols+" FROM agent_launches ORDER BY updatedAt DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("list agent launches: %w", err)
	}
	defer rows.Close()
	var out []domain.AgentLaunchRecord
	for rows.Next() {
		a, err := scanAgentLaunch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListConfirmedAgentLaunchesWithTerminal returns the newest-first confirmed spawn
// sagas that bound a terminal — the boot-reconcile candidates. A confirmed-with-
// terminal saga is the ONLY class the session-open reset keeps (resetStaleAgentLaunches
// deletes every other stage), so a row here from a prior session means a visible agent
// was launched whose supervisor watcher has since been cancelled on open. Boot
// reconciliation cross-joins these against the live terminal.list to find still-running
// orphans. Bounded by limit (<=0 ⇒ 20).
func (s *Store) ListConfirmedAgentLaunchesWithTerminal(limit int) ([]domain.AgentLaunchRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(
		"SELECT "+agentLaunchCols+`
		   FROM agent_launches
		  WHERE stage = 'confirmed' AND terminalId IS NOT NULL AND terminalId != ''
		  ORDER BY updatedAt DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list confirmed agent launches with terminal: %w", err)
	}
	defer rows.Close()
	var out []domain.AgentLaunchRecord
	for rows.Next() {
		a, err := scanAgentLaunch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FindActiveAgentLaunch returns the newest NON-TERMINAL saga for an idempotency
// key (stage NOT IN confirmed/failed), or (nil, nil) — so a fresh launch can
// re-attach instead of double-spawning.
func (s *Store) FindActiveAgentLaunch(idempotencyKey string) (*domain.AgentLaunchRecord, error) {
	row := s.db.QueryRow("SELECT "+agentLaunchCols+`
		  FROM agent_launches
		 WHERE idempotencyKey = ? AND stage NOT IN ('confirmed','failed')
		 ORDER BY updatedAt DESC LIMIT 1`, idempotencyKey)
	a, err := scanAgentLaunch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find active agent launch: %w", err)
	}
	return &a, nil
}

var agentLaunchUpdateCols = newColSet("agentId", "worktreeId", "mode", "title", "name",
	"terminalId", "watcherId", "stage", "errorCode", "errorMessage", "workflowRunId")
var agentLaunchUpdateColsWithTime = unionColSet(agentLaunchUpdateCols, "updatedAt")

// UpdateAgentLaunch force-sets updatedAt = now and applies the allowlisted patch.
func (s *Store) UpdateAgentLaunch(id string, patch map[string]any) error {
	merged := make(map[string]any, len(patch)+1)
	for k, v := range patch {
		merged[k] = v
	}
	merged["updatedAt"] = s.now()
	return s.applyUpdate("agent_launches", agentLaunchUpdateColsWithTime, id, merged)
}
