package daemon

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// verificationScope scopes the post-completion git verification to a worktree.
type verificationScope struct {
	WorktreeID string `json:"worktreeId,omitempty"`
}

// watcherOptions is the engine-managed per-tick state persisted in optionsJson.
type watcherOptions struct {
	PerTerminal        map[string]TerminalState `json:"perTerminal,omitempty"`
	VerificationScope  *verificationScope       `json:"verificationScope,omitempty"`
	SpawnMode          string                   `json:"spawnMode,omitempty"` // "edit" | "explore" (absent ⇒ edit)
	AcceptanceCriteria string                   `json:"acceptanceCriteria,omitempty"`
}

// RunTerminalWatcherCheck runs one watcher check across ALL of its target
// terminals. Each terminal is read, classified, and published independently with
// per-terminal change detection and output-staleness tracking; the most urgent
// per-terminal outcome drives the watcher's aggregate state. Corrupt watcher state
// is disabled with a visible error event rather than throwing every tick.
func RunTerminalWatcherCheck(ctx *CheckContext, rec domain.WatcherRecord) CheckOutcome {
	now := domain.NowMS()

	// --- parse persisted state -------------------------------------------------
	var targets []string
	if err := json.Unmarshal([]byte(rec.TargetsJson), &targets); err != nil || !validTargets(targets) {
		return disableWatcher(ctx, rec, now, fmt.Sprintf("invalid targets: %v", err))
	}
	stopWhen, err1 := parseCondition(rec.StopWhenJson)
	alertWhen, err2 := parseCondition(rec.AlertWhenJson)
	if err1 != nil || err2 != nil {
		return disableWatcher(ctx, rec, now, "corrupt stopWhen/alertWhen condition")
	}
	var options watcherOptions
	if rec.OptionsJson != nil && strings.TrimSpace(*rec.OptionsJson) != "" {
		if err := json.Unmarshal([]byte(*rec.OptionsJson), &options); err != nil {
			return disableWatcher(ctx, rec, now, "corrupt watcher options")
		}
	}

	judgeQuestions := CollectModelJudges(alertWhen, stopWhen)
	needsDeepTail := HasTextCondition(alertWhen) || HasTextCondition(stopWhen)
	timedOut := rec.StopAfterMs != nil && now-rec.CreatedAt >= *rec.StopAfterMs

	// Copy perTerminal so we mutate a fresh map.
	perTerminal := map[string]TerminalState{}
	for k, v := range options.PerTerminal {
		perTerminal[k] = v
	}

	connected := ctx.MCP.Connected()

	// One batched terminal.getStatus for ALL targets (with inline output).
	statuses := StatusBatch{Ok: false, ByID: map[string]TerminalStatusEntry{}}
	if connected {
		statuses = readStatuses(ctx, targets, true)
	}

	// Cross-check the inventory ONCE when any target is absent from getStatus.
	// statuses.Ok already implies connected, so no separate gate (that would open a
	// flap window).
	var list *TerminalListResult
	if statuses.Ok && anyAbsent(targets, statuses.ByID) {
		l := readTerminalList(ctx)
		list = &l
	}

	outcomes := make([]CheckOutcome, 0, len(targets))

	for _, terminalID := range targets {
		prevState := terminalStatePtr(perTerminal, terminalID)
		signals := WatcherSignals{}
		classification := domain.ClassUnknown
		confidence := 0.4
		summary := "Watcher check."
		var evidence []string
		usedModel := false

		entry, hasEntry := statuses.ByID[terminalID]

		switch {
		case !connected:
			classification = domain.ClassNeedsLargeModel
			summary = "Daintree MCP not connected; cannot read terminal."

		case statuses.Ok && !hasEntry:
			classification, confidence, summary, evidence, signals, usedModel =
				resolveAbsent(ctx, rec, &options, terminalID, prevState, list, now, perTerminal)

		default:
			classification, confidence, summary, evidence, signals, usedModel =
				resolvePresent(ctx, rec, &options, terminalID, entry, prevState, needsDeepTail, now, perTerminal)
			if entry.Error != "" {
				evidence = append(evidence, "status error: "+entry.Error)
			}
		}

		// Judges, against THIS terminal's signals.
		judgeResults := map[string]domain.ModelJudgeAnswer{}
		if len(judgeQuestions) > 0 && connected {
			judgeResults = runModelJudges(ctx, judgeQuestions, rec, signals)
		}
		for _, q := range judgeQuestions {
			if ans, ok := judgeResults[q]; ok && ans.Matched && ans.Confidence >= judgeConfidenceFloor {
				evidence = append(evidence, fmt.Sprintf("judge[%s]: %s", q, ans.Reason))
			}
		}

		prevClass := ""
		if prevState != nil {
			prevClass = prevState.Prev
		}
		outcome := DecideOutcome(DecideArgs{
			Classification: classification,
			Confidence:     confidence,
			Summary:        summary,
			Evidence:       evidence,
			Previous:       prevClass,
			Signals:        signals,
			StopWhen:       stopWhen,
			AlertWhen:      alertWhen,
			JudgeResults:   judgeResults,
			TimedOut:       timedOut,
			UsedModel:      usedModel,
		})

		// Persist per-terminal state: latch prev + seen.
		base := perTerminal[terminalID]
		seen := false
		if prevState != nil {
			seen = prevState.Seen
		}
		seen = seen || hasEntry || (list != nil && listHas(list, terminalID))
		base.Prev = string(outcome.Classification)
		base.Seen = seen
		perTerminal[terminalID] = base

		// Publish.
		if outcome.ShouldPublish {
			severity := outcome.Severity
			// Supervisor promotion: surface a clean completion (normally "done").
			if supervisor(rec) && outcome.Stop && domain.RankOf(severity) < domain.RankOf(domain.SeverityAttention) {
				severity = domain.SeverityAttention
			}
			pubSummary := outcome.Summary
			if len(targets) > 1 {
				pubSummary = fmt.Sprintf("[%s] %s", terminalID, outcome.Summary)
			}
			_ = ctx.Queue.Publish(domain.QueuePublishArgs{
				Source:             domain.SourceTerminalWatcher,
				Severity:           severity,
				Title:              fmt.Sprintf("%s: %s", rec.Title, humanize(outcome.Classification)),
				Summary:            pubSummary,
				Target:             &domain.EventTarget{TerminalID: terminalID},
				Evidence:           outcome.Evidence,
				EpistemicKind:      outcome.EpistemicKind,
				DedupeKey:          fmt.Sprintf("watcher:%s:%s", rec.ID, terminalID),
				RecommendedActions: recommendedActionsFor(outcome.Classification, terminalID),
			})
		}

		outcomes = append(outcomes, outcome)
	}

	// Headline = most-urgent per-terminal outcome.
	headline := CheckOutcome{
		Classification: domain.ClassNoChange,
		Confidence:     0.4,
		Summary:        "No terminals to check.",
		Evidence:       []string{},
		EpistemicKind:  domain.EpistemicUnverified,
		Severity:       domain.SeverityDebug,
	}
	if len(outcomes) > 0 {
		headline = outcomes[0]
		for _, o := range outcomes {
			if moreUrgent(o, headline) {
				headline = o
			}
		}
	}

	// Stop semantics across ALL terminals (not the headline).
	var stopReason StopReason
	switch {
	case anyStopReason(outcomes, StopTimeout):
		stopReason = StopTimeout
	case anyStopReason(outcomes, StopConditionMet):
		stopReason = StopConditionMet
	case len(outcomes) > 0 && allStopped(outcomes):
		stopReason = StopTerminal
	}
	stop := stopReason != StopNone

	// Rate-limit backoff.
	rateLimited := false
	for _, o := range outcomes {
		if o.Classification == domain.ClassRateLimited {
			rateLimited = true
			break
		}
	}
	cadence := int64(rec.CadenceMs)
	if rateLimited && rateLimitCooldownMS > cadence {
		cadence = rateLimitCooldownMS
	}
	nextCheckAt := now + cadence

	status := "active"
	if stop {
		if stopReason == StopTimeout {
			status = "timeout"
		} else {
			status = "condition_met"
		}
	}

	options.PerTerminal = perTerminal
	optsJSON, _ := json.Marshal(options)
	// CLAIM the finalize: apply it ONLY while the watcher is still 'active'. If the main turn
	// cancelled this watcher during the check, the claim fails and we do NOT write it back —
	// re-arming (status back to 'active' with a new nextCheckAt) would resurrect a cancelled
	// watcher and keep it supervising forever. A lost claim also means we leave the cancel's
	// own grant revocation in place rather than racing it here.
	claimed, _ := ctx.Store.ClaimDueWatcher(rec.ID, map[string]any{
		"lastClassification": string(headline.Classification),
		"lastEpistemicKind":  string(headline.EpistemicKind),
		"lastCheckedAt":      now,
		"nextCheckAt":        nextCheckAt,
		"optionsJson":        string(optsJSON),
		"status":             status,
	})
	if claimed && stop {
		_, _ = ctx.Store.RevokeGrantsByActor(rec.ID, now)
	}

	headline.Stop = stop
	headline.StopReason = stopReason
	return headline
}

// resolveAbsent classifies a terminal absent from terminal.getStatus, cross-checked
// against the terminal.list inventory.
func resolveAbsent(ctx *CheckContext, rec domain.WatcherRecord, options *watcherOptions, terminalID string,
	prevState *TerminalState, list *TerminalListResult, now int64, perTerminal map[string]TerminalState,
) (domain.WatcherClassification, float64, string, []string, WatcherSignals, bool) {

	var listed *ListedTerminal
	if list != nil {
		if l, ok := list.ByID[terminalID]; ok {
			listed = &l
		}
	}

	if listed != nil {
		agentState := listed.AgentState
		signals := WatcherSignals{
			AgentState:    agentState,
			RuntimeStatus: runtimeFromAgentState(agentState),
			WaitingReason: listed.WaitingReason,
			ExitCode:      listed.ExitCode,
			Tail:          "",
		}
		switch {
		case agentState == "exited":
			evidence := []string{"agentState=exited (terminal.list)"}
			if listed.ExitCode != nil && *listed.ExitCode != 0 {
				evidence = append(evidence, fmt.Sprintf("exitCode=%d (nonzero)", *listed.ExitCode))
			}
			return domain.ClassTerminalExited, 0.95, "Terminal exited.", evidence, signals, false

		case agentState == "waiting":
			if options.SpawnMode == "explore" && listed.WaitingReason != "question" {
				// Explore-idle = finished its turn. Explore is READ-ONLY by intent, so its
				// completion is terminal regardless of git state and is NOT routed through the
				// git-clean gate (the summary claims only "finished its turn", never a clean/
				// verified tree, and the daemon attaches no irreversible action). Enforcing that
				// an explore agent truly cannot edit belongs at the Daintree spawn layer.
				ev := []string{fmt.Sprintf("agentState=waiting%s (explore-idle, terminal.list)", parens(listed.WaitingReason))}
				return domain.ClassCompletedSuccess, 0.85, "Explore agent finished its turn (idle at prompt).", ev, signals, false
			}
			sum := "Agent is waiting for input."
			if listed.WaitingReason == "question" {
				sum = "Agent is asking a question."
			}
			ev := []string{fmt.Sprintf("agentState=waiting%s (terminal.list)", parens(listed.WaitingReason))}
			return domain.ClassWaitingForInput, 0.9, sum, ev, signals, false

		case agentState == "completed":
			g := gateCompletion(ctx, options.VerificationScope, []string{"agentState=completed (terminal.list)"},
				&gateInput{rec: rec, signals: signals, acceptanceCriteria: options.AcceptanceCriteria})
			return g.classification, g.confidence, g.summary, g.evidence, signals, false

		default:
			// Working per list but getStatus dropped it → deep read.
			return classifyFromContent(ctx, rec, options, terminalID, agentState, listed.ExitCode, prevState, signals, now, perTerminal, true)
		}
	}

	if list == nil || !list.Ok {
		// CANNOT prove exit — stay alive (original false-exit bug source).
		return domain.ClassNoChange, 0.4,
			"Terminal absent from terminal.getStatus and terminal.list could not be read; will re-check.",
			nil, WatcherSignals{Tail: ""}, false
	}
	if (prevState == nil || !prevState.Seen) && now-rec.CreatedAt < WatcherSpawnGraceMS {
		return domain.ClassNoChange, 0.5,
			"Terminal not yet registered by Daintree (just spawned); will re-check.",
			nil, WatcherSignals{Tail: ""}, false
	}
	// Absent from BOTH + past grace → real exit.
	return domain.ClassTerminalExited, 0.9,
		"Terminal is no longer reported by Daintree (closed or removed).",
		[]string{"absent from terminal.getStatus and terminal.list"},
		WatcherSignals{AgentState: "exited", RuntimeStatus: "exited", Tail: ""}, false
}

// resolvePresent classifies a terminal present in terminal.getStatus.
func resolvePresent(ctx *CheckContext, rec domain.WatcherRecord, options *watcherOptions, terminalID string,
	entry TerminalStatusEntry, prevState *TerminalState, needsDeepTail bool, now int64, perTerminal map[string]TerminalState,
) (domain.WatcherClassification, float64, string, []string, WatcherSignals, bool) {

	agentState := entry.AgentState
	waitingReason := entry.WaitingReason

	// Tail: prefer inline recentOutput unless a deep tail is needed; "" is valid.
	readFailed := false
	var tail string
	if !needsDeepTail && entry.RecentOutput != nil {
		tail = *entry.RecentOutput
	} else {
		read := readOutput(ctx, terminalID)
		readFailed = !read.Ok
		tail = read.Value
	}

	var outHash string
	var msSinceOutput *int64
	if readFailed {
		base := stateOrEmpty(prevState)
		base.ReadFailures++
		perTerminal[terminalID] = base
		if prevState != nil {
			outHash = prevState.OutHash
		}
	} else {
		st, ms := NextOutputState(prevState, tail, now)
		base := stateOrEmpty(prevState)
		base.OutHash = st.OutHash
		base.OutAt = st.OutAt
		base.Prev = st.Prev
		base.ReadFailures = 0
		perTerminal[terminalID] = base
		outHash = st.OutHash
		m := ms
		msSinceOutput = &m
	}

	signals := WatcherSignals{
		AgentState:    agentState,
		RuntimeStatus: runtimeFromAgentState(agentState),
		WaitingReason: waitingReason,
		ExitCode:      entry.ExitCode,
		Tail:          tail,
		MsSinceOutput: msSinceOutput,
	}

	switch {
	case agentState == "exited":
		evidence := []string{"agentState=exited"}
		if signals.ExitCode != nil && *signals.ExitCode != 0 {
			evidence = append(evidence, fmt.Sprintf("exitCode=%d (nonzero)", *signals.ExitCode))
		}
		return domain.ClassTerminalExited, 0.95, "Terminal exited.", evidence, signals, false

	case agentState == "waiting":
		if options.SpawnMode == "explore" && waitingReason != "question" {
			// Explore-idle = finished its turn. Explore is READ-ONLY by intent, so its completion
			// is terminal regardless of git state and is intentionally NOT git-gated (the summary
			// claims only "finished its turn", never a clean/verified tree; no irreversible action
			// is attached). Enforcing that explore truly cannot edit is a Daintree-spawn concern.
			ev := []string{fmt.Sprintf("agentState=waiting%s (explore-idle)", parens(waitingReason))}
			return domain.ClassCompletedSuccess, 0.85, "Explore agent finished its turn (idle at prompt).", ev, signals, false
		}
		sum := "Agent is waiting for input."
		ev := []string{fmt.Sprintf("agentState=waiting%s", parens(waitingReason))}
		if waitingReason == "question" {
			sum = "Agent is asking a question."
			// Fold the actual question text (terminal tail) into the summary and
			// evidence so the operator sees WHAT is being asked, not just that
			// something is. Deterministic — no model call; tail is already read.
			if snip := tailSnippet(signals.Tail, 2, 200); snip != "" {
				sum = fmt.Sprintf("Agent is asking a question: %q", snip)
				ev = append(ev, fmt.Sprintf("question: %q", snip))
			}
		}
		return domain.ClassWaitingForInput, 0.9, sum, ev, signals, false

	case agentState == "completed":
		g := gateCompletion(ctx, options.VerificationScope, []string{"agentState=completed"},
			&gateInput{rec: rec, signals: signals, acceptanceCriteria: options.AcceptanceCriteria})
		return g.classification, g.confidence, g.summary, g.evidence, signals, false

	case readFailed:
		rf := perTerminal[terminalID].ReadFailures
		return domain.ClassNoChange, 0.4, readFailureSummary(rf), nil, signals, false

	case strings.TrimSpace(tail) != "":
		return classifyTail(ctx, rec, options, terminalID, agentState, entry.ExitCode, outHash, prevState, signals, perTerminal)

	default:
		return domain.ClassNoChange, 0.4, "No new output.", nil, signals, false
	}
}

// classifyFromContent handles the list-fallback "working" branch: getStatus
// dropped the terminal but terminal.list reports it alive, so read scrollback
// directly. signals carries the listed agentState; tail is filled from the read.
func classifyFromContent(ctx *CheckContext, rec domain.WatcherRecord, options *watcherOptions, terminalID, agentState string,
	exitCode *int, prevState *TerminalState, signals WatcherSignals, now int64, perTerminal map[string]TerminalState, _ bool,
) (domain.WatcherClassification, float64, string, []string, WatcherSignals, bool) {

	read := readOutput(ctx, terminalID)
	if !read.Ok {
		base := stateOrEmpty(prevState)
		base.ReadFailures++
		perTerminal[terminalID] = base
		return domain.ClassNoChange, 0.4, readFailureSummary(base.ReadFailures), nil, signals, false
	}
	tail := read.Value
	st, ms := NextOutputState(prevState, tail, now)
	base := stateOrEmpty(prevState)
	base.OutHash = st.OutHash
	base.OutAt = st.OutAt
	base.Prev = st.Prev
	base.ReadFailures = 0
	perTerminal[terminalID] = base
	signals.Tail = tail
	m := ms
	signals.MsSinceOutput = &m

	if strings.TrimSpace(tail) == "" {
		sum := fmt.Sprintf("Agent %s (per terminal.list; getStatus omitted it).", orActive(agentState))
		return domain.ClassNoChange, 0.5, sum, nil, signals, false
	}
	return classifyTail(ctx, rec, options, terminalID, agentState, exitCode, st.OutHash, prevState, signals, perTerminal)
}

// classifyTail is the shared content-classify path: rate-limit signature →
// no-change dedupe (lastClassifyKey) → small model → completion gate routing.
func classifyTail(ctx *CheckContext, rec domain.WatcherRecord, options *watcherOptions, terminalID, agentState string,
	exitCode *int, outHash string, prevState *TerminalState, signals WatcherSignals, perTerminal map[string]TerminalState,
) (domain.WatcherClassification, float64, string, []string, WatcherSignals, bool) {

	// classifyKey EXCLUDES msSinceOutput (noOutputForMs is deterministic).
	classifyKey := fmt.Sprintf("%s|%s|%s", agentState, intOrEmpty(exitCode), outHash)

	if detectRateLimitSignature(tailBytes(signals.Tail, rateLimitTailWindow)) {
		return domain.ClassRateLimited, 0.9,
			"Agent appears rate-limited by its model provider (API limit in recent output).",
			[]string{"rate-limit signature in recent output"}, signals, false
	}
	if prevState != nil && prevState.LastClassifyKey == classifyKey {
		return domain.ClassNoChange, 0.5, "No change in terminal signals since last classification.", nil, signals, false
	}

	prev := ""
	if prevState != nil {
		prev = prevState.Prev
	}
	verdict := classifyWithModel(ctx, rec, signals, prev)
	classification := verdict.Classification
	confidence := verdict.Confidence
	summary := verdict.Summary
	evidence := verdict.Evidence

	// Latch lastClassifyKey ONLY when the key fully determines the verdict.
	if classification != domain.ClassUnknown &&
		classification != domain.ClassCompletedSuccess &&
		classification != domain.ClassRateLimited {
		base := perTerminal[terminalID]
		base.LastClassifyKey = classifyKey
		perTerminal[terminalID] = base
	}

	// A model-claimed completion must still pass the read-only git gate.
	if classification == domain.ClassCompletedSuccess {
		g := gateCompletion(ctx, options.VerificationScope, evidence,
			&gateInput{rec: rec, signals: signals, acceptanceCriteria: options.AcceptanceCriteria})
		return g.classification, g.confidence, g.summary, g.evidence, signals, true
	}
	return classification, confidence, summary, evidence, signals, true
}

// disableWatcher disables a corrupt-state watcher with a visible error event and
// returns a stop:true outcome.
func disableWatcher(ctx *CheckContext, rec domain.WatcherRecord, now int64, reason string) CheckOutcome {
	_ = ctx.Store.UpdateWatcher(rec.ID, map[string]any{"status": "error", "lastCheckedAt": now})
	_, _ = ctx.Store.RevokeGrantsByActor(rec.ID, now)
	_ = ctx.Queue.Publish(domain.QueuePublishArgs{
		Source:        domain.SourceTerminalWatcher,
		Severity:      domain.SeverityError,
		Title:         rec.Title + ": watcher disabled",
		Summary:       fmt.Sprintf("Corrupt watcher state for %s: %s", rec.ID, reason),
		EpistemicKind: domain.EpistemicUnverified,
	})
	return CheckOutcome{
		Classification: domain.ClassUnknown,
		Confidence:     0,
		Summary:        "Watcher disabled due to corrupt state.",
		Evidence:       []string{},
		EpistemicKind:  domain.EpistemicUnverified,
		Severity:       domain.SeverityError,
		ShouldPublish:  false,
		Stop:           true,
		StopReason:     StopTerminal,
	}
}

// --- small helpers -----------------------------------------------------------

func humanize(c domain.WatcherClassification) string {
	return strings.ReplaceAll(string(c), "_", " ")
}

func supervisor(rec domain.WatcherRecord) bool {
	return rec.IsSupervisor != nil && *rec.IsSupervisor
}

func validTargets(targets []string) bool {
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if t == "" {
			return false
		}
	}
	return true
}

func parseCondition(raw *string) (*domain.WatchCondition, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, nil
	}
	var c domain.WatchCondition
	if err := json.Unmarshal([]byte(*raw), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func anyAbsent(targets []string, byID map[string]TerminalStatusEntry) bool {
	for _, t := range targets {
		if _, ok := byID[t]; !ok {
			return true
		}
	}
	return false
}

func listHas(list *TerminalListResult, id string) bool {
	if list == nil {
		return false
	}
	_, ok := list.ByID[id]
	return ok
}

func terminalStatePtr(m map[string]TerminalState, id string) *TerminalState {
	if v, ok := m[id]; ok {
		return &v
	}
	return nil
}

func stateOrEmpty(prev *TerminalState) TerminalState {
	if prev != nil {
		return *prev
	}
	return TerminalState{}
}

func anyStopReason(outcomes []CheckOutcome, r StopReason) bool {
	for _, o := range outcomes {
		if o.StopReason == r {
			return true
		}
	}
	return false
}

func allStopped(outcomes []CheckOutcome) bool {
	for _, o := range outcomes {
		if !o.Stop {
			return false
		}
	}
	return true
}

func parens(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func intOrEmpty(p *int) string {
	if p == nil {
		return ""
	}
	return itoa(int64(*p))
}

func orActive(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

func readFailureSummary(n int) string {
	word := "failures"
	if n == 1 {
		word = "failure"
	}
	return fmt.Sprintf("Could not read terminal output (%d consecutive %s); will re-check.", n, word)
}
