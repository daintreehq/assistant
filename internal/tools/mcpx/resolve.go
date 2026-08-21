package mcpx

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/tools/terminalid"
)

// codeTerminalNotFound is the model-facing code for a terminal id that matches no
// live terminal (or matches several, as an ambiguous prefix). It mirrors the
// contextx/extractionx families' code so the model sees ONE vocabulary for "that
// id is not a terminal" regardless of which family it reached for.
const codeTerminalNotFound = "TERMINAL_NOT_FOUND"

// credibleTruncationRe matches the truncation the model actually produces: the
// "terminal-" marker plus at least the leading 8 hex characters of the uuid. It gates
// PREFIX EXPANSION only — never an id we forward verbatim — because expanding is the
// only operation that can send a mutation somewhere the human did not name. See
// resolveTerminalIDs.
var credibleTruncationRe = regexp.MustCompile(`^terminal-[0-9a-fA-F]{8}`)

// resolveTerminalIDs canonicalizes a whole cohort of caller-supplied terminal ids
// against the live roster in ONE terminal.list read. It exists because the model
// routinely truncates Daintree's terminal-<uuid> ids to a short prefix, and a
// mutation that silently no-ops on a truncated id is worse here than in a read: the
// human already confirmed the move, so a miss reads as "Daintree lost my terminal"
// rather than "you passed a prefix".
//
// One snapshot for the whole cohort, never one read per id: besides the obvious
// traffic saving, it guarantees every id in a single confirmed operation is resolved
// against the SAME roster, so a terminal opening or closing mid-batch cannot make two
// ids in one call disagree about what was live.
//
// FAILS OPEN — returns the requested ids unchanged — whenever the roster is
// unavailable rather than merely absent: no MCP, a transport error, a Daintree-side
// error result, or an empty/unparseable list. An empty read is also the transport-
// hiccup symptom, not "every id you named is wrong", so a discovery blip must never
// block a move the human confirmed. The raw action's own not-found error is the
// backstop in that case.
//
// ATOMIC on a definitive RESOLUTION miss: if the roster IS readable and any id
// resolves to nothing (or to several terminals), the whole call fails with zero moves,
// because the model named something that is not a terminal and guessing which half of
// a cohort it meant is worse than a clean rejection naming the live ids.
//
// That guarantee covers RESOLUTION, not LIVENESS, and the distinction is worth being
// honest about: an all-canonical cohort skips the roster read entirely, so a full id
// that is stale is discovered only by Daintree, mid-batch, after earlier ids have
// already moved. We accept that asymmetry rather than pay a roster read on every move:
// a stale full id is indistinguishable from a live one without asking Daintree, and
// the partial-failure report names exactly what moved and what did not.
//
// Returns (canonicalIDs, nil) or (nil, *Fail).
func resolveTerminalIDs(ctx context.Context, mcp MCPClient, ids []string) ([]string, *tools.ToolResult) {
	// A cohort that is already entirely canonical skips the roster read — the common
	// case when the model echoes ids straight from a spawn or terminal.list result.
	// A stale-but-full id is caught by the raw action's own not-found error, which
	// then flows through the batch's faithful partial-failure reporting.
	allCanonical := true
	for _, id := range ids {
		if !terminalid.LooksCanonical(id) {
			allCanonical = false
			break
		}
	}
	// Check cancellation before the fast return too: an abandoned turn must not fall
	// through into the mutation batch just because its ids happened to be canonical.
	if ctx.Err() != nil {
		fail := tools.Fail(codeCancelled, "Turn cancelled before terminal.moveToWorktree moved anything.", tools.Unrecoverable())
		return nil, &fail
	}
	if allCanonical {
		return ids, nil
	}

	if mcp == nil || !mcp.Connected() {
		return ids, nil // fail open — passthrough reports the dead link with its own hint
	}
	res, err := mcp.CallTool(ctx, "terminal.list", map[string]any{})
	// A cancelled turn must NOT fall through into the mutation path: failing open here
	// would have us start moving terminals for a turn the human already abandoned.
	if ctx.Err() != nil {
		fail := tools.Fail(codeCancelled, "Turn cancelled before terminal.moveToWorktree resolved any ids; nothing was moved.", tools.Unrecoverable())
		return nil, &fail
	}
	if err != nil || res.IsError {
		return ids, nil // fail open
	}
	live := terminalid.ParseListIDs(res.StructuredContent, res.Text)
	if len(live) == 0 {
		return ids, nil // fail open: an empty roster is also the hiccup symptom
	}

	// Guard SUBSTITUTION, not id shape. Expanding a prefix replaces an id AFTER the
	// human confirmed the raw args (dispatch shows them, and there is no pre-confirm
	// hook), so a request too weak to identify one terminal must be refused rather than
	// expanded — otherwise the intended terminal closing mid-approval can leave a
	// DIFFERENT one as the sole prefix match, and it moves, tab group and all, without
	// anyone approving it. Requiring the standard truncation (terminal- plus the first
	// 8 hex of the uuid) makes that collision negligible.
	//
	// An id present in the roster EXACTLY is never substituted, so it is never gated:
	// Daintree ids are not always terminal-<uuid> (agent.launch takes an unrestricted
	// requestedId that becomes the panel id verbatim), and gating on shape would make a
	// legitimately-named live terminal unreachable through this wrapper. Same reasoning
	// covers the fail-open paths above — they forward verbatim, so they substitute
	// nothing and need no guard.
	liveSet := make(map[string]struct{}, len(live))
	for _, l := range live {
		liveSet[strings.TrimSpace(l)] = struct{}{}
	}
	var vague []string
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if _, exact := liveSet[trimmed]; exact {
			continue
		}
		if !credibleTruncationRe.MatchString(trimmed) {
			vague = append(vague, id)
		}
	}
	if len(vague) > 0 {
		fail := tools.Fail(codeTerminalNotFound, fmt.Sprintf(
			"terminal id(s) %s match no live terminal exactly and are too short to expand unambiguously, so nothing was moved. Pass the EXACT, FULL id from terminal.list. Live terminals: %s.",
			join(vague, ", "), join(live, ", ")),
			tools.WithDetails(map[string]any{"tooShort": vague, "liveTerminals": live}))
		return nil, &fail
	}

	r := terminalid.Resolve(ids, live)
	if r.OK() {
		return r.Resolved, nil
	}
	var what []string
	if len(r.Unknown) > 0 {
		what = append(what, fmt.Sprintf("%s matched no live terminal", join(r.Unknown, ", ")))
	}
	if len(r.Ambiguous) > 0 {
		what = append(what, fmt.Sprintf("%s is an ambiguous prefix matching several terminals", join(r.Ambiguous, ", ")))
	}
	fail := tools.Fail(codeTerminalNotFound, fmt.Sprintf(
		"%s. Nothing was moved. Use the EXACT, FULL terminal id (e.g. terminal-5284bfef-3d11-424c-90cb-136f24046295) — never an abbreviated prefix. Live terminals: %s.",
		strings.Join(what, "; "), join(live, ", ")),
		tools.WithDetails(map[string]any{
			"resolved":      r.Resolved,
			"unknown":       r.Unknown,
			"ambiguous":     r.Ambiguous,
			"liveTerminals": live,
		}))
	return nil, &fail
}
