package queue

import "github.com/daintreehq/daintree-assistant/internal/domain"

// The severity RANK order is authoritative for both the severityAtLeast filter
// and ORDER BY ... DESC, and it intentionally DIFFERS from the Severity enum
// declaration order (§6.4). The canonical rank map lives in
// domain.SeverityRank / domain.RankOf (unknown ⇒ 1, "info"). We re-expose the
// SQL mirror here so a concrete EventStore can build the WHERE/ORDER BY without
// re-deriving it.

// SeverityRank returns the numeric rank for a severity, defaulting to the info
// rank (1) for any unknown value — the same fallback as the SQL CASE ELSE 1.
func SeverityRank(s domain.Severity) int { return domain.RankOf(s) }

// SeverityCaseSQL is the SQL expression mirroring domain.SeverityRank, for
// filtering/ordering before LIMIT. The trailing `ELSE 1` makes an unknown stored
// severity sort as `info` — keep it in lockstep with domain.RankOf. The literal
// text is load-bearing for any store reusing it in raw SQL.
const SeverityCaseSQL = "CASE severity " +
	"WHEN 'debug' THEN 0 " +
	"WHEN 'info' THEN 1 " +
	"WHEN 'done' THEN 2 " +
	"WHEN 'attention' THEN 3 " +
	"WHEN 'blocked' THEN 4 " +
	"WHEN 'urgent' THEN 5 " +
	"WHEN 'error' THEN 6 " +
	"ELSE 1 END"
