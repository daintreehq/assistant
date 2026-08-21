package agent

import (
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
)

// This file owns the per-session diagnostic trace for the turn engine. The backend
// migration moved the model call behind Backend.RespondStream, which left the debug
// log blind exactly where it used to be richest: there is no more
// model.request/model.response, so a log reader could not see what the backend was
// shown or what it chose. These helpers restore that — turn.start/turn.end bracket a
// turn, and backend.respond.{request,raw_meta,skill_cue,meta,done,error} narrate each
// round — while keeping payloads BOUNDED (previews + hashes, never the full prompt
// every round, the O(turns²) trap). All debuglog usage in the agent package is
// concentrated here; the rest of the turn loop only calls these s.traceXxx methods.

// safeTrace builds and emits one trace event under a single guard. It is a no-op when
// no seam is wired (the test default, and every normal non-debug run — the app wires
// the seam ONLY when debug logging is on), so the `build` closure — which may hash the
// whole history or summarize large payloads — never runs with tracing off. It recovers
// any panic from EITHER the field builder OR the seam, honouring the invariant that
// this side channel can never break a live turn.
func (s *Session) safeTrace(event string, build func() map[string]any) {
	if s.deps.Trace == nil {
		return
	}
	defer func() { _ = recover() }()
	s.deps.Trace(event, build())
}

// turnStatus classifies a finished turn's reply into complete | cancelled | failed
// for the turn.end event. Send never throws on a model-layer failure — it returns a
// sentinel reply string — so the outcome is read back off that string. The cancel
// check comes FIRST: IsWakeFailureReply also matches the "Turn cancelled" prefix, so
// a cancelled turn would otherwise mis-classify as failed.
func turnStatus(reply string) string {
	switch {
	case reply == domain.CancelledReply:
		return "cancelled"
	case IsWakeFailureReply(reply):
		return "failed"
	default:
		return "complete"
	}
}

// previewMax budgets (in runes) for the various trace previews.
const (
	tracePreviewMax       = 2000 // produced content
	lastMessagePreviewMax = 480  // the newest request message
	shortPreviewMax       = 240  // prompts, replies, tool args
)

// traceTurnStart records the start of a turn (one entry point per turn for a log
// reader). historyLen is the visible-history size BEFORE this turn's user message is
// pushed.
func (s *Session) traceTurnStart(runID, turnID, prompt string, isWake bool, historyLen int) {
	s.safeTrace("turn.start", func() map[string]any {
		return map[string]any{
			"runId":         runID,
			"turnId":        turnID,
			"sessionId":     s.deps.SessionID,
			"isWake":        isWake,
			"promptBytes":   len(prompt),
			"promptPreview": debuglog.Preview(prompt, shortPreviewMax),
			"historyLen":    historyLen,
		}
	})
}

// traceTurnEnd records the terminal outcome of a turn (status derived from the reply,
// plus duration and the number of model rounds run).
func (s *Session) traceTurnEnd(runID, turnID, reply string, durationMs int64, rounds int) {
	s.safeTrace("turn.end", func() map[string]any {
		return map[string]any{
			"runId":        runID,
			"turnId":       turnID,
			"status":       turnStatus(reply),
			"durationMs":   durationMs,
			"rounds":       rounds,
			"replyPreview": debuglog.Preview(reply, shortPreviewMax),
		}
	})
}

// traceBackendRequest records what the backend was shown this round: the message
// count + role sequence, a hash of the whole serialized history (so an unchanged
// prefix is recognisable round-to-round without re-printing it), a preview of ONLY
// the newest message, the tool inventory (count + names + hash), and the structured
// startup/runtime/turn context — all bounded. Startup instructions are represented only
// by byte count and the aggregate hash; their contents are never written to the trace.
func (s *Session) traceBackendRequest(runID, turnID string, round int, req backend.RespondRequest, msgs []backend.Message, tools []backend.Tool) {
	s.safeTrace("backend.respond.request", func() map[string]any {
		roles := make([]string, len(msgs))
		for i, m := range msgs {
			roles[i] = m.Role
		}
		toolNames := make([]string, len(tools))
		for i, t := range tools {
			toolNames[i] = t.Function.Name
		}
		input := map[string]any{
			"messageCount": len(msgs),
			"messageRoles": roles,
			"messagesSha":  debuglog.SummarizeJSON(msgs, 0).SHA,
			"toolCount":    len(tools),
			"toolNames":    toolNames,
			"toolsetSha":   debuglog.SummarizeJSON(toolNames, 0).SHA,
			"toolChoice":   req.Input.ToolChoice,
		}
		if n := len(msgs); n > 0 {
			last := msgs[n-1]
			input["lastMessage"] = map[string]any{
				"role":    last.Role,
				"name":    last.Name,
				"preview": debuglog.Preview(string(last.Content), lastMessagePreviewMax),
			}
		}

		fields := map[string]any{
			"runId":               runID,
			"turnId":              turnID,
			"round":               round,
			"instructionRevision": req.Session.InstructionRevision,
			"statePresent":        req.State != nil,
			"input":               input,
		}
		startup := map[string]any{
			"sha":              debuglog.SummarizeJSON(req.Startup, 0).SHA,
			"projectPresent":   req.Startup.Project != nil,
			"rosterPresent":    req.Startup.AgentRoster != nil,
			"instructionBytes": len(req.Startup.ProjectInstructions),
		}
		if roster := req.Startup.AgentRoster; roster != nil {
			startup["agentCount"] = len(roster.Agents)
			startup["totalAgentCount"] = roster.TotalCount
			startup["complete"] = roster.Complete
			startup["availabilityComplete"] = roster.AvailabilityComplete
		}
		fields["startup"] = startup
		if req.State != nil {
			fields["stateBytes"] = len(*req.State)
		}
		if rc := req.Runtime; rc != nil {
			runtime := map[string]any{
				"permissionTier":  rc.PermissionTier,
				"schedulerActive": rc.SchedulerActive,
			}
			if rc.Worktree != nil {
				worktree := map[string]any{"currentPresent": rc.Worktree.Current != nil}
				if current := rc.Worktree.Current; current != nil {
					worktree["current"] = current
				}
				runtime["worktree"] = worktree
			}
			if d := rc.Display; d != nil {
				// The width the reply was shaped for. Without it, "why did it draw a table
				// that wrapped?" is unanswerable from the log — the geometry is the one
				// input to the output contract that lives entirely on the user's screen.
				runtime["display"] = map[string]any{"columns": d.Columns, "contentWidth": d.ContentWidth}
			}
			if rc.MCP != nil {
				mcp := map[string]any{"connected": rc.MCP.Connected, "transport": rc.MCP.Transport, "status": rc.MCP.Status}
				if rc.MCP.ToolCount != nil {
					mcp["toolCount"] = *rc.MCP.ToolCount
				}
				runtime["mcp"] = mcp
			}
			// The integration surface names the ENDPOINTS the model was told about this
			// round — "was it even pointed at the Daintree the user meant?" is otherwise
			// unanswerable from the log. Endpoints are already sanitized upstream
			// (mcp.SanitizeURL), so no credential can reach the log through here.
			if n := len(rc.MCPServers); n > 0 {
				servers := make([]string, 0, n)
				for _, s := range rc.MCPServers {
					servers = append(servers, s.Name+": "+s.Description)
				}
				runtime["mcpServers"] = servers
			}
			// The open-terminal roster the model reasons over this round is inert runtime
			// data, but it is EXACTLY what "the model thought terminal X was closed / still
			// asking a question" archaeology needs — and Daintree's terminal.list can return
			// an inconsistent subset across calls, so the roster the model actually SAW must
			// be recoverable straight from the log, not guessed at or re-derived after the
			// fact. Log a compact, bounded projection: count + per-terminal id, agent, state,
			// waiting reason and exit code. Metadata only — never terminal output.
			if n := len(rc.OpenTerminals); n > 0 {
				runtime["openTerminalCount"] = n
				const maxRosterEntries = 24
				shown := min(n, maxRosterEntries)
				terms := make([]map[string]any, 0, shown)
				for _, t := range rc.OpenTerminals[:shown] {
					entry := map[string]any{"id": t.ID}
					if t.AgentID != "" {
						entry["agentId"] = t.AgentID
					}
					if t.AgentState != "" {
						entry["agentState"] = t.AgentState
					}
					if t.WaitingReason != "" {
						entry["waitingReason"] = t.WaitingReason
					}
					if t.ExitCode != nil {
						entry["exitCode"] = *t.ExitCode
					}
					terms = append(terms, entry)
				}
				runtime["openTerminals"] = terms
			}
			fields["runtime"] = runtime
		}
		if tc := req.Turn; tc != nil {
			turn := map[string]any{
				"goalPreview":      debuglog.Preview(tc.Goal, shortPreviewMax),
				"isWake":           tc.IsWake,
				"workflowRunCount": len(tc.WorkflowRuns),
			}
			if tc.Memories != nil {
				turn["pinnedMemoryCount"] = len(tc.Memories.Pinned)
				turn["relevantMemoryCount"] = len(tc.Memories.Relevant)
			}
			if len(tc.ResumedWatchers) > 0 {
				turn["resumedWatcherCount"] = len(tc.ResumedWatchers)
			}
			fields["turn"] = turn
		}
		return fields
	})
}

// traceBackendRawMeta records when an HTTP attempt's SSE meta event actually arrives.
// It is deliberately separate from traceBackendMeta: the client defers committed meta
// until first content (or a successful tool-only completion), and a retry can produce
// multiple raw meta events for one logical round. Benchmark parsing uses the first raw
// event to measure backend pre-stream latency without confusing it with commit time.
func (s *Session) traceBackendRawMeta(runID, turnID string, round int, m backend.StreamMeta) {
	s.safeTrace("backend.respond.raw_meta", func() map[string]any {
		return map[string]any{
			"runId":            runID,
			"turnId":           turnID,
			"round":            round,
			"backendRequestId": m.RequestID,
			"model":            m.Model,
			"newlyLoadedCount": len(m.Skills.NewlyLoaded),
		}
	})
}

// traceBackendSkillCue records when the eager, de-duplicated skill-loaded event has
// actually been handed to the output sinks (none of which renders it live — see
// EventSink.SkillLoaded). Timestamping it here is what lets a log separate SELECTION
// latency from generation latency. Only exists for rounds with a newly-loaded skill.
func (s *Session) traceBackendSkillCue(runID, turnID string, round int, refs []backend.SkillRef) {
	s.safeTrace("backend.respond.skill_cue", func() map[string]any {
		return map[string]any{
			"runId":  runID,
			"turnId": turnID,
			"round":  round,
			"skills": skillRefLabels(refs),
		}
	})
}

// traceBackendMeta records the committed backend report: request id, chosen model,
// prompt/catalog version markers, warnings, and the skill-selection outcome (active
// vs newly-loaded runbooks + the selector verdict). This is the surface that says
// where a fix belongs — wrong/no skill loaded points at the backend selector, not a
// local tool. Its timestamp is commit time, not raw SSE arrival time.
func (s *Session) traceBackendMeta(runID, turnID string, round int, m backend.StreamMeta) {
	s.safeTrace("backend.respond.meta", func() map[string]any {
		fields := map[string]any{
			"runId":            runID,
			"turnId":           turnID,
			"round":            round,
			"backendRequestId": m.RequestID,
			"model":            m.Model,
			"promptVersion":    m.PromptVersion,
			"catalogRevision":  m.CatalogRevision,
			"stateSha":         debuglog.Summarize(m.State, 0).SHA,
		}
		if len(m.Warnings) > 0 {
			fields["warnings"] = m.Warnings
		}
		sel := m.Skills.Selector
		selector := map[string]any{"ran": sel.Ran, "degraded": sel.Degraded}
		if sel.TaskType != "" {
			selector["taskType"] = sel.TaskType
		}
		if sel.Confidence != nil {
			selector["confidence"] = *sel.Confidence
		}
		if sel.Reason != "" {
			selector["reason"] = sel.Reason
		}
		fields["skills"] = map[string]any{
			"active":      skillRefLabels(m.Skills.Active),
			"newlyLoaded": skillRefLabels(m.Skills.NewlyLoaded),
			"selector":    selector,
		}
		return fields
	})
}

// timingKeys maps the backend's server-side phase breakdown onto flat, INLINE trace
// keys. Inline rather than a nested block (the shape `usage` and `cost` use) because
// this is the one payload you read ACROSS rounds: "where did the wait go" is answered
// by grepping a whole session's done lines and sorting a column, and a ten-line
// indented JSON block per round makes that miserable. The `server` prefix is not
// decoration — it is what keeps `serverTotalMs` from being confused with the
// client-observed `durationMs` sitting next to it on the same line.
//
// A nil phase is OMITTED, never zero-filled. That is the backend's contract (it
// serializes with exclude_none) and the whole reason these are pointers: a selector
// that did not run must not read in the log like one that answered instantly.
func timingKeys(t *backend.TurnTimings) []struct {
	key string
	val *int
} {
	return []struct {
		key string
		val *int
	}{
		{"serverSelectionMs", t.SelectionMs},
		{"serverDocsMs", t.DocsMs},
		{"serverPreparationMs", t.PreparationMs},
		{"serverUpstreamOpenMs", t.UpstreamOpenMs},
		{"serverThinkingMs", t.ThinkingMs},
		{"serverFirstOutputMs", t.FirstOutputMs},
		{"serverGenerationMs", t.GenerationMs},
		{"serverTotalMs", t.TotalMs},
	}
}

// addBackendTimings writes the server-reported phase breakdown into a done event, plus
// the one figure only the CLIENT can know: how much of the round the backend does not
// account for. That derived number is gated on retries == 0 deliberately — a retried
// round's durationMs spans every attempt and its backoff sleeps, while the backend's
// total covers only the winning attempt, so subtracting them there would report the
// abandoned attempt as transport overhead. Better absent than confidently wrong.
func addBackendTimings(fields map[string]any, t *backend.TurnTimings, durationMs int64, retries int) {
	if !t.Any() {
		return
	}
	for _, p := range timingKeys(t) {
		if p.val != nil {
			fields[p.key] = *p.val
		}
	}
	if t.TotalMs != nil && retries == 0 {
		// Everything outside the server's own clock: the dial, our upload, the flight
		// home, and our decode of the stream. addTransportMarks then attributes it —
		// this is the total the client* keys break down.
		//
		// A negative result is OMITTED rather than clamped to zero, because clamping
		// would present a broken measurement as a real one. It happens for two reasons:
		// the two clocks start at different instants (ours before the request is even
		// serialized), so a fast round can land a millisecond "negative"; and durationMs
		// comes from wall-clock time, so an NTP step mid-round can move it arbitrarily.
		if overhead := durationMs - int64(*t.TotalMs); overhead >= 0 {
			fields["clientOverheadMs"] = overhead
		}
	}
}

// addTransportMarks writes the client-side latency of the attempt: how long until we
// had a connection, until our request was on the wire, and until the first byte came
// back. This is the half of a turn the backend's own timings cannot see — its clock
// starts when the request lands — and without it the difference between a
// client-measured round and the server's total is one unattributable number.
//
// EVERY mark is written only when it was actually taken. A failed attempt produces a
// partial set on purpose (a DNS failure resolves nothing and connects to nothing), so a
// zero-filled field would report a stage that never ran — and `clientFirstByteMs=0` on
// a request that never got a response would read as the fastest round ever recorded.
// Same absent-is-not-zero rule the server's phases follow.
func addTransportMarks(fields map[string]any, m *backend.TransportMarks) {
	if m == nil {
		return
	}
	if m.Reused != nil {
		fields["clientConnReused"] = *m.Reused
	}
	for key, val := range map[string]*int64{
		"clientConnectMs":     m.ConnectMs,
		"clientDnsMs":         m.DNSMs,
		"clientTlsMs":         m.TLSMs,
		"clientRequestSentMs": m.RequestSentMs,
		"clientFirstByteMs":   m.FirstByteMs,
	} {
		if val != nil {
			fields[key] = *val
		}
	}
}

// traceBackendDone records a completed respond round: timing (round + first token),
// produced content/reasoning sizes and a preview, the tool calls the model asked for
// (id + name + bounded args + args hash), the finish reason, and usage. Per-token
// deltas are deliberately NOT logged — only the aggregate.
//
// durationMs and firstTokenMs are CLIENT-observed and span every retried attempt; the
// server<Phase>Ms keys are the backend's own breakdown of the winning attempt. Keeping
// both is the point: the gap between them is the transport, and neither side can see it
// alone.
func (s *Session) traceBackendDone(runID, turnID string, round int, result backend.RespondResult, durationMs, firstTokenMs int64, retries int) {
	s.safeTrace("backend.respond.done", func() map[string]any {
		calls := make([]map[string]any, 0, len(result.Message.ToolCalls))
		for _, tc := range result.Message.ToolCalls {
			calls = append(calls, map[string]any{
				"id":          tc.ID,
				"name":        tc.Function.Name,
				"argsPreview": debuglog.Preview(tc.Function.Arguments, shortPreviewMax),
				"argsSha":     debuglog.Summarize(tc.Function.Arguments, 0).SHA,
			})
		}
		fields := map[string]any{
			"runId":          runID,
			"turnId":         turnID,
			"round":          round,
			"durationMs":     durationMs,
			"contentChars":   utf8.RuneCountInString(result.Message.Content),
			"contentPreview": debuglog.Preview(result.Message.Content, tracePreviewMax),
			"finishReason":   result.FinishReason,
			"toolCallCount":  len(calls),
			"usage": map[string]any{
				"promptTokens":     result.Usage.PromptTokens,
				"completionTokens": result.Usage.CompletionTokens,
				"totalTokens":      result.Usage.TotalTokens,
				"cachedTokens":     result.Usage.CachedTokens,
			},
		}
		if firstTokenMs > 0 {
			fields["firstTokenMs"] = firstTokenMs
		}
		// Retry count first: it is the caveat that makes durationMs readable at all. A
		// 40-second round that retried twice is a transport story, not a slow model, and
		// without this the two are indistinguishable on the done line.
		if retries > 0 {
			fields["retries"] = retries
		}
		addBackendTimings(fields, result.Timings, durationMs, retries)
		addTransportMarks(fields, result.Transport)
		// Cost, when the backend reported it. Logged per round rather than only in the
		// session total because "why was that turn expensive?" is a question you answer
		// by reading a log — and because the selector share is the one number prompt
		// work can actually move. Absent stays absent: an unreported cost must not
		// appear here as a zero any more than it may in the running total.
		if c := result.Cost; c != nil {
			cost := map[string]any{"total": c.Total, "complete": c.IsComplete()}
			if c.Main != nil {
				cost["main"] = *c.Main
			}
			if c.Selector != nil {
				cost["selector"] = *c.Selector
			}
			fields["cost"] = cost
		}
		if rc := result.Message.ReasoningContent; rc != "" {
			fields["reasoningPresent"] = true
			fields["reasoningChars"] = utf8.RuneCountInString(rc)
		}
		if len(calls) > 0 {
			fields["toolCalls"] = calls
		}
		return fields
	})
}

// traceBackendError records a terminal respond failure (a non-cancel error — cancel
// is a clean stop, logged as turn.end status=cancelled).
func (s *Session) traceBackendError(runID, turnID string, round int, durationMs int64, retries int, marks *backend.TransportMarks, err error) {
	if err == nil {
		return
	}
	s.safeTrace("backend.respond.error", func() map[string]any {
		fields := map[string]any{
			"runId":      runID,
			"turnId":     turnID,
			"round":      round,
			"durationMs": durationMs,
			"error":      err.Error(),
		}
		// The retry tally is what makes a long durationMs here legible: a round that
		// burned a minute across nine attempts and backoff sleeps is a transport story,
		// not one slow call, and the two are otherwise identical on this line.
		if retries > 0 {
			fields["retries"] = retries
		}
		// The failing attempt's client-side marks, when it got far enough to have any.
		// This is where they matter MOST: a mid-stream death 30 s in reads completely
		// differently once you can see we connected in 200 ms and had a first byte at
		// 2 s. A failure that never reached the wire carries none, which is itself the
		// answer.
		addTransportMarks(fields, marks)
		return fields
	})
}

// traceToolGap records a tool call rejected BEFORE it reached the registry's Dispatch
// (bad JSON args, or a tool excluded from this turn's explicit allowlist). Those
// rejections never produce a tool.call audit event, so without this they were the one
// class of tool failure invisible in the trace.
func (s *Session) traceToolGap(event, runID, toolCallID, name, rawArgs string) {
	s.safeTrace(event, func() map[string]any {
		fields := map[string]any{
			"runId":      runID,
			"toolCallId": toolCallID,
			"tool":       name,
		}
		if rawArgs != "" {
			fields["argsPreview"] = debuglog.Preview(rawArgs, shortPreviewMax)
		}
		return fields
	})
}

// traceCancelledStub records that count tool calls were given a synthetic CANCELLED
// result (the turn was cancelled before they ran) so the transcript stays well-formed.
func (s *Session) traceCancelledStub(runID string, count int) {
	if count <= 0 {
		return
	}
	s.safeTrace("tool.cancelled_stub", func() map[string]any {
		return map[string]any{"runId": runID, "count": count}
	})
}

// traceToolRepeat records a circuit-breaker firing (warning or abort) with the
// repeated call's signature, so a stuck loop is greppable in the trace.
func (s *Session) traceToolRepeat(event, runID, name string, count int, errCode, signature string) {
	s.safeTrace(event, func() map[string]any {
		return map[string]any{
			"runId":     runID,
			"tool":      name,
			"count":     count,
			"errorCode": errCode,
			"signature": signature,
		}
	})
}

// errCodeOf returns a tool result's error code, or "" when the result has none.
func errCodeOf(res domain.ToolResult) string {
	if res.Error != nil {
		return res.Error.Code
	}
	return ""
}

// skillRefLabels renders skill refs to compact "title" (or id fallback) labels for
// the meta event — never the full runbook body (server-owned, never in the trace).
func skillRefLabels(refs []backend.SkillRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		label := r.Title
		if label == "" {
			label = r.ID
		}
		if label == "" {
			continue
		}
		out = append(out, label)
	}
	return out
}

// traceServerCompaction records what the backend's compacted context block did to this
// session's history — the span it named, whether the splice was applied, and if not,
// which gate refused it.
//
// This is the ONLY place a compaction surfaces. There is no cockpit card, no cue, and
// no /compaction command, for the same reason backend skill loads are invisible: it is
// prompt assembly, not a step the operator takes, and a per-turn notice about a
// server-side optimisation would be noise on every turn once the feature is switched
// on. When it goes wrong, the reason belongs where archaeology already looks — beside
// backend.respond.meta, keyed by the same runId/turnId/round.
func (s *Session) traceServerCompaction(runID, turnID string, round int, c *backend.StreamCompaction, sentLen int, applied bool, reason compactionRejectReason) {
	if c == nil {
		return
	}
	s.safeTrace("backend.compaction", func() map[string]any {
		// Dereference the span edges instead of logging the *int fields directly:
		// debuglog renders only scalars inline, so a pointer falls through to the block
		// formatter and a one-line event grows two indented JSON stanzas. A missing edge
		// (the very reason a span is refused as out-of-bounds) logs as an explicit null,
		// which is a different fact from the perfectly ordinary index 0.
		var startIndex, endIndex any = debuglog.Null, debuglog.Null
		if start, end, ok := c.Replaces.Bounds(); ok {
			startIndex, endIndex = start, end
		}
		fields := map[string]any{
			"runId":        runID,
			"turnId":       turnID,
			"round":        round,
			"blockTurnId":  c.TurnID,
			"startIndex":   startIndex,
			"endIndex":     endIndex,
			"sentMessages": sentLen,
			"blockBytes":   len(c.Block.Content),
			"applied":      applied,
		}
		if reason != "" {
			fields["reason"] = string(reason)
		}
		return fields
	})
}
