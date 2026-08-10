package ui

import (
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/ui/theme"
)

// render_signin.go draws the `/login` sheet — a card directly above the composer that
// REPLACES it while a sign-in is in progress, mirroring the approval and question
// sheets so the cockpit has exactly one input surface at any moment.
//
// The sheet is a linear wizard (endpoint → [custom URL] → key → verifying) rather than
// one form: the key prompt names the endpoint it is about to send the key to, which it
// could not do if both were entered at once.

// signInKeyPreviewCells bounds the masked key readout. A long key would otherwise push
// the fixed bottom band wider than the terminal; the count carries the real information
// ("did my paste land?") anyway.
const signInKeyPreviewCells = 32

// renderSignIn draws the sheet at content width.
func renderSignIn(th theme.Theme, s *pendingSignIn, width int) string {
	var b strings.Builder

	b.WriteString(th.Accent().Render(truncateCells(th.Glyphs.Approval+" Daintree sign-in", width)))
	b.WriteString("\n\n")

	// The sign-in in force, so the user always knows what they are replacing.
	if s.current.SignedIn {
		b.WriteString(th.Dim().Render(truncateCells(
			fmt.Sprintf("currently %s · key %s", s.current.Endpoint, s.current.KeyRedacted), width)))
	} else {
		b.WriteString(th.Dim().Render(truncateCells("not signed in", width)))
	}
	b.WriteString("\n\n")

	switch s.stage {
	case signInStageEndpoint:
		b.WriteString(renderSignInEndpoints(th, s, width))
	case signInStageCustomURL:
		b.WriteString(th.Body().Render(truncateCells("Backend URL", width)))
		b.WriteByte('\n')
		b.WriteString(renderSignInField(th, s.urlInput, false, width))
	case signInStageKey:
		b.WriteString(th.Body().Render(truncateCells("API key for "+s.baseURL, width)))
		b.WriteByte('\n')
		b.WriteString(renderSignInField(th, s.keyInput, true, width))
	case signInStageVerifying:
		b.WriteString(th.Body().Render(truncateCells("Checking "+s.baseURL+" …", width)))
	}

	if s.errMsg != "" {
		b.WriteString("\n")
		b.WriteString(th.Warning().Render(capWrap(s.errMsg, width, 3)))
	}

	b.WriteString("\n")
	b.WriteString(th.Dim().Render(truncateCells(signInHint(s), width)))
	return b.String()
}

// renderSignInEndpoints draws the choice list. Short enough (three entries) that it
// needs none of the question sheet's scroll-window machinery.
func renderSignInEndpoints(th theme.Theme, s *pendingSignIn, width int) string {
	var b strings.Builder
	b.WriteString(th.Body().Render(truncateCells("Which backend should the assistant use?", width)))
	b.WriteByte('\n')
	for i, c := range backend.EndpointChoices {
		target := c.URL
		if target == "" {
			target = c.Note
		}
		row := fmt.Sprintf(" %d) %-8s %s", i+1, c.Label, target)
		if i == s.selected {
			b.WriteString(th.Body().Reverse(true).Render(truncateCells(row, width)))
		} else {
			b.WriteString(th.Body().Render(truncateCells(row, width)))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderSignInField draws a text entry row. A masked field NEVER renders its content —
// it renders bullets plus a character count, so a paste can be confirmed as landed
// without the key appearing on screen (or, worse, in the host's scrollback).
func renderSignInField(th theme.Theme, value string, masked bool, width int) string {
	shown := value
	if masked {
		n := len([]rune(value))
		dots := n
		if dots > signInKeyPreviewCells {
			dots = signInKeyPreviewCells
		}
		shown = strings.Repeat("•", dots)
		if n > 0 {
			shown += fmt.Sprintf("  (%d)", n)
		}
	}
	return th.Body().Render(truncateCells("› "+shown, width-1)) + th.Accent().Render("▏")
}

// signInHint is the per-stage key legend.
func signInHint(s *pendingSignIn) string {
	switch s.stage {
	case signInStageEndpoint:
		return "↑/↓ or 1–3 select · Enter continue · Esc cancel"
	case signInStageCustomURL:
		return "Enter continue · Esc back"
	case signInStageKey:
		if s.currentKeySet {
			return "Enter continue (empty keeps the current key) · Esc back"
		}
		return "Enter sign in · Esc back"
	default:
		return "verifying — please wait"
	}
}
