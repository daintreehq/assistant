package storage

import (
	"fmt"
	"sort"
	"strings"
)

// scanner is the common Scan surface of *sql.Row and *sql.Rows, so one scan*
// helper serves both single-row and multi-row paths.
type scanner interface {
	Scan(dest ...any) error
}

// colSet is an allowlist of mutable column names for a dynamic UPDATE.
type colSet map[string]struct{}

func newColSet(cols ...string) colSet {
	m := make(colSet, len(cols))
	for _, c := range cols {
		m[c] = struct{}{}
	}
	return m
}

func (c colSet) has(col string) bool { _, ok := c[col]; return ok }

// unionColSet returns a new colSet = base plus extra columns.
func unionColSet(base colSet, extra ...string) colSet {
	m := make(colSet, len(base)+len(extra))
	for k := range base {
		m[k] = struct{}{}
	}
	for _, c := range extra {
		m[c] = struct{}{}
	}
	return m
}

// patchHasAllowedKey reports whether the patch sets at least one column in the
// allowlist (used by the workflow no-op guard so an empty/irrelevant patch never
// bumps updatedAt).
func patchHasAllowedKey(patch map[string]any, allowed colSet) bool {
	for k := range patch {
		if allowed.has(k) {
			return true
		}
	}
	return false
}

// applyUpdate builds a dynamic UPDATE touching only allowlisted columns from the
// patch map. No-op when the patch sets
// no allowed key. Column names come ONLY from the fixed allowlist (never the patch
// keys directly into SQL beyond the membership check) so interpolation is safe.
//
// Value coercion mirrors toSqlValue: bool→1/0, nil→NULL, everything else passes
// through to the driver. Patch keys are sorted for a deterministic statement.
func (s *Store) applyUpdate(table string, allowed colSet, id string, patch map[string]any) error {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		if allowed.has(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil // nothing allowed to set
	}
	sort.Strings(keys)

	sets := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)+1)
	for _, k := range keys {
		sets = append(sets, k+" = ?")
		args = append(args, toSQLValue(patch[k]))
	}
	args = append(args, id)
	q := fmt.Sprintf("UPDATE %s SET %s WHERE id = ?", table, strings.Join(sets, ", "))
	if _, err := s.db.Exec(q, args...); err != nil {
		return fmt.Errorf("update %s: %w", table, err)
	}
	return nil
}

// applyUpdateGuarded is applyUpdate with EXTRA, caller-supplied WHERE conditions appended
// (and their bind args), returning the number of rows affected so the caller can detect that
// the row no longer matched the guard (e.g. it was concurrently cancelled or edited) and
// react instead of blindly overwriting it. Returns (0, nil) when the patch sets no allowed
// key. whereExtra must begin with " AND " and reference only fixed column names.
func (s *Store) applyUpdateGuarded(table string, allowed colSet, id string, patch map[string]any, whereExtra string, whereArgs ...any) (int64, error) {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		if allowed.has(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return 0, nil
	}
	sort.Strings(keys)

	sets := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)+1+len(whereArgs))
	for _, k := range keys {
		sets = append(sets, k+" = ?")
		args = append(args, toSQLValue(patch[k]))
	}
	args = append(args, id)
	args = append(args, whereArgs...)
	q := fmt.Sprintf("UPDATE %s SET %s WHERE id = ?%s", table, strings.Join(sets, ", "), whereExtra)
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return 0, fmt.Errorf("update %s: %w", table, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// toSQLValue coerces a patch value to a driver-bindable form: bool→1/0, *bool→1/0
// (nil→NULL), and dereferenced pointer scalars; nil→NULL; everything else passes
// through (the driver handles string/int/float/[]byte).
func toSQLValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		if x {
			return 1
		}
		return 0
	case *bool:
		if x == nil {
			return nil
		}
		if *x {
			return 1
		}
		return 0
	case *string:
		if x == nil {
			return nil
		}
		return *x
	case *int:
		if x == nil {
			return nil
		}
		return *x
	case *int64:
		if x == nil {
			return nil
		}
		return *x
	default:
		return v
	}
}
