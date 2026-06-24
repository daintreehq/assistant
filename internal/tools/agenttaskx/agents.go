package agenttaskx

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// agentRosterTimeout bounds the agentSettings.get read so a hung Daintree can't
// freeze a spawn for the MCP transport's (much longer) default timeout before the
// caller falls open. A configured-agents read is a cheap local lookup (single-digit
// ms in practice), so a few seconds is generous slack, not a real ceiling.
const agentRosterTimeout = 5 * time.Second

// ConfiguredAgentIDs reads Daintree's per-agent settings map (agentSettings.get,
// no args -> { agents: { <id>: {...} } }) and returns the configured agent ids,
// sorted. Returns nil on any error, timeout, a non-map payload, or an empty map so
// callers FAIL OPEN — a discovery hiccup must never block a spawn. Unions the
// structuredContent map and the JSON text body (Daintree returns results in text),
// mirroring parseTerminalList, so a divergence between the two sources can't drop ids.
//
// NOTE: this is the user-CONFIGURED subset, not Daintree's full launchable catalog
// (the built-in AGENT_REGISTRY is baked into Daintree and exposed by no MCP tool).
// It is exported so the composition root can also fetch it once at MCP-connect time to
// surface the roster in the startup runtime context — not just lazily at spawn time.
func ConfiguredAgentIDs(ctx context.Context, mcp MCPClient) []string {
	cctx, cancel := context.WithTimeout(ctx, agentRosterTimeout)
	defer cancel()
	res, err := mcp.CallTool(cctx, "agentSettings.get", map[string]any{})
	if err != nil || res.IsError {
		return nil
	}
	ids := map[string]struct{}{}
	add := func(id string) {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = struct{}{}
		}
	}
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if agents, ok := sc["agents"].(map[string]any); ok {
			for id := range agents {
				add(id)
			}
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed struct {
			Agents map[string]json.RawMessage `json:"agents"`
		}
		if json.Unmarshal([]byte(res.Text), &parsed) == nil {
			for id := range parsed.Agents {
				add(id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// resolveAgentID validates agentID against Daintree's configured agents. It returns
// ok=true (proceed) when the id is configured OR the list is unavailable/empty (fail
// open — never block a spawn on a discovery hiccup). When the id is definitively
// absent it returns ok=false with the available ids and the closest near-miss, so the
// caller can say "did you mean …". This is the ONLY place a typo'd or mis-transcribed
// agent name is caught: Daintree's agent.launch does NOT validate agentId — it
// silently launches a terminal that immediately detaches with zero output.
func resolveAgentID(ctx context.Context, mcp MCPClient, agentID string) (ok bool, available []string, suggestion string) {
	available = ConfiguredAgentIDs(ctx, mcp)
	if len(available) == 0 {
		return true, nil, "" // fail open: unknown roster, let the launch proceed
	}
	for _, id := range available {
		if id == agentID {
			return true, available, ""
		}
	}
	return false, available, closestAgentID(agentID, available)
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
