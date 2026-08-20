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
		b.WriteString(renderSignInField(th, s.urlInput, false, width, ""))
	case signInStageKey:
		b.WriteString(th.Body().Render(truncateCells("API key for "+s.baseURL, width)))
		b.WriteByte('\n')
		// Who bills the key, at the moment it is asked for. Shared wording with the CLI
		// flow so the two surfaces cannot drift on a claim about the user's money.
		//
		// The row budget is deliberately generous. capWrap ELLIPSIZES past its cap, and
		// at 40 columns a longer notice lost "including background supervision" — the
		// clause a reader most needs, since it is the spend that happens while nobody is
		// looking. The sentence is short enough to land whole well below that width.
		b.WriteString(th.Dim().Render(capWrap(backend.KeyPurposeNotice, width, 4)))
		b.WriteByte('\n')
		// The "keep it" affordance, stated in full and in the accent colour directly
		// above the field. It existed from the start but only as a dim key legend under
		// the sheet, which reads as chrome — so the common case (hop between the local
		// and deployed backend, same key) looked like it demanded a re-paste of a secret
		// the user cannot see, and the sheet appeared to be discarding the stored key.
		// Naming the key that would be kept is what makes the sentence trustworthy.
		if s.currentKeySet {
			b.WriteString(th.Accent().Render(capWrap(
				"Leave this blank and press Enter to keep your current key ("+s.current.KeyRedacted+").", width, 3)))
			b.WriteByte('\n')
		}
		b.WriteString(renderSignInField(th, s.keyInput, true, width, keepFieldPlaceholder(s)))
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
// placeholder, when non-empty, stands in for an empty field: it says what pressing
// Enter right now would do, rather than leaving a blank row that reads as "nothing
// entered yet, and nothing will happen".
func renderSignInField(th theme.Theme, value string, masked bool, width int, placeholder string) string {
	if value == "" && placeholder != "" {
		return th.Dim().Render(truncateCells("› "+placeholder, width-1)) + th.Accent().Render("▏")
	}
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
		// Input-dependent, because the sheet stays on screen WHILE the key is typed: a
		// standing "Enter keeps the current key" would be a live lie the moment a
		// character lands, and the key it appears to protect is one the user cannot see
		// to sanity-check. Once there is input, Enter does exactly what it now says.
		//
		// TrimSpace, not a bare emptiness test, because submitSignIn trims before it
		// decides — so a field holding only spaces DOES keep the current key, and the
		// legend has to agree with the code that acts. (The field itself still shows the
		// bullet: "a character landed" is the one thing it exists to confirm.)
		if s.currentKeySet && strings.TrimSpace(s.keyInput) == "" {
			return "Enter keeps the current key · type or paste to replace it · Esc back"
		}
		return "Enter sign in · Esc back"
	default:
		return "verifying — please wait"
	}
}

// keepFieldPlaceholder is what the empty key field says when Enter would keep the
// existing key.
//
// It names no key material at all, redacted or otherwise: the accent line directly
// above already carries the redacted key, and a second copy one row below is noise
// rather than confirmation. Kept SHORT for a second reason — the field is a single
// truncateCells row and cannot wrap, so a longer sentence loses its tail at the
// narrow widths the sheet is pinned at (measured: 40 columns dropped "key)").
func keepFieldPlaceholder(s *pendingSignIn) string {
	if !s.currentKeySet {
		return ""
	}
	return "(Enter keeps the current key)"
}
