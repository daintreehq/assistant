package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/credentials"
)

// signin.go drives the `/login` sheet: opening it, its key handling, and the async
// verify that swaps the live backend client (app.SignIn). The sheet's drawing lives in
// render_signin.go; the sign-in mechanics themselves live in internal/app.

// SignInResultMsg reports the outcome of a `/login` attempt back to the loop. Err is
// nil on success. It is exported because the pump/controller path constructs it.
type SignInResultMsg struct {
	Endpoint string
	Err      error
}

// openSignIn parks the sign-in sheet, pre-selecting the endpoint currently in use so
// the common case (re-enter a key for the same backend) is Enter, Enter, paste.
func (m Model) openSignIn() (tea.Model, tea.Cmd) {
	// Refuse while a turn is running. A turn is MULTI-ROUND: agent.Session calls
	// RespondStream again after every tool round, and each call re-reads the swappable
	// delegate. Swapping between rounds would send round N+1 to a different endpoint
	// carrying the opaque backend `state` token that the PREVIOUS endpoint signed —
	// which the new one rejects, failing the turn. It would also silently move that
	// turn's remaining cost onto the newly entered key. Waiting is the honest answer.
	if m.inFlight {
		return m.onCommandComplete(CommandCompleteMsg{
			Title: "Login",
			Text:  "Can't change the sign-in while a turn is running — the turn would finish against a different backend. Cancel it (Esc) or wait for it to complete, then run /login again.",
		})
	}
	st := m.controller.app.SignInStatus()
	s := &pendingSignIn{current: st, currentKeySet: st.SignedIn}
	for i, c := range backend.EndpointChoices {
		if c.URL != "" && c.URL == st.Endpoint {
			s.selected = i
		}
	}
	// Be precise about WHEN an env override bites. A sign-in here takes effect immediately
	// (SignIn swaps the live client), but config is re-resolved from the environment on
	// the next launch — so the env value silently reasserts then. The earlier wording
	// claimed the sign-in would not take effect at all, which contradicted the behaviour.
	if st.EnvOverride != "" {
		s.errMsg = st.EnvOverride + " is set in the environment. This sign-in applies to the running session, but that value takes over again on the next launch — unset it to make this stick."
	}
	m.pendingSignIn = s
	return m.afterStateChange(nil)
}

// onSignInKey routes a keypress to the current wizard stage.
func (m Model) onSignInKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	s := m.pendingSignIn
	if s == nil {
		return m, nil
	}
	switch s.stage {
	case signInStageEndpoint:
		return m.onSignInEndpointKey(k, s)
	case signInStageCustomURL:
		return m.onSignInTextKey(k, s, false)
	case signInStageKey:
		return m.onSignInTextKey(k, s, true)
	default:
		// Verifying: the request is in flight and cannot be steered. Swallow input with
		// a bell rather than queueing keystrokes that would land on whatever comes next.
		return m, bellCmd()
	}
}

func (m Model) onSignInEndpointKey(k tea.KeyPressMsg, s *pendingSignIn) (tea.Model, tea.Cmd) {
	n := len(backend.EndpointChoices)
	switch {
	case k.Code == tea.KeyEscape || k.Code == tea.KeyEsc:
		return m.closeSignIn("Sign-in cancelled.")
	case k.Code == tea.KeyUp:
		s.selected = clampChoice(s.selected-1, n)
		return m.afterStateChange(nil)
	case k.Code == tea.KeyDown:
		s.selected = clampChoice(s.selected+1, n)
		return m.afterStateChange(nil)
	case k.Code == tea.KeyEnter || k.Code == tea.KeyKpEnter:
		return m.advanceFromEndpoint(s)
	}
	// A bare digit picks that entry directly.
	if len(k.Text) == 1 && k.Text[0] >= '1' && k.Text[0] <= '9' {
		if idx := int(k.Text[0] - '1'); idx < n {
			s.selected = idx
			return m.advanceFromEndpoint(s)
		}
	}
	return m, bellCmd()
}

// advanceFromEndpoint moves past the choice list: "Custom" opens the URL field,
// anything else fixes the endpoint and goes straight to the key.
func (m Model) advanceFromEndpoint(s *pendingSignIn) (tea.Model, tea.Cmd) {
	s.errMsg = ""
	chosen := backend.EndpointChoices[s.selected]
	if chosen.URL == "" {
		s.stage = signInStageCustomURL
		// Seed a custom URL already in use, so re-entering the sheet does not make the
		// user retype it.
		if s.urlInput == "" && !knownEndpoint(s.current.Endpoint) {
			s.urlInput = s.current.Endpoint
		}
		return m.afterStateChange(nil)
	}
	s.baseURL = chosen.URL
	s.stage = signInStageKey
	return m.afterStateChange(nil)
}

// onSignInTextKey drives both text stages. masked selects the key field, which differs
// only in which buffer it edits and what Enter does with it.
func (m Model) onSignInTextKey(k tea.KeyPressMsg, s *pendingSignIn, masked bool) (tea.Model, tea.Cmd) {
	buf := &s.urlInput
	if masked {
		buf = &s.keyInput
	}
	switch {
	case k.Code == tea.KeyEscape || k.Code == tea.KeyEsc:
		// Esc steps BACK a stage rather than abandoning the sheet — a mistyped URL
		// should not cost the endpoint choice too. From the first stage it cancels.
		s.errMsg = ""
		s.stage = signInStageEndpoint
		return m.afterStateChange(nil)
	case k.Code == tea.KeyBackspace:
		if r := []rune(*buf); len(r) > 0 {
			*buf = string(r[:len(r)-1])
		}
		return m.afterStateChange(nil)
	case k.Code == tea.KeyEnter || k.Code == tea.KeyKpEnter:
		if masked {
			return m.submitSignIn(s)
		}
		normalized, err := credentials.NormalizeBaseURL(s.urlInput)
		if err != nil {
			s.errMsg = err.Error()
			return m.afterStateChange(nil)
		}
		s.errMsg, s.baseURL, s.stage = "", normalized, signInStageKey
		return m.afterStateChange(nil)
	}
	if k.Text != "" {
		*buf += k.Text
		s.errMsg = ""
		return m.afterStateChange(nil)
	}
	return m, bellCmd()
}

// onSignInPaste appends a bracketed paste to the active field.
//
// Both fields are single-line values, so ALL whitespace is stripped rather than just
// trimmed: keys and URLs copied out of a browser, a chat client, or a wrapped terminal
// routinely arrive with a trailing newline or an embedded line break, and either would
// otherwise fail the shape check with a message that sounds like the key itself is bad.
func (m Model) onSignInPaste(text string) (tea.Model, tea.Cmd) {
	s := m.pendingSignIn
	cleaned := strings.Join(strings.Fields(text), "")
	if cleaned == "" {
		return m, bellCmd()
	}
	switch s.stage {
	case signInStageCustomURL:
		s.urlInput += cleaned
	case signInStageKey:
		s.keyInput += cleaned
	default:
		// Nothing to paste into on the menu or while verifying.
		return m, bellCmd()
	}
	s.errMsg = ""
	return m.afterStateChange(nil)
}

// submitSignIn validates locally, then hands off to the async verify+swap.
func (m Model) submitSignIn(s *pendingSignIn) (tea.Model, tea.Cmd) {
	key := strings.TrimSpace(s.keyInput)
	if key == "" {
		// Empty means "keep the current key" — the reason changing only the endpoint
		// does not require retyping a secret the user cannot see.
		if !s.currentKeySet {
			s.errMsg = "a key is required"
			return m.afterStateChange(nil)
		}
		var err error
		if key, err = m.controller.currentAPIKey(); err != nil {
			s.errMsg = err.Error()
			return m.afterStateChange(nil)
		}
	}
	if err := credentials.ValidateKeyShape(key); err != nil {
		s.errMsg = err.Error()
		return m.afterStateChange(nil)
	}
	s.errMsg = ""
	s.stage = signInStageVerifying
	return m.afterStateChange(m.controller.signIn(m.ctx, credentials.Credentials{BaseURL: s.baseURL, APIKey: key}))
}

// onSignInResult lands the async outcome. A failure returns to the key stage with the
// reason inline so the user can correct it without losing the sheet; a success pops the
// sheet and reports through the ordinary command-result card.
func (m Model) onSignInResult(msg SignInResultMsg) (tea.Model, tea.Cmd) {
	s := m.pendingSignIn
	if s == nil {
		return m, nil
	}
	if msg.Err != nil {
		s.stage = signInStageKey
		s.errMsg = msg.Err.Error()
		return m.afterStateChange(nil)
	}
	text := "Signed in to " + msg.Endpoint + ". This applies from your next message."
	// A partial verification must never read as a full one — say which check was skipped.
	if w := m.controller.lastSignInWarning(); w != "" {
		text += "\n\nNote: " + w + "."
	}
	return m.closeSignIn(text)
}

// closeSignIn pops the sheet and emits a result card.
func (m Model) closeSignIn(text string) (tea.Model, tea.Cmd) {
	m.pendingSignIn = nil
	return m.onCommandComplete(CommandCompleteMsg{Title: "Login", Text: text})
}

// knownEndpoint reports whether a URL is one of the menu's fixed choices (so a value
// that is NOT is treated as a custom endpoint worth preserving).
func knownEndpoint(u string) bool {
	for _, c := range backend.EndpointChoices {
		if c.URL != "" && c.URL == u {
			return true
		}
	}
	return false
}
