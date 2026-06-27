// Package terminalid canonicalizes caller-supplied Daintree terminal ids against the
// live roster. It exists because the model routinely TRUNCATES Daintree's full
// terminal-<uuid> ids to a short prefix (e.g. "terminal-5284bfef") — in its own prose,
// in scratch, and in tool calls. Daintree matches only the EXACT id, so a truncated id
// matches nothing: terminal.getStatus returns an empty set and the await/extract poll
// loops then grind to their cap reporting "still working" for agents that already
// finished. Mapping the prefix back to its canonical id makes the truncated form work,
// and a definitively-unknown id can be rejected loud-and-fast (naming the live ids)
// instead of a silent multi-minute wait.
//
// The pure resolution logic lives here so both the extraction family (terminal.awaitAll
// / terminal.extract*) and the context family (terminal.read / terminal.summarize) share
// ONE implementation — each supplies its own terminal.list roster source (it must NOT
// import internal/daemon, and the two families have different MCP seams). It mirrors the
// agenttaskx resolveWorktreeID precedent, with prefix EXPANSION instead of branch
// aliasing.
package terminalid

import (
	"encoding/json"
	"regexp"
	"strings"
)

// canonicalRe matches a full Daintree terminal id: "terminal-" followed by a canonical
// UUID. A truncated prefix (e.g. "terminal-5284bfef") is shorter and never matches, so it
// still gets resolved.
var canonicalRe = regexp.MustCompile(`^terminal-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// LooksCanonical reports whether id is already a full terminal-<uuid> id. A caller on a
// hot path may skip the terminal.list roster read for such an id and rely on the
// downstream not-found detection to catch a stale-but-full id — only a truncated/odd id
// then pays for a roster read. A truncated prefix never matches, so it is always resolved.
func LooksCanonical(id string) bool {
	return canonicalRe.MatchString(strings.TrimSpace(id))
}

// Resolution is the outcome of canonicalizing a batch of requested terminal ids.
type Resolution struct {
	// Resolved is the canonical id for each request that matched exactly one live
	// terminal, in input order, deduped (two requests mapping to the same canonical id
	// collapse to one). Only trustworthy when OK() — when Unknown/Ambiguous are non-empty
	// it holds only the requests that DID resolve.
	Resolved []string
	// Unknown holds the ORIGINAL requests that matched no live terminal.
	Unknown []string
	// Ambiguous holds the ORIGINAL requests whose prefix matched more than one live
	// terminal — the caller must pass the full id to disambiguate.
	Ambiguous []string
}

// OK reports whether every request resolved to exactly one live terminal.
func (r Resolution) OK() bool { return len(r.Unknown) == 0 && len(r.Ambiguous) == 0 }

// Resolve canonicalizes requested ids against the live roster. Matching is per request,
// in order:
//
//  1. exact id (request == a live id) — always wins, even when the request also happens
//     to be a prefix of a longer live id (an exact id is never lost to a coincidental
//     prefix collision);
//  2. unique prefix (exactly one live id has the request as a prefix) — the truncation
//     case;
//  3. zero prefix matches → Unknown; more than one → Ambiguous.
//
// Pure and side-effect-free: live is the canonical id set from terminal.list. Blank
// requests and blank live ids are ignored. Resolve does NOT decide what to do with an
// empty/unreadable roster — the caller does (it should FAIL OPEN and skip resolution
// there, since an empty read is also the transport-hiccup symptom, not "every id wrong").
func Resolve(requested, live []string) Resolution {
	liveSet := make(map[string]struct{}, len(live))
	cleanLive := make([]string, 0, len(live))
	for _, l := range live {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, dup := liveSet[l]; dup {
			continue
		}
		liveSet[l] = struct{}{}
		cleanLive = append(cleanLive, l)
	}

	var res Resolution
	seen := make(map[string]struct{}, len(requested))
	add := func(id string) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		res.Resolved = append(res.Resolved, id)
	}
	for _, req := range requested {
		r := strings.TrimSpace(req)
		if r == "" {
			continue
		}
		if _, ok := liveSet[r]; ok {
			add(r) // exact match wins outright
			continue
		}
		var matches []string
		for _, l := range cleanLive {
			if strings.HasPrefix(l, r) {
				matches = append(matches, l)
			}
		}
		switch len(matches) {
		case 1:
			add(matches[0])
		case 0:
			res.Unknown = append(res.Unknown, req)
		default:
			res.Ambiguous = append(res.Ambiguous, req)
		}
	}
	return res
}

// ParseListIDs extracts canonical terminal ids from a terminal.list result. It UNIONS
// the structuredContent payload and the JSON text body (Daintree returns results in the
// text block, so reading only one source can drop ids) and dedupes in first-seen order.
// Each entry's id is "id", or "terminalId" when "id" is absent. Never throws; returns nil
// when nothing parses — which the caller treats as an empty/unreadable roster (fail open).
func ParseListIDs(structured any, text string) []string {
	var ids []string
	seen := map[string]struct{}{}
	collect := func(entries []any) {
		for _, e := range entries {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if strings.TrimSpace(id) == "" {
				id, _ = m["terminalId"].(string)
			}
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if sc, ok := structured.(map[string]any); ok {
		if arr, ok := sc["terminals"].([]any); ok {
			collect(arr)
		}
	}
	if strings.TrimSpace(text) != "" {
		var parsed struct {
			Terminals []any `json:"terminals"`
		}
		if json.Unmarshal([]byte(text), &parsed) == nil {
			collect(parsed.Terminals)
		}
	}
	return ids
}

// ListEntry is one terminal.list row with the metadata fields the open-terminal
// inventory carries. It is the full-entry counterpart to ParseListIDs's id-only output —
// the ID-only path stays fast and untouched while the inventory path reuses the same
// union+dedup parse to keep one source of truth for terminal.list decoding.
type ListEntry struct {
	ID         string
	Kind       string
	WorktreeID string
	Title      string
	AgentID    string
	AgentState string
}

// ParseListEntries extracts full terminal.list rows (id + metadata) the same way
// ParseListIDs extracts ids: it UNIONS the structuredContent payload and the JSON text
// body (Daintree returns results in the text block, so reading only one source can drop
// rows) and dedupes by id in first-seen order. Each row's id is "id", or "terminalId"
// when "id" is absent; a row with no id is skipped. Never throws; returns nil when
// nothing parses (an empty/unreadable roster).
func ParseListEntries(structured any, text string) []ListEntry {
	var entries []ListEntry
	seen := map[string]struct{}{}
	collect := func(rows []any) {
		for _, e := range rows {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			if strings.TrimSpace(id) == "" {
				id, _ = m["terminalId"].(string)
			}
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			str := func(key string) string {
				s, _ := m[key].(string)
				return strings.TrimSpace(s)
			}
			entries = append(entries, ListEntry{
				ID:         id,
				Kind:       str("kind"),
				WorktreeID: str("worktreeId"),
				Title:      str("title"),
				AgentID:    str("agentId"),
				AgentState: str("agentState"),
			})
		}
	}
	if sc, ok := structured.(map[string]any); ok {
		if arr, ok := sc["terminals"].([]any); ok {
			collect(arr)
		}
	}
	if strings.TrimSpace(text) != "" {
		var parsed struct {
			Terminals []any `json:"terminals"`
		}
		if json.Unmarshal([]byte(text), &parsed) == nil {
			collect(parsed.Terminals)
		}
	}
	return entries
}
