package agent

import (
	"encoding/json"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// compactionMarkerPrefix and the clear marker bound the rehydration boundary. The
// compaction marker is matched by PREFIX, the clear marker by EXACT equality.
// Preserve the em-dash characters (U+2014).
const (
	compactionMarkerPrefix = "[conversation compacted"
	compactionMarker       = "[conversation compacted — earlier turns dropped from context]"
	injectNotePrefix       = "[system event]\n"
	// userInterjectPrefix frames a message the human typed WHILE a turn was already
	// running, folded into that turn at the next tool-iteration boundary (see
	// Session.InjectPrompt / foldInInjections). Unlike injectNotePrefix (a daemon
	// "[system event]"), this is genuine user input — the prefix only tells the model
	// it arrived mid-task so it can fold the new instruction into the work in flight.
	userInterjectPrefix = "[the user sent this while you were working — take it into account as you continue]\n"
)

// RehydrateResult is the reconstructed working history on resume.
type RehydrateResult struct {
	RestoredMessages []models.ChatMessage
	InitialSeq       int
	// DirtyFreshStart is set when a dup-seq tangle forced an EMPTY working history
	// resumed at maxSeq+1 (a safe fresh start that still continues numbering past
	// the dirty rows). NewSession persists a clear breadcrumb so the durable log
	// records the reset boundary at the new, collision-free seq.
	DirtyFreshStart bool
	// DroppedRows counts what rehydration silently elided to keep the resume valid:
	// rows whose tool-call JSON was malformed (the call list lost, text kept), tool
	// results orphaned from any declaring assistant message, and an incomplete
	// trailing tool exchange. Each is a correct, intentional drop, but a non-zero
	// total is observable evidence of upstream corruption — the caller surfaces it
	// (one info event on the first resumed turn + a boot debug-log line).
	DroppedRows int
}

// RehydrateSession reconstructs the working model history from persisted rows.
// Returns (result, true) to resume, or (_, false) to start
// fresh. Fresh-start fallbacks: empty rows, or a dup-seq tangle (the fingerprint
// of a historical double-write bug — replaying it is worse than losing it).
func RehydrateSession(rows []domain.ConversationMessageRecord) (RehydrateResult, bool) {
	if len(rows) == 0 {
		return RehydrateResult{}, false
	}

	// initialSeq = max(seq)+1 via an O(n) reduce (NOT a spread-max).
	// Computed first so the dup-seq safe-fresh-start below can continue numbering
	// PAST the dirty rows rather than restarting at 0.
	maxSeq := 0
	for _, r := range rows {
		if r.Seq > maxSeq {
			maxSeq = r.Seq
		}
	}
	initialSeq := maxSeq + 1

	// Dup-seq detection: a non-unique seq set is the fingerprint of a prior buggy
	// double-write. We can't trust the ordering, so we start with an
	// EMPTY working history — but we resume (not "false"/fresh) at maxSeq+1 so the
	// fresh control rows and every subsequent append are numbered ABOVE the dirty
	// rows and never collide with them again. Returning "fresh" here would restart
	// persisting at seq 0, so the next resume would keep seeing the same dup-seqs.
	// IsDirtyFreshStart marks the resume so the caller persists a clear breadcrumb.
	seen := make(map[int]struct{}, len(rows))
	for _, r := range rows {
		if _, dup := seen[r.Seq]; dup {
			return RehydrateResult{RestoredMessages: []models.ChatMessage{}, InitialSeq: initialSeq, DirtyFreshStart: true}, true
		}
		seen[r.Seq] = struct{}{}
	}

	// markerIdx = LAST row that is a compact/clear breadcrumb.
	markerIdx := -1
	for i, r := range rows {
		if r.Role == "system" && (strings.HasPrefix(r.Content, compactionMarkerPrefix) || r.Content == domain.ClearMarker) {
			markerIdx = i
		}
	}

	var working []domain.ConversationMessageRecord
	if markerIdx >= 0 {
		// Everything AFTER the marker (the marker itself is skipped — a durable-log
		// breadcrumb, not a model message). A trailing CLEAR ⇒ empty history.
		working = rows[markerIdx+1:]
	} else {
		// No marker: drop the three control rows (seq 0,1,2).
		for _, r := range rows {
			if r.Seq >= domain.ControlMessageCount {
				working = append(working, r)
			}
		}
	}

	dropped := 0
	msgs := make([]models.ChatMessage, 0, len(working))
	for _, r := range working {
		m, lostCalls := recordToChatMessage(r)
		if lostCalls {
			dropped++
		}
		msgs = append(msgs, m)
	}
	msgs, orphanDrops := dropOrphanToolResults(msgs)
	dropped += orphanDrops
	msgs, tailDrops := dropOrphanToolCallTail(msgs)
	dropped += tailDrops

	return RehydrateResult{RestoredMessages: msgs, InitialSeq: initialSeq, DroppedRows: dropped}, true
}

// ResumeCheckpoint reconciles a durable checkpoint (loaded from the store) against
// the working history RehydrateSession just rebuilt, deciding the compaction depth a
// resumed session should continue from. A persisted checkpoint records the compaction
// depth and the conversation seq at the last compaction boundary; on resume the
// session continues the chain from that depth so the depth tag on the next compaction
// note keeps climbing across restarts rather than silently resetting to 0 (the
// pre-checkpoint behaviour, noted in session.go's compactionDepth doc).
//
// It is a SEMANTIC validity gate layered on top of the storage-level parse check:
// LoadLatestCheckpoint already fell back past an unparseable payload, so here we only
// reject a checkpoint whose LastSeq sits at or beyond the resume's InitialSeq — that
// describes a conversation this DB never reached, so it is stale/unrelated and its
// depth is not trusted. Returns (depth, true) for a consistent checkpoint and
// (0, false) for a nil or inconsistent one, so the caller can both apply the depth and
// observe the rejection. nil-safe.
func ResumeCheckpoint(cp *domain.ContextCheckpointRecord, res RehydrateResult) (int, bool) {
	if cp == nil {
		return 0, false
	}
	if cp.CompactionDepth < 0 || cp.LastSeq < 0 {
		return 0, false
	}
	// The checkpoint must sit BEHIND the resume's next seq; one at/after it belongs to
	// a different (or corrupt) conversation than the delta we just replayed.
	if cp.LastSeq >= res.InitialSeq {
		return 0, false
	}
	return cp.CompactionDepth, true
}

// SelectResumeCheckpoint picks the first checkpoint (most-current first, as returned
// by storage.LoadValidCheckpoints) that passes the ResumeCheckpoint semantic-validity
// gate — so a semantically-stale 'latest' (one whose seq is ahead of the replayed
// delta) falls back to the 'prev' slot exactly as a JSON-corrupt one does, completing
// the issue's "fall back to the previous valid checkpoint" requirement across both
// failure modes. Returns the chosen record, its compaction depth, and whether any was
// accepted; (zero, 0, false) for an empty slice or when none pass.
func SelectResumeCheckpoint(cps []domain.ContextCheckpointRecord, res RehydrateResult) (domain.ContextCheckpointRecord, int, bool) {
	for _, cp := range cps {
		if depth, ok := ResumeCheckpoint(&cp, res); ok {
			return cp, depth, true
		}
	}
	return domain.ContextCheckpointRecord{}, 0, false
}

// recordToChatMessage rebuilds a ChatMessage from a persisted row. Empty
// content becomes a null content. Malformed tool-call JSON is dropped — one bad
// row never aborts a resume — and the returned bool reports that drop so the
// caller can count it (the only loss this function can incur; text is preserved).
func recordToChatMessage(r domain.ConversationMessageRecord) (models.ChatMessage, bool) {
	m := models.ChatMessage{Role: r.Role}
	if r.Content == "" {
		m.ContentNull = true
	} else {
		m.StringContent = r.Content
	}
	lostCalls := false
	if r.Role == "assistant" && r.ToolCallsJson != nil && *r.ToolCallsJson != "" {
		var calls []models.ToolCallRequest
		if err := json.Unmarshal([]byte(*r.ToolCallsJson), &calls); err == nil {
			m.ToolCalls = calls
		} else {
			// On error: drop calls, keep text — and report the loss.
			lostCalls = true
		}
	}
	// A null content is a valid wire shape ONLY alongside tool_calls. When the calls
	// were just dropped as malformed and the row carried no text, downgrade null→""
	// so the resumed row stays a valid {role:"assistant", content:""} rather than a
	// provider-rejectable {content:null} with no tool_calls.
	if lostCalls && len(m.ToolCalls) == 0 && m.ContentNull {
		m.ContentNull = false
	}
	if r.Role == "assistant" && r.ReasoningContent != nil {
		// Restore the chain-of-thought so the replayed history keeps the reasoning
		// DeepSeek requires for a tool-call turn.
		m.ReasoningContent = *r.ReasoningContent
	}
	if r.Role == "tool" && r.ToolCallID != nil {
		m.ToolCallID = *r.ToolCallID
	}
	return m, lostCalls
}

// dropOrphanToolResults filters out role=="tool" messages whose tool_call_id was
// never declared by a PRECEDING assistant tool_calls. DeepSeek rejects an
// orphan tool result (its parent's toolCallsJson was malformed/lost). The pass is
// strictly forward and declarations accumulate as we go, so a tool result is kept
// only when an EARLIER assistant message already declared its id — a result that
// precedes its (future) declaration is still an orphan in transcript order and is
// dropped. Pre-collecting ids from the whole history would wrongly keep it.
// Non-tool messages are always kept (and declare their ids before later results).
// The returned int is the number of orphan results dropped (len(in) - len(out)).
func dropOrphanToolResults(messages []models.ChatMessage) ([]models.ChatMessage, int) {
	declared := make(map[string]struct{})
	out := make([]models.ChatMessage, 0, len(messages))
	for _, m := range messages {
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				continue
			}
			if _, ok := declared[m.ToolCallID]; !ok {
				continue
			}
			out = append(out, m)
			continue
		}
		// Non-tool message: record its declared tool-call ids so subsequent tool
		// results can match, then keep it.
		for _, tc := range m.ToolCalls {
			declared[tc.ID] = struct{}{}
		}
		out = append(out, m)
	}
	return out, len(messages) - len(out)
}

// dropOrphanToolCallTail cuts an incomplete trailing tool exchange: if the
// LAST assistant message with tool_calls has any call id NOT answered by a later
// tool message, slice the history to just before that assistant message. Only the
// tail is checked — a mid-history break implies an already-unusable DB. The
// returned int is how many messages the cut removed (the assistant row plus any
// partial results after it); 0 when nothing is cut.
func dropOrphanToolCallTail(messages []models.ChatMessage) ([]models.ChatMessage, int) {
	lastCall := -1
	for i, m := range messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			lastCall = i
		}
	}
	if lastCall == -1 {
		return messages, 0
	}
	answered := make(map[string]struct{})
	for _, m := range messages[lastCall+1:] {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = struct{}{}
		}
	}
	for _, tc := range messages[lastCall].ToolCalls {
		if _, ok := answered[tc.ID]; !ok {
			// Incomplete trailing exchange — cut it.
			return messages[:lastCall], len(messages) - lastCall
		}
	}
	return messages, 0
}
