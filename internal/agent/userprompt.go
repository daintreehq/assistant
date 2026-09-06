package agent

import (
	"encoding/json"

	"github.com/daintreehq/assistant/internal/prompts"
)

// UserPrompt keeps send-time context attached while a message is queued or reclaimed.
type UserPrompt struct {
	Text string
	// HistoryText preserves per-message locations when stranded messages are coalesced.
	HistoryText string
	Worktree    *prompts.WorktreeContext
}

// ContextualText records the location alongside the message in model history.
// UI events retain Text so metadata never becomes editable composer text.
func (p UserPrompt) ContextualText() string {
	if p.HistoryText != "" {
		return p.HistoryText
	}
	if p.Worktree == nil {
		return p.Text
	}
	var location any
	if p.Worktree.Present {
		location = map[string]string{"id": p.Worktree.ID, "path": p.Worktree.Path, "branch": p.Worktree.Branch}
	}
	data, _ := json.Marshal(location)
	return p.Text + "\n\n[Worktree selected when this message was sent: " + string(data) + "]"
}

func (s *Session) currentMessageWorktree() *prompts.WorktreeContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messageWorktree
}
