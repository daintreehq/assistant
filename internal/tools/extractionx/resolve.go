package extractionx

import (
	"context"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/tools/terminalid"
)

// codeUnknownTerminals is the loud-and-fast failure when a caller-supplied terminal id
// matches no live terminal (or an ambiguous prefix). It replaces the old silent failure
// mode where a truncated id ground the poll loop to its cap reporting "still working".
const codeUnknownTerminals = "UNKNOWN_TERMINALS"

// resolveTerminalIDs canonicalizes caller-supplied terminal ids against the live roster
// BEFORE any poll/read, so an abbreviated/prefix id (the model routinely truncates
// Daintree's full terminal-<uuid> ids) still resolves and a definitively-unknown id fails
// immediately instead of grinding for the whole wait budget.
//
// It runs ONCE per tool call (never inside the poll loop — re-listing every tick would
// burst Daintree's read throttle). Returns (canonicalIDs, nil) on success, or
// (nil, *Fail) when the roster is live and non-empty but an id resolves to no — or an
// ambiguous — terminal.
//
// FAILS OPEN: when the roster is unreadable OR empty it returns the ids UNCHANGED and lets
// the poll proceed. An empty/zero-terminal read is also the transport-hiccup symptom (the
// same case the poll loop's own empty-read guard handles), so rejecting there would turn a
// blip into a hard failure and could strand a legitimate wait. (Residual: if the roster is
// genuinely empty AND the ids are wrong, the poll still grinds to its cap — an accepted
// tradeoff vs. false-failing a real hiccup.)
//
// Unlike the single-id read path, this does NOT skip resolution for full canonical-looking
// ids: a cohort wait is infrequent, and always-resolving means a STALE full id also fails
// fast here (rather than the await loop's absent-guard reporting it as "finished" when it
// sits in a cohort alongside live terminals).
func resolveTerminalIDs(ctx context.Context, deps Deps, ids []string) ([]string, *tools.ToolResult) {
	live, ok := deps.Reader.ListTerminals(ctx)
	if !ok || len(live) == 0 {
		return ids, nil // fail open — never block a wait on a discovery hiccup
	}
	r := terminalid.Resolve(ids, live)
	if r.OK() {
		return r.Resolved, nil
	}

	var b strings.Builder
	if len(r.Unknown) > 0 {
		fmt.Fprintf(&b, "No live terminal matches %s. ", quoteList(r.Unknown))
	}
	if len(r.Ambiguous) > 0 {
		verb := "is"
		if len(r.Ambiguous) > 1 {
			verb = "are"
		}
		fmt.Fprintf(&b, "%s %s an ambiguous prefix matching several terminals. ", quoteList(r.Ambiguous), verb)
	}
	b.WriteString("Use the EXACT, FULL terminal id returned by the spawn (e.g. terminal-5284bfef-3d11-424c-90cb-136f24046295) — never an abbreviated prefix. Live terminals: " + strings.Join(live, ", ") + ".")

	fail := tools.Fail(codeUnknownTerminals, b.String(), tools.WithDetails(map[string]any{
		"unknown":       r.Unknown,
		"ambiguous":     r.Ambiguous,
		"liveTerminals": live,
	}))
	return nil, &fail
}

// quoteList renders ids as a comma-separated list of quoted strings for an error message.
func quoteList(ids []string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = fmt.Sprintf("%q", id)
	}
	return strings.Join(q, ", ")
}
