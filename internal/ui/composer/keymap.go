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
type keymap struct {
	cancel  key.Binding
	ops     key.Binding
	palette key.Binding
	history key.Binding
}

func defaultKeymap() keymap {
	return keymap{
		cancel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("Esc", "cancel")),
		ops:     key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("^O", "inspect ops")),
		palette: key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "commands")),
		history: key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "history")),
	}
}

// hintRow builds the adaptive hint row. The ORDER adapts but the SET is stable
// (promotion, not new chrome):
//   - cancelActive = cancellable (fall back to busy when the caller doesn't
//     distinguish turn kinds).
//   - leadWithOps = actionable attention pending AND no cancellable turn in
//     flight. Cancel takes precedence over attention.
//   - "^O inspect ops" is emitted EXACTLY ONCE, promoted to the front only when
//     leadWithOps, else appended last.
func (k keymap) hintRow(cancelActive, leadWithOps bool) []Hint {
	hints := make([]Hint, 0, 5)
	if cancelActive {
		hints = append(hints, bindingHint(k.cancel))
	}
	if leadWithOps {
		hints = append(hints, bindingHint(k.ops))
	}
	hints = append(hints, bindingHint(k.palette))
	hints = append(hints, bindingHint(k.history))
	if !leadWithOps {
		hints = append(hints, bindingHint(k.ops))
	}
	return hints
}

func bindingHint(b key.Binding) Hint {
	h := b.Help()
	return Hint{Key: h.Key, Action: h.Desc}
}
