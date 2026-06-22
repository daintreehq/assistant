package storage

import (
	"database/sql"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// nullStr converts an optional string pointer to a driver-bindable value (NULL
// when nil). Used for all *string columns.
func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullI64 binds an optional int64 (NULL when nil).
func nullI64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullInt binds an optional int (NULL when nil).
func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// ptrStr / ptrI64 / ptrInt copy a value to a fresh pointer (so callers never
// alias a scratch variable).
func ptrStr(v string) *string { return &v }
func ptrI64(v int64) *int64   { return &v }
func ptrInt(v int) *int       { return &v }

// strFromNull returns a *string from a sql.NullString.
func strFromNull(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// i64FromNull returns a *int64 from a sql.NullInt64.
func i64FromNull(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// intFromNull returns a *int from a sql.NullInt64.
func intFromNull(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// boolToInt maps a *bool to the 0/1 INTEGER storage form (nil ⇒ 0). isSupervisor
// is the only boolean column.
func boolToInt(p *bool) int {
	if p != nil && *p {
		return 1
	}
	return 0
}

// sevCase is the SQL CASE that mirrors domain.SeverityRank. It
// MUST stay in sync with the Go map — generated below from RankOf to avoid drift.
// Note `done`(2) ranks BELOW attention/blocked/urgent/error; unknown ⇒ ELSE 1.
var sevCase = buildSevCase()

func buildSevCase() string {
	// Build "CASE severity WHEN 'x' THEN n ... ELSE 1 END" from the single source
	// of truth (domain.SeverityRank) so the SQL ordering can never drift from Go.
	var b strings.Builder
	b.WriteString("CASE severity")
	// Stable order for readability; values come straight from the map.
	order := []domain.Severity{
		domain.SeverityDebug, domain.SeverityInfo, domain.SeverityDone,
		domain.SeverityAttention, domain.SeverityBlocked, domain.SeverityUrgent,
		domain.SeverityError,
	}
	for _, sev := range order {
		b.WriteString(" WHEN '")
		b.WriteString(string(sev))
		b.WriteString("' THEN ")
		b.WriteByte(byte('0' + domain.RankOf(sev)))
	}
	b.WriteString(" ELSE 1 END")
	return b.String()
}

// escapeFTSQuery tokenizes on whitespace and quotes EACH token, doubling any
// internal `"`, then space-joins → FTS5 implicit-AND keyword search. This is a
// security boundary: raw user input passed to MATCH raises a
// SQLite syntax error / lets the user inject FTS operators. Returns "" if the
// query has no tokens (caller short-circuits to an empty result).
func escapeFTSQuery(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, tok := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(tok, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}
