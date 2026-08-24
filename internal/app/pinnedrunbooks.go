package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/daintreehq/assistant/internal/backend"
)

// pinnedrunbooks.go owns the `--runbook` preflight: the one place that decides whether a
// caller-named runbook may ride a turn, and the one place that refuses to send it.
//
// The whole feature exists because a silently-unpinned run is indistinguishable from a
// pinned one. So every failure here is LOUD and happens BEFORE a turn is spent —
// refusing to launch is the honest answer, while proceeding unpinned reproduces exactly
// the ambiguity the flag was added to remove.

// NormalizePinnedRunbookIDs cleans a caller-supplied pin list: trim, drop blanks, and
// collapse exact repeats keeping first-seen order. Case is PRESERVED — a runbook id is
// the backend's own key, not a user-facing label, and lowercasing one would turn a
// typo the catalog check can name into a different typo it cannot.
//
// Order is preserved because the backend admits pins in the order given and budgets
// them against max_active_runbooks: with a cap of two, `--runbook a --runbook b` and
// `--runbook b --runbook a` are genuinely different requests.
//
// Returns nil (not an empty slice) for "no pins", which is what every caller tests.
func NormalizePinnedRunbookIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// PinnedRunbookIDs returns this session's pinned runbook ids (nil when none were named).
// The returned slice is a copy — the App's own list must not be mutable through it.
func (a *App) PinnedRunbookIDs() []string {
	if len(a.pinnedRunbookIDs) == 0 {
		return nil
	}
	return append([]string(nil), a.pinnedRunbookIDs...)
}

// backendAcceptsPinnedRunbookIDs reports whether it is SAFE to attach pinned runbook ids to
// a request. The exact shape of backendAcceptsDisplayContext, and for the same reason:
// Selection is validated with extra="forbid", so guessing wrong costs the whole turn.
// Fails closed on an unknown backend, and on one whose cached answer came from a
// DIFFERENT endpoint than the one about to be called.
func (a *App) backendAcceptsPinnedRunbookIDs() bool {
	snap := a.backendCaps.Load()
	if snap == nil || a.Backend == nil {
		return false
	}
	return snap.baseURL == a.Backend.BaseURL() && snap.caps.Runbooks.PinnedRunbookIDs
}

// PreparePinnedRunbooks negotiates the pins this App was created with, and must be called
// once after app.Create on every path that can run a turn — one-shot, interactive, and
// the MCP session factory. It returns a non-fatal advisory (empty when there is none)
// for the caller to surface in whatever output contract it owns, or an error that must
// abort the launch.
//
// It returns IMMEDIATELY when no pins were named, which is the entire reason the
// capability fetch lives here rather than in a boot handshake: an ordinary launch pays
// no new network round trip, and only a caller who actually asked for a pin waits for
// the answer.
//
// The three outcomes, in the order they are decided:
//
//   - the capability fetch fails, or the backend does not advertise
//     runbooks.pinned_runbook_ids → ERROR. Withholding the field and running anyway is
//     precisely the silent no-op `--runbook` exists to prevent, and sending it anyway
//     would 422 the turn.
//   - the backend advertises pinning but no catalog → ADVISORY. The id cannot be
//     checked locally; the backend's own `unknown_runbook_id_ignored` warning is the
//     backstop, and refusing to launch over a capability the SERVER is missing would
//     punish the caller for the deployment's age.
//   - the catalog is advertised and an id is not in it → ERROR, with a near miss. This
//     is the common case (a typo), and it is worth the whole feature to catch it here
//     instead of after a normal-looking run.
func (a *App) PreparePinnedRunbooks(ctx context.Context) (string, error) {
	pins := a.pinnedRunbookIDs
	if len(pins) == 0 {
		return "", nil
	}
	caps, err := a.BackendCapabilities(ctx)
	if err != nil {
		return "", fmt.Errorf("--runbook needs the backend's capabilities and they could not be read (%w); "+
			"a pin that cannot be negotiated would run silently unpinned", err)
	}
	if !caps.Runbooks.PinnedRunbookIDs {
		return "", errors.New("this backend does not accept pinned runbooks (no runbooks.pinned_runbook_ids capability), " +
			"so --runbook cannot be honoured; upgrade the backend, or drop --runbook to let the selector choose")
	}
	if caps.Runbooks.Catalog == nil {
		return fmt.Sprintf("this backend advertises no runbook catalog, so %s could not be checked before the turn; "+
			"an id it does not recognise will be reported as a warning instead", quoteIDs(pins)), nil
	}
	if err := validatePinnedRunbookIDs(pins, caps.Runbooks.Catalog); err != nil {
		return "", err
	}
	return "", nil
}

// validatePinnedRunbookIDs checks every pin against the advertised catalog and reports
// ALL failures at once — a caller who mistyped two ids should not have to re-run to
// discover the second.
//
// Matching is exact and case-sensitive (the id is the backend's key); the near-miss
// suggestion is case-insensitive, because a wrong case is exactly the kind of typo a
// suggestion should catch. A suggestion never rewrites what is sent: guessing at a
// runbook would defeat the point of naming one.
func validatePinnedRunbookIDs(pins []string, catalog []backend.RunbookRef) error {
	known := make(map[string]struct{}, len(catalog))
	for _, ref := range catalog {
		known[ref.ID] = struct{}{}
	}
	var bad []string
	for _, id := range pins {
		if _, ok := known[id]; ok {
			continue
		}
		if near := nearestRunbookID(id, catalog); near != "" {
			bad = append(bad, fmt.Sprintf("%q (did you mean %q?)", id, near))
			continue
		}
		bad = append(bad, fmt.Sprintf("%q", id))
	}
	if len(bad) == 0 {
		return nil
	}
	noun := "unknown runbook id"
	if len(bad) > 1 {
		noun = "unknown runbook ids"
	}
	return fmt.Errorf("%s: %s — run `daintree-assistant --list-runbooks` to see what this backend can load",
		noun, strings.Join(bad, ", "))
}

// maxRunbookSuggestionDistance is how wrong an id may be and still be worth guessing at.
// Two edits catches the typos that actually happen — a transposition, a dropped
// character, a wrong case — while staying far short of the distance at which a
// suggestion starts naming an unrelated runbook and sending the reader to fix something
// that was never the problem.
const maxRunbookSuggestionDistance = 2

// nearestRunbookID returns the catalog id within maxRunbookSuggestionDistance edits of want,
// or "" when nothing is close enough. Ties break on the lexicographically smallest id so
// the suggestion is deterministic rather than dependent on catalog order.
//
// This deliberately does not reuse internal/commands.suggestCommand: that package
// imports app, so the dependency can only run the other way.
func nearestRunbookID(want string, catalog []backend.RunbookRef) string {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return ""
	}
	best, bestD := "", maxRunbookSuggestionDistance+1
	for _, ref := range catalog {
		id := strings.TrimSpace(ref.ID)
		if id == "" {
			continue
		}
		d := levenshtein(want, strings.ToLower(id))
		// Checked FIRST and separately from the tie-break below. Folding the two
		// together works only because nothing sorts before the empty initial best, which
		// is the kind of accident that survives until someone seeds `best` differently.
		if d > maxRunbookSuggestionDistance {
			continue
		}
		if best == "" || d < bestD || (d == bestD && id < best) {
			best, bestD = id, d
		}
	}
	return best
}

// levenshtein is the classic edit distance (insertions/deletions/substitutions),
// rune-aware so a multi-byte id doesn't skew the count.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// quoteIDs renders a pin list for a human sentence, sorted so the message is stable.
func quoteIDs(ids []string) string {
	out := make([]string, len(ids))
	copy(out, ids)
	sort.Strings(out)
	for i, id := range out {
		out[i] = fmt.Sprintf("%q", id)
	}
	return strings.Join(out, ", ")
}
