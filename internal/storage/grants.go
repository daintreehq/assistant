package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
)

const grantCols = `id,actorId,actorType,allowedRiskClassesJson,allowedToolNamesJson,expiresAt,maxUses,usesRemaining,revokedAt,createdAt,source`

func scanGrant(sc scanner) (domain.AutomationGrantRecord, error) {
	var g domain.AutomationGrantRecord
	var actorType, source string
	var riskJson, toolJson sql.NullString
	var revokedAt sql.NullInt64
	if err := sc.Scan(&g.ID, &g.ActorID, &actorType, &riskJson, &toolJson, &g.ExpiresAt,
		&g.MaxUses, &g.UsesRemaining, &revokedAt, &g.CreatedAt, &source); err != nil {
		return domain.AutomationGrantRecord{}, err
	}
	g.ActorType = domain.AutomationGrantActorType(actorType)
	g.Source = domain.AutomationGrantSource(source)
	g.AllowedRiskClassesJson = strFromNull(riskJson)
	g.AllowedToolNamesJson = strFromNull(toolJson)
	g.RevokedAt = i64FromNull(revokedAt)
	return g, nil
}

// InsertGrant inserts an automation grant. Defaults: id grt_, usesRemaining =
// maxUses (when zero), revokedAt NULL, source 'local', createdAt now.
func (s *Store) InsertGrant(rec domain.AutomationGrantRecord) (domain.AutomationGrantRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixGrant)
	}
	if rec.UsesRemaining == 0 {
		rec.UsesRemaining = rec.MaxUses
	}
	if rec.Source == "" {
		rec.Source = domain.GrantSourceLocal
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = s.now()
	}
	_, err := s.db.Exec(`
		INSERT INTO automation_grants
		  (id,actorId,actorType,allowedRiskClassesJson,allowedToolNamesJson,expiresAt,
		   maxUses,usesRemaining,revokedAt,createdAt,source)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.ActorID, string(rec.ActorType), nullStr(rec.AllowedRiskClassesJson),
		nullStr(rec.AllowedToolNamesJson), rec.ExpiresAt, rec.MaxUses, rec.UsesRemaining,
		nullI64(rec.RevokedAt), rec.CreatedAt, string(rec.Source))
	if err != nil {
		return domain.AutomationGrantRecord{}, fmt.Errorf("insert grant: %w", err)
	}
	return rec, nil
}

// GetGrant returns a grant by id, or (nil, nil) when absent.
func (s *Store) GetGrant(id string) (*domain.AutomationGrantRecord, error) {
	row := s.db.QueryRow("SELECT "+grantCols+" FROM automation_grants WHERE id = ?", id)
	g, err := scanGrant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get grant: %w", err)
	}
	return &g, nil
}

// ListGrants returns LIVE grants (revokedAt NULL, expiresAt > now, usesRemaining
// > 0), optionally for a single actor, ORDER BY createdAt. now<=0 ⇒ s.now().
func (s *Store) ListGrants(actorID string, now int64) ([]domain.AutomationGrantRecord, error) {
	if now <= 0 {
		now = s.now()
	}
	q := "SELECT " + grantCols + ` FROM automation_grants
		 WHERE revokedAt IS NULL AND expiresAt > ? AND usesRemaining > 0`
	args := []any{now}
	if actorID != "" {
		q += " AND actorId = ?"
		args = append(args, actorID)
	}
	q += " ORDER BY createdAt"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	defer rows.Close()
	var out []domain.AutomationGrantRecord
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ConsumeGrant atomically consumes one use of the first live grant for actorID
// that matches actorType AND authorizes (toolName OR riskClass — union). The
// decrement is a guarded UPDATE (usesRemaining > 0 AND revokedAt IS NULL AND
// expiresAt > now); only on changes>0 is the use truly consumed (KEEP atomicity:
// single-conn store rules out interleave). Returns the post-decrement grant, or
// (nil, nil) when nothing authorizes.
func (s *Store) ConsumeGrant(actorID string, actorType domain.AutomationGrantActorType,
	toolName string, riskClass domain.RiskClass, now int64) (*domain.AutomationGrantRecord, error) {
	if now <= 0 {
		now = s.now()
	}
	// Run the candidate scan + guarded decrement + post-decrement reload in ONE
	// transaction so "first live grant" stays stable against a concurrent
	// consume/revoke (single connection ⇒ BEGIN IMMEDIATE serialization). Use only
	// tx-scoped statements; never call s.ListGrants / s.GetGrant inside the tx (they
	// take the same single connection and would deadlock).
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin consume grant: %w", err)
	}

	q := "SELECT " + grantCols + ` FROM automation_grants
		 WHERE revokedAt IS NULL AND expiresAt > ? AND usesRemaining > 0
		   AND actorId = ?
		 ORDER BY createdAt`
	rows, err := tx.Query(q, now, actorID)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("list grants: %w", err)
	}
	var grants []domain.AutomationGrantRecord
	for rows.Next() {
		g, serr := scanGrant(rows)
		if serr != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return nil, serr
		}
		grants = append(grants, g)
	}
	if rerr := rows.Err(); rerr != nil {
		_ = rows.Close()
		_ = tx.Rollback()
		return nil, rerr
	}
	_ = rows.Close()

	for _, g := range grants {
		if g.ActorType != actorType {
			continue
		}
		if !grantAuthorizes(g, toolName, riskClass) {
			continue
		}
		res, uerr := tx.Exec(`
			UPDATE automation_grants
			   SET usesRemaining = usesRemaining - 1
			 WHERE id = ? AND usesRemaining > 0 AND revokedAt IS NULL AND expiresAt > ?`,
			g.ID, now)
		if uerr != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("consume grant: %w", uerr)
		}
		n, rerr := res.RowsAffected()
		if rerr != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("consume grant rows affected: %w", rerr)
		}
		if n > 0 {
			out, gerr := scanGrant(tx.QueryRow(
				"SELECT "+grantCols+" FROM automation_grants WHERE id = ?", g.ID))
			if gerr != nil {
				_ = tx.Rollback()
				return nil, fmt.Errorf("reload consumed grant: %w", gerr)
			}
			if cerr := tx.Commit(); cerr != nil {
				_ = tx.Rollback()
				return nil, fmt.Errorf("commit consume grant: %w", cerr)
			}
			return &out, nil
		}
		// Lost the race / already exhausted: try the next live grant.
	}
	if cerr := tx.Commit(); cerr != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("commit consume grant: %w", cerr)
	}
	return nil, nil
}

// RevokeGrant stamps revokedAt WHERE revokedAt IS NULL; reports whether changed.
func (s *Store) RevokeGrant(id string, now int64) (bool, error) {
	if now <= 0 {
		now = s.now()
	}
	res, err := s.db.Exec(
		"UPDATE automation_grants SET revokedAt = ? WHERE id = ? AND revokedAt IS NULL", now, id)
	if err != nil {
		return false, fmt.Errorf("revoke grant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("revoke grant rows affected: %w", err)
	}
	return n > 0, nil
}

// RevokeGrantsByActor revokes all live grants for an actor; returns the count.
func (s *Store) RevokeGrantsByActor(actorID string, now int64) (int, error) {
	if now <= 0 {
		now = s.now()
	}
	res, err := s.db.Exec(
		"UPDATE automation_grants SET revokedAt = ? WHERE actorId = ? AND revokedAt IS NULL",
		now, actorID)
	if err != nil {
		return 0, fmt.Errorf("revoke grants by actor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("revoke grants by actor rows affected: %w", err)
	}
	return int(n), nil
}

// grantAuthorizes reports whether a grant authorizes (toolName, riskClass) by the
// union rule: toolName in the allowed-names list OR riskClass in the allowed-risk
// list. JSON parsing is tolerant — null/garbage ⇒ an empty list (no panic, no
// false grant).
func grantAuthorizes(g domain.AutomationGrantRecord, toolName string, riskClass domain.RiskClass) bool {
	names := parseStringList(g.AllowedToolNamesJson)
	for _, n := range names {
		if n == toolName {
			return true
		}
	}
	classes := parseStringList(g.AllowedRiskClassesJson)
	for _, c := range classes {
		if c == string(riskClass) {
			return true
		}
	}
	return false
}

// parseStringList tolerantly decodes a *string JSON array column into []string;
// nil/empty/garbage ⇒ nil (never errors).
func parseStringList(p *string) []string {
	if p == nil || *p == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(*p), &out); err != nil {
		return nil
	}
	return out
}
