package agenttaskx

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// agentRosterTimeout bounds the agent.listAvailable read so a hung Daintree can't
// freeze a spawn for the MCP transport's (much longer)
// default timeout before the caller falls open. A configured-agents read is a cheap
// local lookup (single-digit ms in practice), so a few seconds is generous slack, not a
// real ceiling. A var (not const) only so tests can shorten it.
var agentRosterTimeout = 5 * time.Second

type availableAgent struct {
	ID           string `json:"id"`
	Availability string `json:"availability"`
	Launchable   *bool  `json:"launchable"`
}

// availableAgentRoster reads Daintree's narrow effective direct-agent registry. Registry
// membership and launchability are intentionally separate: missing/installed/blocked rows
// are useful discovery results but must not be mistaken for runnable editing agents.
// Returns nil on any read/shape failure so callers fail open when discovery itself is
// unavailable. Text and structured result channels are unioned by exact id.
func availableAgentRoster(ctx context.Context, mcp MCPClient) []availableAgent {
	// Bound the read with a CANCEL-based deadline, NOT context.WithTimeout. The shared
	// mcp.Client degrades (tears down) the connection on any non-abort CallTool error,
	// and a context.DeadlineExceeded is NOT treated as an abort — only context.Canceled
	// is (mcp.isAborted). A roster read is best-effort and must never degrade a working
	// connection just because it was slow, so a timeout here surfaces as a cancel; the
	// caller still falls open to nil.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(agentRosterTimeout, cancel)
	defer timer.Stop()
	res, err := mcp.CallTool(cctx, "agent.listAvailable", map[string]any{})
	if err != nil || res.IsError {
		return nil
	}
	rowsByID := map[string]availableAgent{}
	add := func(row availableAgent) {
		if strings.TrimSpace(row.ID) == "" {
			return
		}
		existing := rowsByID[row.ID]
		existing.ID = row.ID
		if availability := strings.TrimSpace(row.Availability); availability != "" {
			existing.Availability = availability
		}
		if row.Launchable != nil {
			existing.Launchable = row.Launchable
		}
		rowsByID[row.ID] = existing
	}
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if raw, present := sc["agents"]; present {
			encoded, marshalErr := json.Marshal(raw)
			var rows []availableAgent
			if marshalErr == nil && json.Unmarshal(encoded, &rows) == nil {
				for _, row := range rows {
					add(row)
				}
			}
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed struct {
			Agents []availableAgent `json:"agents"`
		}
		if json.Unmarshal([]byte(res.Text), &parsed) == nil {
			for _, row := range parsed.Agents {
				add(row)
			}
		}
	}
	if len(rowsByID) == 0 {
		return nil
	}
	out := make([]availableAgent, 0, len(rowsByID))
	for _, row := range rowsByID {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RegisteredAgentIDs is the sorted registry-membership view used by diagnostics/tests.
// It includes known-unlaunchable rows; resolveAgentID performs the launchability gate.
func RegisteredAgentIDs(ctx context.Context, mcp MCPClient) []string {
	roster := availableAgentRoster(ctx, mcp)
	if len(roster) == 0 {
		return nil
	}
	ids := make([]string, len(roster))
	for i, row := range roster {
		ids[i] = row.ID
	}
	return ids
}

// resolveAgentID validates both membership and the host's known launchability state.
// An unreadable roster or omitted availability fails open; an explicit false or one of
// missing/installed/blocked fails closed before agent.launch can return a diagnostic setup
// panel that looks superficially like a successfully spawned terminal.
func resolveAgentID(ctx context.Context, mcp MCPClient, agentID string) (ok bool, available []string, suggestion, unavailableState string) {
	roster := availableAgentRoster(ctx, mcp)
	if len(roster) == 0 {
		return true, nil, "", "" // fail open: unknown roster, let the launch proceed
	}
	available = make([]string, len(roster))
	for i, row := range roster {
		available[i] = row.ID
	}
	for _, row := range roster {
		if row.ID != agentID {
			continue
		}
		if row.Launchable != nil {
			if *row.Launchable {
				return true, available, "", ""
			}
			state := row.Availability
			if state == "" {
				state = "not-launchable"
			}
			return false, available, "", state
		}
		switch row.Availability {
		case "ready", "unauthenticated", "":
			return true, available, "", ""
		case "missing", "installed", "blocked":
			return false, available, "", row.Availability
		default:
			return true, available, "", "" // forward-compatible unknown state
		}
	}
	return false, available, closestAgentID(agentID, available), ""
}

// closestAgentID returns the candidate with the smallest case-insensitive edit
// distance to target, but only when it is a plausible near-miss (distance <=
// max(2, len(target)/3)) — so a mis-transcription like "antiravity" suggests
// "antigravity" (distance 1) while a wholly unrelated string suggests nothing.
func closestAgentID(target string, candidates []string) string {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return ""
	}
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		if d := levenshtein(t, strings.ToLower(c)); d < bestDist {
			best, bestDist = c, d
		}
	}
	threshold := len(t) / 3
	if threshold < 2 {
		threshold = 2
	}
	if bestDist > threshold {
		return ""
	}
	return best
}

// levenshtein is the standard two-row edit distance (insert/delete/substitute = 1).
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur := make([]int, len(br)+1)
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
