package app

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/prompts"
)

// mcpTurnReconnectInterval throttles ensureStartupForTurn's own mid-session recovery
// attempt (see mcpEverConnected on App) to at most one bounded ReconnectMcp handshake
// per interval, so a sustained outage costs one capped 8s connect attempt every 30s of
// turns rather than one on every single turn. Mirrors the supervisor daemon's own
// mcpReconnectEvery (internal/supervisor/runtime.go) in spirit — the two intervals need
// not match exactly since the daemon and an attached session never run at once (see
// CLAUDE.md "Single-owner, durable supervision").
const mcpTurnReconnectInterval = 30 * time.Second

// startupReadTimeout bounds every best-effort Daintree snapshot read. Use a plain
// cancellation timer rather than context.WithTimeout: the shared MCP client treats
// DeadlineExceeded as a transport failure, while Canceled is an intentional abort that
// must not degrade a healthy connection.
const startupReadTimeout = 5 * time.Second

// refreshStartupContext fetches the stable project/agent snapshot and current
// worktree concurrently. Interactive boot calls this while the logo is animating; a
// reconnect replaces the whole cache atomically only after all reads settle. Failures are
// represented as nil snapshots, never stale values from a prior connection.
func (a *App) refreshStartupContext(ctx context.Context, connected bool) {
	a.updateStartupContext(ctx, connected, true)
}

func (a *App) ensureStartupContext(ctx context.Context, connected bool) {
	a.updateStartupContext(ctx, connected, false)
}

func (a *App) updateStartupContext(ctx context.Context, connected, force bool) {
	a.startupRefreshMu.Lock()
	defer a.startupRefreshMu.Unlock()

	if !connected {
		a.startupMu.Lock()
		a.startupGeneration++
		a.cachedProject = nil
		a.cachedAgents = nil
		a.cachedWorktree = nil
		a.startupReady = false
		a.startupMu.Unlock()
		return
	}
	if !force {
		a.startupMu.RLock()
		ready := a.startupReady
		a.startupMu.RUnlock()
		if ready {
			return
		}
	}
	a.startupMu.Lock()
	a.startupGeneration++
	generation := a.startupGeneration
	a.startupMu.Unlock()

	projectCh := make(chan *prompts.ProjectContext, 1)
	agentsCh := make(chan *prompts.AgentRosterContext, 1)
	worktreeCh := make(chan *prompts.WorktreeContext, 1)
	go func() { projectCh <- a.fetchProjectContext(ctx) }()
	go func() { agentsCh <- a.fetchAgentRoster(ctx) }()
	go func() { worktreeCh <- a.fetchWorktreeContext(ctx) }()

	project := <-projectCh
	agents := <-agentsCh
	worktree := <-worktreeCh
	a.startupMu.Lock()
	if a.startupGeneration != generation {
		a.startupMu.Unlock()
		return
	}
	a.cachedProject = project
	a.cachedAgents = agents
	a.cachedWorktree = worktree
	// A cancelled launch must not count as ready: a later bootstrap or first submit can
	// retry. Ordinary read failures with a live parent context are an explicit degraded
	// snapshot and should fail open rather
	// than stalling every later user turn on repeated discovery attempts.
	a.startupReady = ctx.Err() == nil
	a.startupMu.Unlock()
}

// ensureStartupForTurn joins the splash prefetch when it is still running, performs
// the same bounded discovery itself when no live boot attempt exists, or recovers a
// connection that was live earlier THIS process and has since died mid-session. Thus
// the first backend request never races an empty boot cache merely because the user
// submitted before the bootstrap command completed — and a turn hours into an attached
// session, whose MCP session Daintree evicted out from under it, gets a real chance to
// reconnect instead of running every remaining turn against a client that already knows
// it is dead (the interactive host runs no background reconnect loop of its own — that
// is the supervisor daemon's job, and the daemon does not run while a session is
// attached, see CLAUDE.md "Single-owner, durable supervision").
func (a *App) ensureStartupForTurn(ctx context.Context) {
	// Taking the lifecycle gate first joins an in-flight splash/bootstrap connect. Read
	// all gate state while holding it so a disconnected, already-attempted host fails open
	// on later turns instead of repeating an unbounded network handshake every time.
	a.mcpLifecycleMu.Lock()
	a.startupMu.RLock()
	ready := a.startupReady
	a.startupMu.RUnlock()
	status := a.MCP.Status()
	attempted := a.startupConnectAttempted
	// A boot that never connected at all fails open PERMANENTLY by design — see
	// ConnectMcp's own comment — because there is no evidence a retry would do
	// anything a plain user-issued /reconnect couldn't. But a session that DID
	// connect and has since gone quiet is different: credential revocation is the
	// one terminal case (Daintree rotated/revoked the bearer; reconnecting with the
	// same dead token would just fail again and risks tripping the host's abuse
	// policy) — everything else is exactly the "gone mid-session, worth another
	// try" case this exists for, throttled so a sustained outage cannot turn every
	// turn into an 8s-capped handshake attempt.
	shouldRetryDead := attempted &&
		!status.Connected &&
		a.mcpEverConnected &&
		!mcp.IsCredentialTerminalStatus(status.Error) &&
		time.Since(a.lastMcpReconnectAttempt) >= mcpTurnReconnectInterval
	if shouldRetryDead {
		// Stamped INSIDE the lock, before releasing it: two turns racing this check
		// concurrently must not both pass the throttle and both fire a reconnect.
		a.lastMcpReconnectAttempt = time.Now()
	}
	a.mcpLifecycleMu.Unlock()

	if ready && status.Connected {
		return
	}
	if !attempted {
		// The prior attempt was cancelled externally (or no boot path ran), so this
		// live turn gets one bounded retry before the first backend request.
		a.ConnectMcp(ctx)
		return
	}
	if shouldRetryDead {
		// Re-validate under the gate immediately before firing: a concurrent manual
		// /reconnect or /doctor could have already re-established a fresh session in
		// the window since this decision was made above. ReconnectMcp always tears
		// down whatever session is live and re-handshakes UNCONDITIONALLY (that is
		// what a forced reconnect means), so calling it onto a session someone else
		// JUST fixed would undo their fix and pay a second needless handshake. A
		// turn's own context dying in the same window makes the attempt pointless
		// for the same reason ConnectMcp's cancelled-launch path already skips it.
		a.mcpLifecycleMu.Lock()
		stillDead := !a.MCP.Status().Connected
		a.mcpLifecycleMu.Unlock()
		if stillDead && ctx.Err() == nil {
			a.ReconnectMcp(ctx)
		}
	}
}

// refreshCurrentWorktree re-reads the live renderer selection and publishes successful
// results into the shared snapshot. It is the Session's CurrentWorktreeFetcher: invoked
// on the session's DETACHED worktree refresher (TTL-gated, never inline on a model
// round) with the app-scoped background context — deadline-free by contract, since
// callStartupRead self-bounds with a plain cancel timer (a ctx deadline would make
// mcp.Client tear down the shared transport). A nil return is an unavailable read;
// `{Present:false}` is Daintree reporting no resolvable current row.
func (a *App) refreshCurrentWorktree(ctx context.Context) *prompts.WorktreeContext {
	a.startupMu.RLock()
	generation := a.startupGeneration
	a.startupMu.RUnlock()
	worktree := a.fetchWorktreeContext(ctx)
	if worktree == nil {
		return nil
	}
	a.startupMu.Lock()
	if a.startupGeneration != generation {
		a.startupMu.Unlock()
		return nil
	}
	a.cachedWorktree = worktree
	a.startupMu.Unlock()
	return worktree
}

func (a *App) fetchProjectContext(ctx context.Context) *prompts.ProjectContext {
	if res, ok := a.callStartupRead(ctx, "project.getCurrent"); ok {
		obj := mergedResultObject(res)
		if raw, present := obj["project"]; present {
			if raw == nil {
				return nil
			}
			var row struct {
				ID                    string `json:"id"`
				Name                  string `json:"name"`
				Path                  string `json:"path"`
				Status                string `json:"status"`
				DaintreeConfigPresent *bool  `json:"daintreeConfigPresent"`
				InRepoSettings        *bool  `json:"inRepoSettings"`
			}
			if decodeAny(raw, &row) {
				return &prompts.ProjectContext{
					ID:                    strings.TrimSpace(row.ID),
					Name:                  strings.TrimSpace(row.Name),
					Path:                  strings.TrimSpace(row.Path),
					Status:                strings.TrimSpace(row.Status),
					DaintreeConfigPresent: row.DaintreeConfigPresent,
					InRepoSettings:        row.InRepoSettings,
				}
			}
		}
	}

	return nil
}

func (a *App) fetchAgentRoster(ctx context.Context) *prompts.AgentRosterContext {
	if res, ok := a.callStartupRead(ctx, "agent.listAvailable"); ok {
		if rows, complete, availabilityComplete, parsed := parseAvailableAgents(res); parsed {
			return &prompts.AgentRosterContext{
				Agents:               rows,
				Complete:             complete,
				AvailabilityComplete: availabilityComplete,
				TotalCount:           len(rows),
			}
		}
	}

	return nil
}

func parseAvailableAgents(res mcp.CallResult) ([]prompts.AgentContext, bool, bool, bool) {
	type rawAgent struct {
		ID             string `json:"id"`
		DisplayName    string `json:"displayName"`
		Source         string `json:"source"`
		Availability   string `json:"availability"`
		Installed      *bool  `json:"installed"`
		Launchable     *bool  `json:"launchable"`
		Pinned         *bool  `json:"pinned"`
		ToolbarVisible *bool  `json:"toolbarVisible"`
	}
	var complete, availabilityComplete, parsed bool
	byID := map[string]prompts.AgentContext{}
	order := make([]string, 0)
	addRows := func(raw any) bool {
		if raw == nil {
			return false
		}
		var rows []rawAgent
		if !decodeAny(raw, &rows) {
			return false
		}
		for _, raw := range rows {
			id := raw.ID
			if strings.TrimSpace(id) == "" || id == "daintree-assistant" {
				continue
			}
			row, exists := byID[id]
			if !exists {
				row.ID = id
				order = append(order, id)
			}
			if value := strings.TrimSpace(raw.DisplayName); value != "" {
				row.DisplayName = value
			}
			if value := strings.TrimSpace(raw.Source); value != "" {
				row.Source = value
			}
			if value := strings.TrimSpace(raw.Availability); value != "" {
				row.Availability = value
			}
			if raw.Installed != nil {
				row.Installed = raw.Installed
			}
			if raw.Launchable != nil {
				row.Launchable = raw.Launchable
			}
			if raw.Pinned != nil {
				row.Pinned = raw.Pinned
			}
			if raw.ToolbarVisible != nil {
				row.ToolbarVisible = raw.ToolbarVisible
			}
			byID[id] = row
		}
		return true
	}
	applyObject := func(obj map[string]any) {
		if raw, present := obj["agents"]; present {
			parsed = addRows(raw) || parsed
		}
		if value, ok := obj["complete"].(bool); ok {
			complete = value
		}
		if value, ok := obj["availabilityComplete"].(bool); ok {
			availabilityComplete = value
		}
	}
	if strings.TrimSpace(res.Text) != "" {
		var obj map[string]any
		if json.Unmarshal([]byte(res.Text), &obj) == nil {
			applyObject(obj)
		}
	}
	if obj, ok := res.StructuredContent.(map[string]any); ok {
		applyObject(obj) // structured values update text rows without dropping text-only ids
	}
	if !parsed {
		return nil, false, false, false
	}
	rows := make([]prompts.AgentContext, 0, len(order))
	for _, id := range order {
		row := byID[id]
		if row.Installed == nil && row.Availability != "" {
			value := availabilityInstalled(row.Availability)
			row.Installed = &value
		}
		if row.Launchable == nil && row.Availability != "" {
			value := row.Availability == "ready" || row.Availability == "unauthenticated"
			row.Launchable = &value
		}
		rows = append(rows, row)
	}
	// Source first, id as the tiebreak. Daintree's discovery order is only stable WITHIN a
	// session (plugin scans race, registry walks differ per machine), and this roster rides
	// the cacheable startup block that sits ahead of the entire conversation — so an
	// order-only reshuffle of an otherwise unchanged registry would rewrite that prefix and
	// throw the prompt cache away. Sorting the fully merged rows (never `order`, which is
	// mid-accumulation: a text row's empty source is often filled by a later structured one)
	// also makes the projection's byte budget deterministic about WHICH rows survive a
	// catalog that overflows it. Ids are unique by construction here, so this is a total
	// order and needs no stable sort.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Source != rows[j].Source {
			return rows[i].Source < rows[j].Source
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, complete, availabilityComplete, true
}

func availabilityInstalled(state string) bool {
	switch state {
	case "installed", "ready", "blocked", "unauthenticated":
		return true
	default:
		return false
	}
}

func (a *App) fetchWorktreeContext(ctx context.Context) *prompts.WorktreeContext {
	res, ok := a.callStartupRead(ctx, "worktree.getCurrent")
	if !ok {
		return nil
	}
	obj := mergedResultObject(res)
	raw, present := obj["worktree"]
	if !present {
		return nil
	}
	if raw == nil {
		return &prompts.WorktreeContext{Present: false}
	}
	var row struct {
		ID          string `json:"id"`
		Path        string `json:"path"`
		Branch      string `json:"branch"`
		IsMain      bool   `json:"isMain"`
		IssueNumber *int   `json:"issueNumber"`
		IssueTitle  string `json:"issueTitle"`
		PRNumber    *int   `json:"prNumber"`
		PRTitle     string `json:"prTitle"`
		PRURL       string `json:"prUrl"`
		Status      string `json:"status"`
		LastCommit  string `json:"lastCommit"`
	}
	if !decodeAny(raw, &row) {
		return nil
	}
	return &prompts.WorktreeContext{
		Present:     true,
		ID:          strings.TrimSpace(row.ID),
		Path:        strings.TrimSpace(row.Path),
		Branch:      strings.TrimSpace(row.Branch),
		IsMain:      row.IsMain,
		IssueNumber: row.IssueNumber,
		IssueTitle:  strings.TrimSpace(row.IssueTitle),
		PRNumber:    row.PRNumber,
		PRTitle:     strings.TrimSpace(row.PRTitle),
		PRURL:       strings.TrimSpace(row.PRURL),
		Status:      strings.TrimSpace(row.Status),
		LastCommit:  strings.TrimSpace(row.LastCommit),
	}
}

func (a *App) callStartupRead(ctx context.Context, tool string) (mcp.CallResult, bool) {
	cctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(startupReadTimeout, cancel)
	res, err := a.MCP.CallTool(cctx, tool, map[string]any{}, mcp.CallOptions{})
	timer.Stop()
	cancel()
	return res, err == nil && !res.IsError
}

// mergedResultObject unions Daintree's JSON text body with structuredContent, with the
// structured form winning recursively. The host normally returns identical values in
// both channels, but this prevents a partial transport projection from silently dropping
// fields that were present in the other representation.
func mergedResultObject(res mcp.CallResult) map[string]any {
	out := map[string]any{}
	if strings.TrimSpace(res.Text) != "" {
		var textObj map[string]any
		if json.Unmarshal([]byte(res.Text), &textObj) == nil {
			mergeObject(out, textObj)
		}
	}
	if structured, ok := res.StructuredContent.(map[string]any); ok {
		mergeObject(out, structured)
	}
	return out
}

func mergeObject(dst, src map[string]any) {
	for key, value := range src {
		srcMap, srcOK := value.(map[string]any)
		dstMap, dstOK := dst[key].(map[string]any)
		if srcOK && dstOK {
			mergeObject(dstMap, srcMap)
			continue
		}
		dst[key] = value
	}
}

func decodeAny(value any, dst any) bool {
	raw, err := json.Marshal(value)
	return err == nil && json.Unmarshal(raw, dst) == nil
}

// ProjectName is the splash/masthead view of the same cached startup project sent to
// the backend. It is intentionally a non-blocking field read.
func (a *App) ProjectName() string {
	a.startupMu.RLock()
	defer a.startupMu.RUnlock()
	if a.cachedProject == nil {
		return ""
	}
	return a.cachedProject.Name
}

func worktreeLabel(w *prompts.WorktreeContext) string {
	if w == nil {
		return ""
	}
	if !w.Present {
		return "(none — not in a worktree)"
	}
	for _, value := range []string{w.Branch, w.ID, w.Path} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
