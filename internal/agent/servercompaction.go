package agent

import (
	"strings"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/models"
)

// serverCompactionMarker is the durable breadcrumb written when the BACKEND's
// compacted context block replaces a prefix of this session's history.
//
// It deliberately shares compactionMarkerPrefix with the client-authored /compact
// marker, because RehydrateSession's boundary rule ("working history is everything
// after the last marker") is exactly the rule this needs, and the two mechanisms
// should not each own a private notion of where history begins. The wording differs
// only so a human reading the durable log can tell which one ran: the client's marker
// means "a small model summarised these turns", this one means "the server sent back
// reconciled state that stands in for them".
const serverCompactionMarker = "[conversation compacted — server-reconciled context block replaced earlier turns]"

// compactionRejectReason names why a block was NOT applied, for the debug trace. Every
// value here is an ordinary outcome, never an error: server-side compaction is
// best-effort by contract, so a block this client will not apply costs one turn's
// prompt savings and nothing else. The turn that carried it has already produced its
// answer.
type compactionRejectReason string

const (
	compactionRejectCapability  compactionRejectReason = "capability_closed"
	compactionRejectTurnID      compactionRejectReason = "turn_id_mismatch"
	compactionRejectSpanBounds  compactionRejectReason = "span_out_of_bounds"
	compactionRejectSpanBoundry compactionRejectReason = "span_not_on_user_boundary"
	compactionRejectSpanFrozen  compactionRejectReason = "span_crosses_frozen_history"
	compactionRejectToolSplit   compactionRejectReason = "span_splits_tool_transaction"
	compactionRejectBlockShape  compactionRejectReason = "block_shape_invalid"
	compactionRejectBlockSize   compactionRejectReason = "block_over_byte_cap"
)

// applyServerCompaction splices the backend's compacted context block into this
// session's working history, replacing the span it stands in for. Reports whether it
// was applied and, when it was not, why.
//
// sentLen is the length of the message array THIS round actually sent. It is the
// anchor for the whole validation: the block's indices address that array and no
// other, and history is append-only for the life of a round (every reslicing path —
// /clear, /compact, auto-compact, truncation — either runs at the top of the turn loop
// or refuses while a turn is in flight), so messages[0:sentLen] is still byte-for-byte
// what the server saw.
//
// Applied compaction is DESTRUCTIVE to working history, in exactly the way /compact
// already is: the replaced turns leave s.messages, and the durable rows that carried
// them stay in the table but fall behind the marker this writes. That is the point —
// the block IS the history now, and re-sending what it replaced would defeat the whole
// feature. Which is also why nothing here ever "un-applies": the capability gate
// decides whether a block is ACCEPTED, never whether an accepted one keeps being sent.
func (s *Session) applyServerCompaction(c *backend.StreamCompaction, sentLen int, turnID string) (bool, compactionRejectReason) {
	if c == nil {
		return false, ""
	}
	// The gate is consulted per round, not once per session: the answer is pinned to an
	// endpoint and the backend delegate is swappable. Fails closed when nil (the test
	// default) or when the cached descriptor came from a different deployment.
	if s.deps.BackendContextCompaction == nil {
		return false, compactionRejectCapability
	}
	caps, ok := s.deps.BackendContextCompaction()
	if !ok {
		return false, compactionRejectCapability
	}
	// A block is a statement about ONE conversation prefix at ONE moment. Applying one
	// stamped with a different turn would splice away messages nobody measured.
	if c.TurnID == "" || c.TurnID != turnID {
		return false, compactionRejectTurnID
	}
	if reason := validateCompactionBlock(c.Block, caps); reason != "" {
		return false, reason
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	start, end := c.Replaces.StartIndex, c.Replaces.EndIndex
	// end < sentLen rather than <=: the span always stops short of the newest user
	// message (the backend's selector ends there deliberately), so a span reaching the
	// end of the sent array is a contract this code does not implement.
	if start < 0 || end <= start || end >= sentLen || sentLen > len(s.messages) {
		return false, compactionRejectSpanBounds
	}
	// Both edges must sit on user-message boundaries. The server guarantees this
	// already; re-checking is what makes a splice that would begin mid-transaction
	// structurally impossible rather than merely unlikely.
	if s.messages[start].Role != "user" || s.messages[end].Role != "user" {
		return false, compactionRejectSpanBoundry
	}
	// The span must open strictly AFTER every block already in history. One that reached
	// back over a block would replace frozen state with a summary of a summary and,
	// worse, move the boundary the next request's selector reads.
	lastBlock := -1
	for i := 0; i < sentLen; i++ {
		if isCompactionBlockMessage(s.messages[i]) {
			lastBlock = i
		}
	}
	if start <= lastBlock {
		return false, compactionRejectSpanFrozen
	}
	if !compactionSpanToolClosed(s.messages[:sentLen], start, end) {
		return false, compactionRejectToolSplit
	}

	block := models.ChatMessage{
		Role:          "user",
		StringContent: c.Block.Content,
		Name:          backend.ContextCompactionBlockName,
	}
	// Copy the tail before reslicing so the re-append cannot alias the array it is
	// about to overwrite (the same hazard keepValidTail guards on the /compact path).
	tail := append([]models.ChatMessage(nil), s.messages[end:]...)
	s.messages = append(s.messages[:start:start], block)
	s.messages = append(s.messages, tail...)

	// The stashed real prompt_tokens measured the PRE-splice history. Left set, the
	// next maybeAutoCompact would see that figure against a history the block just
	// shrank and compact again immediately — a useless second compaction on top of the
	// server's. Zeroing it makes the next check fall back to the char estimate until a
	// fresh round reports real usage, which is the same thing compactLocked does and
	// for the same reason.
	s.lastPromptTokens = 0

	// Durable form: the marker moves the rehydration boundary, then the block and the
	// tail are re-persisted after it. The rows the block replaced are left in place,
	// behind the marker — an append-only log, exactly as /compact leaves them. The tail
	// is small here (the span stops at the newest user message, so what follows is this
	// turn's own messages), which is what makes re-persisting it cheap.
	s.persistMessageLocked(models.TextMessage("system", serverCompactionMarker))
	s.persistMessageLocked(block)
	for _, m := range tail {
		s.persistMessageLocked(m)
	}
	return true, ""
}

// validateCompactionBlock checks the block itself, before any index arithmetic.
func validateCompactionBlock(b backend.StreamCompactionBlock, caps backend.ContextCompactionCaps) compactionRejectReason {
	// Role and name are both load-bearing, not decoration: the backend honours the
	// reserved name ONLY on a user message, so a block that arrived as anything else
	// would be spliced in here and then ignored as a boundary on the next request —
	// and the same prefix would be compacted all over again.
	if b.Role != "user" || b.Name != backend.ContextCompactionBlockName {
		return compactionRejectBlockShape
	}
	if strings.TrimSpace(b.Content) == "" || !utf8.ValidString(b.Content) {
		return compactionRejectBlockShape
	}
	// Measured in UTF-8 bytes because that is what the capability names and what prompt
	// cost is actually paid in.
	if caps.MaxBlockContentBytes > 0 && len(b.Content) > caps.MaxBlockContentBytes {
		return compactionRejectBlockSize
	}
	return ""
}

// isCompactionBlockMessage reports whether m is a compacted context block. The name is
// honoured only on a user message, mirroring the backend's own selector: honouring it
// elsewhere would let a boundary land inside a tool transaction.
func isCompactionBlockMessage(m models.ChatMessage) bool {
	return m.Role == "user" && m.Name == backend.ContextCompactionBlockName
}

// compactionSpanToolClosed reports whether every tool call in [start,end) has its
// result there too, and vice versa.
//
// A mirror of the backend's own _is_tool_closed, and worth duplicating rather than
// trusting: the cost of being wrong is asymmetric. Ending the span on a user message
// keeps calls and results together in a well-ordered conversation, but the request
// boundary does not REQUIRE that ordering — a result may legally arrive after the next
// user message. Compacting such a span would delete an assistant's call while leaving
// its result behind, and every subsequent request this session makes would carry a tool
// result answering nothing, which the upstream rejects outright.
func compactionSpanToolClosed(messages []models.ChatMessage, start, end int) bool {
	inside := map[string]bool{}
	for i, m := range messages {
		if len(m.ToolCalls) == 0 {
			continue
		}
		callInside := i >= start && i < end
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				// An unidentifiable call cannot be paired either way, so the span
				// cannot be shown safe. Refuse rather than guess.
				return false
			}
			if callInside {
				inside[tc.ID] = true
			}
		}
	}
	for i, m := range messages {
		if m.Role != "tool" || m.ToolCallID == "" {
			continue
		}
		// A result inside the span needs its call inside; a result outside needs it
		// outside. An orphan (a result whose call appears nowhere) fails the first test
		// and is refused too, which is the conservative direction.
		if resultInside := i >= start && i < end; resultInside != inside[m.ToolCallID] {
			return false
		}
	}
	return true
}
