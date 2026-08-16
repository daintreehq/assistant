package agenttaskx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

const (
	// agentLaunchNameMaxLen caps the human-readable agent.launch name (tab label).
	agentLaunchNameMaxLen = 60
	// defaultAgentID is the name prefix when the caller omits an agentId.
	defaultAgentID = "claude"
)

// editConstraintsBlock is appended to an edit-mode spawned-agent prompt.
const editConstraintsBlock = "Make changes only in this worktree. Do not modify unrelated files. Run relevant tests if practical. Report back changed files, tests run, remaining risks. If you need clarification, stop and ask."

// exploreConstraintsBlock is appended to an explore-mode (read-only) prompt. It
// expresses ONLY the read-only guardrail + ambiguity guidance — it must NOT dictate
// WHAT to investigate. The taskPrompt already says what to do; assuming every explore
// is a codebase survey ("report the project's structure, key components, risks…")
// contradicts any non-codebase task the assistant delegates (e.g. "give me one
// interesting fact"), producing a self-contradictory prompt. Keep it task-neutral.
const exploreConstraintsBlock = "This is a read-only task: do not create, modify, or delete any files, and do not run commands that mutate state. Do exactly what the task above asks, then report back. If the task is ambiguous, state your assumptions and proceed; only stop to ask if you are genuinely blocked."

var wsRun = regexp.MustCompile(`\s+`)

// buildAgentPrompt composes the agent prompt from the task, caller-supplied
// context hints, the mode-specific constraints block, and the acceptance
// criteria.
func buildAgentPrompt(a *spawnArgs) string {
	lines := []string{strings.TrimSpace(a.TaskPrompt)}
	var ctxLines []string
	if a.WorktreeID != "" {
		ctxLines = append(ctxLines, "Work in worktree: "+a.WorktreeID)
	}
	if a.Context != nil {
		var files []string
		for _, f := range a.Context.FilePaths {
			if strings.TrimSpace(f) != "" {
				files = append(files, f)
			}
		}
		if len(files) > 0 {
			bullets := make([]string, len(files))
			for i, f := range files {
				bullets[i] = "  - " + f
			}
			ctxLines = append(ctxLines, "Relevant files:\n"+strings.Join(bullets, "\n"))
		}
		if a.Context.IncludeDiff {
			ctxLines = append(ctxLines, "Review the current working-tree diff in this worktree before changing anything.")
		}
	}
	if len(ctxLines) > 0 {
		lines = append(lines, "\nContext:\n"+strings.Join(ctxLines, "\n"))
	}
	constraints := editConstraintsBlock
	if a.Mode == "explore" {
		constraints = exploreConstraintsBlock
	}
	lines = append(lines, "\n"+constraints)
	if criteria := strings.TrimSpace(a.AcceptanceCriteria); criteria != "" {
		lines = append(lines, "\nAcceptance criteria (your work is verified against these — state clearly when each is met):\n"+criteria)
	}
	return strings.Join(lines, "\n")
}

// resolveLaunchAgentID applies the same trim/default spawn() does, so every
// title/name derivation keys off ONE id — the raw model spelling, since
// resolveAgentID only canonicalizes later (spawn.go).
func resolveLaunchAgentID(agentID string) string {
	id := strings.TrimSpace(agentID)
	if id == "" {
		id = defaultAgentID
	}
	return id
}

// normalizeTaskTitle collapses whitespace runs and strips any leading
// "<agentId>:" label the caller wrote into the title itself.
//
// The tab the user sees is already "<Agent>: <title>", and every surface that
// describes `title` shows that RENDERED label — so the model copies it into the
// argument ("Claude: prs merge target") and the wrapper prefixes it again,
// producing "Claude: Claude: prs merge target" (issue #337). Stripping here,
// rather than skipping the prepend, keeps buildAgentLaunchName's cap arithmetic
// a single code path: a title that already carries the prefix rejoins the exact
// add-and-truncate flow an unprefixed one takes, so it can never be double-cut
// and non-matching titles stay byte-identical to before — they ARE dedupe
// identity (computeIdempotencyKey embeds the name, reconcileViaTerminalList
// matches it exactly).
//
// The match is deliberately narrow: the FIRST colon-delimited token must equal
// the resolved agentId under EqualFold. Case and a missing space after the colon
// are tolerated (the colon is the delimiter that matters); a repeated prefix is
// collapsed by looping, since each pass must clear the same bar. A space BEFORE
// the colon, a different agent's name ("Claude: …" launched as codex), or the id
// without a colon are all left alone — those are plausible task text, not the
// wrapper's syntax. Returns "" for a title that is nothing but prefixes;
// callers decide the fallback.
func normalizeTaskTitle(title, agentID string) string {
	id := resolveLaunchAgentID(agentID)
	task := strings.TrimSpace(wsRun.ReplaceAllString(title, " "))
	for {
		label, rest, found := strings.Cut(task, ":")
		if !found || !strings.EqualFold(label, id) {
			return task
		}
		task = strings.TrimSpace(rest)
	}
}

// buildAgentLaunchName derives the "<Agent>: <task>" terminal/tab label, hard-
// capped at agentLaunchNameMaxLen so the "<Agent>: " prefix always survives.
// Exactly one prefix: a title that already carries it is normalized away first
// (see normalizeTaskTitle).
func buildAgentLaunchName(title, agentID string) string {
	id := resolveLaunchAgentID(agentID)
	prefix := strings.ToUpper(id[:1]) + id[1:] + ": "
	task := normalizeTaskTitle(title, id)
	if task == "" {
		task = "task"
	}
	room := agentLaunchNameMaxLen - len(prefix)
	if room < 0 {
		room = 0
	}
	head := task
	if len(task) > room {
		head = task[:room]
	}
	// Final hard cap so the invariant holds even for a pathologically long agentId.
	full := prefix + head
	if len(full) > agentLaunchNameMaxLen {
		full = full[:agentLaunchNameMaxLen]
	}
	return full
}

// fieldRe builds the text-body regex extractField uses ("?key"? : "?value"?).
func fieldRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`"?` + regexp.QuoteMeta(key) + `"?\s*[:=]\s*"?([\w.-]+)"?`)
}

// extractField robustly pulls a named string field from an MCP launch result:
// structuredContent (direct, or nested under task/agent/result/data), else a
// regex on the text body. Returns "" when absent.
func extractField(res MCPCallResult, key string) string {
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if direct, ok := sc[key].(string); ok && direct != "" {
			return direct
		}
		for _, nestedKey := range []string{"task", "agent", "result", "data"} {
			if nested, ok := sc[nestedKey].(map[string]any); ok {
				if v, ok := nested[key].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	if res.Text != "" {
		if m := fieldRe(key).FindStringSubmatch(res.Text); m != nil {
			return m[1]
		}
	}
	return ""
}

// reconcileTerminal is one terminal.list entry with the reconciliation fields.
type reconcileTerminal struct {
	id         string
	name       string
	agentID    string
	worktreeID string
}

// parseTerminalList parses terminal.list entries from either the structured
// payload or a JSON text body, preserving the reconciliation fields. Never throws.
func parseTerminalList(res MCPCallResult) []reconcileTerminal {
	var entries []any
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if arr, ok := sc["terminals"].([]any); ok {
			entries = append(entries, arr...)
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var parsed struct {
			Terminals []any `json:"terminals"`
		}
		if err := json.Unmarshal([]byte(res.Text), &parsed); err == nil {
			entries = append(entries, parsed.Terminals...)
		}
	}
	var out []reconcileTerminal
	for _, t := range entries {
		e, ok := t.(map[string]any)
		if !ok {
			continue
		}
		id, _ := e["id"].(string)
		if id == "" {
			id, _ = e["terminalId"].(string)
		}
		if id == "" {
			continue
		}
		name, _ := e["name"].(string)
		agentID, _ := e["agentId"].(string)
		worktreeID, _ := e["worktreeId"].(string)
		out = append(out, reconcileTerminal{id: id, name: name, agentID: agentID, worktreeID: worktreeID})
	}
	return out
}

// reconcileViaTerminalList answers "did the previous attempt actually start an
// agent?" by reading terminal.list and matching the deterministic launch name (and
// agentId/worktreeId when present). Binds ONLY on a single unambiguous match;
// returns "" otherwise. Never throws.
func reconcileViaTerminalList(ctx context.Context, mcp MCPClient, name, agentID, worktreeID string) string {
	res, err := mcp.CallTool(ctx, "terminal.list", map[string]any{})
	if err != nil || res.IsError {
		return ""
	}
	var matches []reconcileTerminal
	// parseTerminalList can yield the SAME terminal twice (once from the structured
	// payload, once from the JSON text body) — without id-dedup a lone true match
	// counts as two and is wrongly rejected as ambiguous.
	seen := make(map[string]struct{})
	for _, t := range parseTerminalList(res) {
		if _, dup := seen[t.id]; dup {
			continue
		}
		seen[t.id] = struct{}{}
		if t.name != name {
			continue
		}
		if t.agentID != "" && t.agentID != agentID {
			continue
		}
		if worktreeID != "" && t.worktreeID != "" && t.worktreeID != worktreeID {
			continue
		}
		matches = append(matches, t)
	}
	if len(matches) == 1 {
		return matches[0].id
	}
	return ""
}

// computeIdempotencyKey derives a deterministic 16-hex key over the EXACT arguments
// forwarded to agent.launch (the composed prompt, worktreeId|"", agentId, and the
// terminal name). Daintree dedupes on this key as requestKey AND rejects a reuse with
// different arguments ("same requestKey, different arguments"), so the key must mirror
// those args faithfully — keying a hand-picked subset let a title tweak (which changes
// `name`) reuse the key and hard-collide. mode/context/acceptanceCriteria fold in via
// `prompt`. Keys are sorted for a canonical JSON before sha256 (a flat object, so a
// key-sort suffices).
func computeIdempotencyKey(prompt, worktreeID, agentID, name string) string {
	parts := map[string]string{
		"prompt":     prompt,
		"worktreeId": worktreeID,
		"agentId":    agentID,
		"name":       name,
	}
	keys := make([]string, 0, len(parts))
	for k := range parts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Build the canonical {"agentId":..,"mode":..,..} object manually, matching
	// JSON.stringify of an entries-sorted object (string values, so json.Marshal
	// of each value gives the same escaping JS uses).
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(parts[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// ptr returns a pointer to a non-empty string, or nil for an empty one (for the
// optional *string record fields).
func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
