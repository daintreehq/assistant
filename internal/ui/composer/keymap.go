package composer

import "charm.land/bubbles/v2/key"

// Hint is one entry in the adaptive hint row: a key cue and its action label.
// We reuse Bubbles' key.Binding for the underlying help primitives, but the
// composer's hints are PROMOTED/REORDERED per state, so we
// carry a flat {Key, Action} the renderer can order — the binding only supplies
// the canonical help text.
type Hint struct {
	Key    string
	Action string
}

// keymap holds the canonical bindings reused for the help/hint primitives. The
// actual key DISPATCH happens in composer.go against the raw tea.KeyPressMsg
// (the contract needs modifier combinations and offset-aware behavior that a
// flat binding table can't capture); these exist so the hint row's labels are a
// single source of truth via Bubbles' key.Help.
//
// Escape is deliberately NOT in here. Its label depends on the composer's live state
// (clear draft / edit follow-up / cancel turn), so a static binding would be a second,
// permanently-stale source of truth for it; hints.go derives the label instead.
type keymap struct {
	ops     key.Binding
	palette key.Binding
	history key.Binding
}

func defaultKeymap() keymap {
	return keymap{
		ops:     key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("^O", "inspect ops")),
		palette: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "commands")),
		history: key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "history")),
	}
}

// hintState is the per-frame input to hintRow: what the composer's live state means for
// each adaptive entry. Derived once in renderHints so the row and the copy can't drift.
type hintState struct {
	// escape is the state-derived Escape action; EscapeHintHidden omits it entirely.
	escape EscapeHintMode
	// submit is the Enter verb ("send" idle, "add" mid-turn) when a draft is waiting to
	// go, or "" when there is nothing to submit or the slash palette owns Enter.
	submit string
	// newlineCue is the key cue for the terminal-independent newline, shown alongside
	// submit; "" suppresses it.
	newlineCue string
	// discovery shows "/ commands" and "↑ history". Suppressed while drafting: mid-word
	// "/" types a literal slash rather than opening the palette, and ↑ walks the draft's
	// own visual rows before it ever reaches history.
	discovery bool
	// leadWithOps = actionable attention pending AND no cancellable turn in flight.
	// Cancel takes precedence over attention.
	leadWithOps bool
}

// hintRow builds the adaptive hint row. The ORDER adapts but the SET is stable
// (promotion, not new chrome), and "^O inspect ops" is emitted EXACTLY ONCE — promoted to
// the front only when leadWithOps, else appended last.
//
// The slice is in PRIORITY order, because the renderer drops from the tail when the row
// won't fit: the state-specific Escape action and the way to send what you just typed must
// both outlive generic discovery hints.
func (k keymap) hintRow(s hintState) []Hint {
	hints := make([]Hint, 0, 6)
	if h, ok := escapeHint(s.escape); ok {
		hints = append(hints, h)
	}
	if s.submit != "" {
		hints = append(hints, Hint{Key: "Enter", Action: s.submit})
		if s.newlineCue != "" {
			hints = append(hints, Hint{Key: s.newlineCue, Action: "newline"})
		}
	}
	if s.leadWithOps {
		hints = append(hints, bindingHint(k.ops))
	}
	if s.discovery {
		hints = append(hints, bindingHint(k.palette), bindingHint(k.history))
	}
	if !s.leadWithOps {
		hints = append(hints, bindingHint(k.ops))
	}
	return hints
}

func bindingHint(b key.Binding) Hint {
	h := b.Help()
	return Hint{Key: h.Key, Action: h.Desc}
}
