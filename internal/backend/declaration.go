package backend

import "strings"

// The response-declaration markers. The model is asked to open its reply with one of
// these so the backend learns, from the first bytes, whether the turn is finishing or
// about to call tools — and the BACKEND strips it before a single byte reaches this
// client. Neither marker is part of the wire contract, and in normal operation the
// filter below never fires.
//
// It exists because the failure mode is asymmetric. Server-side stripping is one
// scanner on one path; if it ever regresses — a new non-streaming route, a provider
// that reorders fragments, a refactor that forwards raw text — the visible symptom is
// "[[DAINTREE:FINAL]]" printed at the top of a human's reply, in every surface at once
// (cockpit, classic REPL, --json, the embedded host, the MCP server). Guarding at the
// stream boundary covers all of them from one place, which a render-layer guard in
// internal/ui could not: three of those five surfaces never render through it.
const (
	declarationFinalMarker = "[[DAINTREE:FINAL]]"
	declarationToolsMarker = "[[DAINTREE:TOOLS]]"
)

// declarationMaxLeadingWhitespace bounds how much whitespace may precede a marker
// before the reply is read as ordinary prose that merely happens to start with
// brackets. Mirrors MAX_LEADING_WHITESPACE in the backend's own scanner: the two
// implementations must agree about where a marker ends and the reply begins, or the
// client would eat text the server meant to keep.
const declarationMaxLeadingWhitespace = 8

// declarationFilter removes a leading declaration marker from a reply that arrives in
// fragments. Feed it every content fragment in order and forward what it returns; call
// Finish once, when the reply has ended, to release anything still held.
//
// While the opening is ambiguous it returns "" and holds — bounded by the marker's own
// length, so at most ~26 characters of a real reply are ever delayed, and only for a
// reply that actually opens with "[" or whitespace. Every other reply flushes its first
// fragment untouched, which is the entire streaming population in practice.
//
// A faithful port of the backend's DeclarationScanner, deliberately: the two are the
// same algorithm on the same input, so they cannot disagree.
//
// It also sits upstream of the retry-commit boundary, which client.go latches on the
// first OnContent call. Holding an ambiguous opening therefore holds that latch for the
// same handful of characters — and that is the correct direction: nothing has reached
// the user yet, so a failure inside the window is still safely replayable. A reply that
// is nothing BUT a marker legitimately commits no content at all.
type declarationFilter struct {
	open   bool // the opening is settled and everything now passes through
	eatSep bool // the marker is gone; the whitespace it was written on is next
	buffer string
}

// Feed returns the visible text of one fragment, with the marker (if any) removed.
func (f *declarationFilter) Feed(text string) string {
	if f.open {
		return text
	}
	if text == "" {
		return ""
	}
	f.buffer += text
	return f.drain(false)
}

// Finish releases anything still held, now that no more text is coming. A stream that
// stopped mid-marker ("[[DAINT") left real characters buffered, and they are still owed.
func (f *declarationFilter) Finish() string {
	if f.open {
		return ""
	}
	return f.drain(true)
}

func (f *declarationFilter) drain(final bool) string {
	if !f.eatSep {
		hadMarker, settled := f.resolve(final)
		if !settled {
			return ""
		}
		if !hadMarker {
			// Not a declaration, so not ours: hand back every byte received, leading
			// whitespace included.
			out := f.buffer
			f.buffer = ""
			f.open = true
			return out
		}
		f.eatSep = true
	}
	// The marker is gone; what remains is the whitespace it was written on. Trailing
	// spaces and tabs go unconditionally (they cannot be meaningful at the start of a
	// reply), then exactly ONE line terminator — so a reply that deliberately opens
	// with a blank line still does.
	rest := strings.TrimLeft(f.buffer, " \t")
	switch {
	case strings.HasPrefix(rest, "\r\n"):
		rest = rest[2:]
	case !final && (rest == "" || rest == "\r"):
		// A lone "\r" may yet be half of "\r\n", and an empty buffer says nothing at
		// all. Both resolve on the next fragment.
		f.buffer = rest
		return ""
	case strings.HasPrefix(rest, "\n"), strings.HasPrefix(rest, "\r"):
		rest = rest[1:]
	}
	f.open = true
	f.buffer = ""
	return rest
}

// resolve settles whether the reply opened with a marker. settled=false means the
// answer still depends on characters that have not arrived. When it reports a marker,
// the marker and the whitespace before it are already off the buffer.
func (f *declarationFilter) resolve(final bool) (hadMarker, settled bool) {
	buf := f.buffer
	lead := len(buf) - len(strings.TrimLeft(buf, " \t\r\n\v\f"))
	if lead > declarationMaxLeadingWhitespace {
		return false, true
	}
	body := buf[lead:]
	if body == "" {
		// Whitespace only so far. Under the bound, so a marker may still follow —
		// unless the reply has ended, in which case none did.
		return false, final
	}
	for _, marker := range [...]string{declarationFinalMarker, declarationToolsMarker} {
		if strings.HasPrefix(body, marker) {
			f.buffer = body[len(marker):]
			return true, true
		}
	}
	if !final && (strings.HasPrefix(declarationFinalMarker, body) ||
		strings.HasPrefix(declarationToolsMarker, body)) {
		// Still a plausible prefix of one of them; hold for more.
		return false, false
	}
	return false, true
}
