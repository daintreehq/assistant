package agenttaskx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// worktreeRosterTimeout bounds the worktree.list read so a hung Daintree can't freeze a
// spawn for the MCP transport's (much longer) default timeout. A worktree list is a cheap
// local lookup, so a few seconds is generous slack. A var (not const) only so tests can
// shorten it. Mirrors agentRosterTimeout.
var worktreeRosterTimeout = 5 * time.Second

// worktreeInfo is the slice of a worktree.list entry resolveWorktreeID needs: the
// canonical id (which Daintree keys worktrees by — a filesystem path), the path, and the
// branch (what a model most often confuses for the id).
type worktreeInfo struct {
	id     string
	path   string
	branch string
}

// listWorktrees reads Daintree's worktree.list (no args -> { worktrees: [{id,path,branch,
// isActive,isMain}] }) and returns the entries, deduped by id. Returns nil on any error,
// timeout, a non-list payload, or an empty list so callers FAIL OPEN — a discovery hiccup
// must never block a spawn. Unions the structuredContent payload and the JSON text body
// (Daintree returns results in text), mirroring ConfiguredAgentIDs / parseTerminalList, so
// a divergence between the two sources can't drop a worktree.
func listWorktrees(ctx context.Context, mcp MCPClient) []worktreeInfo {
	// Bound the read with a CANCEL-based deadline, NOT context.WithTimeout: the shared
	// mcp.Client degrades (tears down) the connection on a non-abort CallTool error, and a
	// context.DeadlineExceeded is NOT treated as an abort (only context.Canceled is). A
	// best-effort read must never degrade a working connection just because it was slow, so
	// a timeout here surfaces as a cancel; the caller still falls open to nil.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(worktreeRosterTimeout, cancel)
	defer timer.Stop()
	res, err := mcp.CallTool(cctx, "worktree.list", map[string]any{})
	if err != nil || res.IsError {
		return nil
	}

	byID := map[string]*worktreeInfo{}
	var order []string
	// Merge by id across both sources: the first occurrence sets the entry and its position,
	// a later duplicate only BACKFILLS fields the first source left blank — so a structured
	// entry carrying just an id still picks up a branch/path that arrives in the text body
	// (without that merge, branch-name resolution would silently break for such a worktree).
	add := func(id, path, branch string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		path, branch = strings.TrimSpace(path), strings.TrimSpace(branch)
		if w, dup := byID[id]; dup {
			if w.path == "" {
				w.path = path
			}
			if w.branch == "" {
				w.branch = branch
			}
			return
		}
		byID[id] = &worktreeInfo{id: id, path: path, branch: branch}
		order = append(order, id)
	}

	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if arr, ok := sc["worktrees"].([]any); ok {
			for _, e := range arr {
				if m, ok := e.(map[string]any); ok {
					id, _ := m["id"].(string)
					path, _ := m["path"].(string)
					branch, _ := m["branch"].(string)
					add(id, path, branch)
				}
			}
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed struct {
			Worktrees []struct {
				ID     string `json:"id"`
				Path   string `json:"path"`
				Branch string `json:"branch"`
			} `json:"worktrees"`
		}
		if json.Unmarshal([]byte(res.Text), &parsed) == nil {
			for _, w := range parsed.Worktrees {
				add(w.ID, w.Path, w.Branch)
			}
		}
	}
	out := make([]worktreeInfo, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// resolveWorktreeID validates and NORMALIZES a caller-supplied worktreeId against
// Daintree's real worktrees. It is the worktree analogue of resolveAgentID, and exists for
// the same reason: Daintree's agent.launch does NOT validate worktreeId — it silently
// returns a terminal-less success when the id can't be resolved (e.g. a model passing the
// BRANCH name "main" where the worktree id — a path — belongs; see daintreehq/daintree
// issue #10812), so the whole spawn fails invisibly and the model retries into the void.
//
// Returns:
//   - empty input -> ("", true, nil, "")           omitted; Daintree runs in the active worktree
//   - id/path/branch match -> (canonicalId, true)  a branch/path match MAPS to the worktree's id
//   - unreadable/empty roster -> (input, true)     fail open: a discovery hiccup never blocks a spawn
//   - definitively unknown -> ("", false, available, suggestion)
func resolveWorktreeID(ctx context.Context, mcp MCPClient, worktreeID string) (resolved string, ok bool, available []worktreeInfo, suggestion string) {
	worktreeID = strings.TrimSpace(worktreeID)
	if worktreeID == "" {
		return "", true, nil, "" // omitted: Daintree picks the active worktree
	}
	available = listWorktrees(ctx, mcp)
	if len(available) == 0 {
		return worktreeID, true, nil, "" // fail open: unreadable roster, let the launch proceed
	}
	lower := strings.ToLower(worktreeID)
	// Precedence, each pass total over the list before the next: an exact id/path wins
	// outright; then a case-insensitive id/path (macOS paths are case-insensitive, and a
	// case-only difference is far more likely the SAME worktree than a branch alias); only
	// then a branch-name match, which maps to the worktree's canonical id (the "main" case).
	// An exact id is therefore never lost to a coincidental branch collision.
	for _, w := range available {
		if w.id == worktreeID || (w.path != "" && w.path == worktreeID) {
			return w.id, true, available, ""
		}
	}
	for _, w := range available {
		if strings.ToLower(w.id) == lower || (w.path != "" && strings.ToLower(w.path) == lower) {
			return w.id, true, available, ""
		}
	}
	// Branch match. Git allows only one worktree per branch, but guard against a duplicate
	// (or case-only branch alias) so an ambiguous branch never silently spawns in the wrong
	// worktree — fall through to the reject + available list instead, where the model picks
	// the exact id.
	var branchMatch []string
	for _, w := range available {
		if w.branch != "" && strings.ToLower(w.branch) == lower {
			branchMatch = append(branchMatch, w.id)
		}
	}
	if len(branchMatch) == 1 {
		return branchMatch[0], true, available, ""
	}
	return "", false, available, closestWorktreeID(worktreeID, available)
}

// closestWorktreeID returns the id of the worktree whose branch/id is the closest
// case-insensitive edit-distance near-miss to target, or "" when nothing is plausibly
// close (distance <= max(2, len(target)/3)). Mirrors closestAgentID.
func closestWorktreeID(target string, candidates []worktreeInfo) string {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return ""
	}
	bestID, bestDist := "", 1<<30
	for _, w := range candidates {
		for _, cand := range []string{w.branch, w.id} {
			if cand == "" {
				continue
			}
			if d := levenshtein(t, strings.ToLower(cand)); d < bestDist {
				bestID, bestDist = w.id, d
			}
		}
	}
	threshold := len(t) / 3
	if threshold < 2 {
		threshold = 2
	}
	if bestDist > threshold {
		return ""
	}
	return bestID
}

// formatWorktrees renders the available worktrees as "branch (id)" entries for an error
// message, so the model sees both the human label and the value it must actually pass.
func formatWorktrees(available []worktreeInfo) string {
	parts := make([]string, 0, len(available))
	for _, w := range available {
		switch {
		case w.branch != "":
			parts = append(parts, fmt.Sprintf("%s (%s)", w.branch, w.id))
		default:
			parts = append(parts, w.id)
		}
	}
	return strings.Join(parts, ", ")
}

// worktreesDetail renders the available worktrees as structured rows for a Fail's details,
// so a tool caller gets the machine-usable {id, path, branch} (not just the formatted
// string), mirroring how the unknown-agent reject surfaces its available roster.
func worktreesDetail(available []worktreeInfo) []map[string]any {
	out := make([]map[string]any, 0, len(available))
	for _, w := range available {
		out = append(out, map[string]any{"id": w.id, "path": w.path, "branch": w.branch})
	}
	return out
}
