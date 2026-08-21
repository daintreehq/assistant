package markdown

import (
	"net/url"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// osc8Intro is the OSC 8 hyperlink introducer our renderer emits. A full sequence is
//
//	ESC ] 8 ; <params> ; <uri> <terminator>
//
// where <params> is a ':'-separated key=value list (glamour emits a single
// "id=<fnv32>") and <terminator> is BEL or 7-bit ST (ESC \). A sequence whose
// <uri> is empty CLOSES the current link rather than opening one.
//
// We deliberately do NOT recognise the C1 forms — 8-bit OSC (0x9D) as an
// introducer or 8-bit ST (0x9C) as a terminator. Our output is UTF-8, in which a
// lone 0x9C byte is not a control at all but the CONTINUATION byte of an ordinary
// rune ('Ü' is C3 9C), so honouring it splits a URI mid-rune and spills its tail —
// and a raw BEL — into visible text. C1 controls are instead removed from the
// model's prose at the input boundary by sanitizeInput, which is where that
// defence belongs: the only OSC 8 in the output is the 7-bit form we generated.
const osc8Intro = "\x1b]8;"

// linkState is the effective OSC-8 link at the current point in the stream.
// OSC-8 links do NOT nest — a new opener replaces the current link, and a closer
// carries no id — so this is a single state, not a stack.
type linkState int

const (
	linkNone    linkState = iota // no link open
	linkKept                     // an allowed link is open and was emitted verbatim
	linkDropped                  // a rejected link is open and was suppressed
)

// sanitizeInput removes everything in the model's own prose that could drive the
// terminal, BEFORE the markdown is parsed.
//
// ansi.Strip handles ESC-introduced sequences, but it is not enough on its own:
// it only runs when an ESC byte is present, and it passes valid UTF-8 runes
// through — including U+0080..U+009F, the C1 controls. xterm decodes UTF-8 to
// code points and honours those, so a model writing U+009D ("8-bit OSC") followed
// by "8;;file:///etc/passwd" U+009C would hand the host a clickable local-file
// target without ever emitting an ESC. Dropping C0 (bar the layout characters
// markdown needs) and C1 wholesale closes that, and keeps filterHyperlinkSchemes'
// job honest: the only OSC 8 downstream is the 7-bit form we generated ourselves.
func sanitizeInput(s string) string {
	if strings.IndexByte(s, '\x1b') >= 0 {
		s = ansi.Strip(s)
	}
	if !strings.ContainsFunc(s, isTerminalControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !isTerminalControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isTerminalControl reports whether a rune is a terminal control we refuse to
// pass through. Newline and tab are markdown's block structure, so they stay.
func isTerminalControl(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// filterHyperlinkSchemes restricts the OSC-8 hyperlinks in glamour's rendered
// output to http/https targets.
//
// WHY: glamour turns ANY destination url.Parse accepts into a real hyperlink
// (see glamour/v2 ansi/link.go makeHyperlink) — mailto:, file://, data:,
// javascript:, even a relative path — and exposes no option to gate it. Daintree
// embeds xterm.js in Electron with a linkHandler that OPENS an OSC-8 target
// externally, so a model-authored `[click](javascript:…)` would become genuinely
// actionable. Issue #348 draws the line at http/https; everything else keeps its
// visible text and loses only the (zero-width) escape.
//
// The transform is purely SUBTRACTIVE — it removes whole sequences and never
// rewrites a kept one — with exactly one exception: when a rejected opener
// supersedes a still-open allowed one, a hyperlink reset takes its place so the
// allowed target cannot bleed onto the rejected span. Keeping the allowed bytes
// identical is what preserves the wrap survival issue #348 asked for: lipgloss's
// WrapWriter closes and REOPENS the span (same full URL, same id=) around every
// newline it inserts, so each physical row carries the complete URI on its own.
//
// (That reopen is not universal — a URI containing ';' defeats the OSC parsing in
// lipgloss's own dependency, so such a link is tracked on its first row only. That
// is an upstream gap, not one this filter can close; it is unaffected either way
// by what happens here.)
//
// Cell widths are unaffected: a complete OSC-8 sequence occupies no cells, so
// removing whole sequences cannot change wrapping or row boundaries.
func filterHyperlinkSchemes(s string) string { return filterOSC8(s, httpHyperlinkURI) }

// stripHyperlinks removes EVERY OSC-8 sequence, for deriving the plain-text
// fallback.
//
// ansi.Strip cannot be trusted to do this: it scans an OSC payload byte by byte
// and so reads a lone 0x9C as 8-bit ST — which in UTF-8 is the continuation byte
// of an ordinary rune, so a perfectly valid target like "https://example.com/Ü…"
// gets cut mid-rune and the remainder of the URI, plus the real BEL, lands in the
// plain text as visible characters. Framing the sequences correctly first leaves
// ansi.Strip only the SGR it does handle properly.
func stripHyperlinks(s string) string {
	return filterOSC8(s, func(string) bool { return false })
}

// filterOSC8 is the shared walk: it removes every OSC-8 sequence whose URI `keep`
// rejects. With a keep that accepts nothing, no link is ever open, so no reset is
// ever substituted and the result is purely subtractive.
func filterOSC8(s string, keep func(uri string) bool) string {
	// Fast path: the overwhelming majority of rendered blocks carry no link at all.
	if !strings.Contains(s, osc8Intro) {
		return s
	}

	var (
		b      strings.Builder
		edited bool // b is only populated once something is actually removed
		last   int  // start of the span of s not yet copied into b
		state  = linkNone
	)
	// drop replaces s[from:to] with `repl` (usually empty), copying the untouched
	// run before it on first use so an all-allowed string never allocates.
	drop := func(from, to int, repl string) {
		if !edited {
			b.Grow(len(s))
			edited = true
		}
		b.WriteString(s[last:from])
		b.WriteString(repl)
		last = to
	}

	for i := 0; i < len(s); {
		j := strings.Index(s[i:], osc8Intro)
		if j < 0 {
			break
		}
		j += i

		n, uri, ok := scanOSC8(s[j:])
		if !ok {
			if n == 0 {
				break // unterminated tail: nothing after it to filter, leave it be
			}
			// Aborted string (a bare ESC, CAN or SUB). The terminal resumes normal
			// processing at that byte, so we resume scanning there too.
			i = j + n
			continue
		}
		i = j + n

		if uri == "" {
			// Closer. Drop it only if the link it closes was the one we suppressed;
			// a closer that follows a kept link (or no link at all) must survive.
			if state == linkDropped {
				drop(j, i, "")
			}
			state = linkNone
			continue
		}

		// Opener.
		if keep(uri) {
			state = linkKept
			continue // kept verbatim — no copy needed, it stays in the pending run
		}
		repl := ""
		if state == linkKept {
			// A rejected link superseding an allowed one: close the allowed target
			// explicitly, or its URI would paint the rejected span's cells.
			repl = ansi.ResetHyperlink()
		}
		drop(j, i, repl)
		state = linkDropped
	}

	if !edited {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// scanOSC8 parses the OSC 8 sequence at the start of s (which must begin with
// osc8Intro), returning how many bytes it spans and the URI it carries.
//
// Written by hand rather than via ansi.DecodeSequence because that decoder works
// on BYTES and so accepts a lone 0x9C as 8-bit ST — which in UTF-8 output is the
// continuation byte of a perfectly ordinary rune, and mis-framing there truncates
// the URI mid-rune and spills the remainder into visible text. This grammar is
// exactly what we generate, and nothing else needs to be understood.
//
// ok is false when the sequence never terminates (n == 0, so there is nothing
// further to scan) or when it is aborted by a bare ESC, CAN or SUB — for which n
// is the offset of the aborting byte, always > 0, so the caller makes progress.
func scanOSC8(s string) (n int, uri string, ok bool) {
	body := s[len(osc8Intro):]
	for i := 0; i < len(body); i++ {
		var end int
		switch body[i] {
		case '\x07': // BEL
			end = i + 1
		case '\x1b': // 7-bit ST (ESC \), or a bare ESC that aborts the string
			if i+1 >= len(body) || body[i+1] != '\\' {
				return len(osc8Intro) + i, "", false
			}
			end = i + 2
		case '\x18', '\x1a': // CAN, SUB
			return len(osc8Intro) + i, "", false
		default:
			continue
		}
		// Params are ':'-separated, so the FIRST ';' ends them and everything up to
		// the terminator is the URI — which matters because a URI may contain ';'.
		k := strings.IndexByte(body[:i], ';')
		if k < 0 {
			return len(osc8Intro) + end, "", false
		}
		return len(osc8Intro) + end, body[k+1 : i], true
	}
	return 0, "", false
}

// httpHyperlinkURI reports whether a URI may ship as a real clickable hyperlink.
//
// url.Parse rather than a prefix test, because a prefix test would wave through
// host-less shapes like "http:///foo" or a bare "https://" — and url.Parse also
// rejects ASCII control characters outright. It lower-cases the scheme as it
// parses, so "hTtPs://…" matches (pinned by a test). Requiring a non-empty
// Hostname (not Host, which would accept ":8080") rejects the malformed
// HTTP-shaped targets without dragging DNS or private-network policy in here.
//
// Userinfo is refused: "https://good.example@evil.example/" reads as good.example
// to a human but navigates to evil.example, and a link the MODEL authored has no
// legitimate reason to carry credentials. The visible href glamour prints beside
// the label makes that misreading easy, so the shape simply never becomes
// clickable.
//
// The parsed URL is only ever used for validation — the ORIGINAL bytes ship, so
// the sequence is never re-serialized or canonicalized.
func httpHyperlinkURI(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil {
		return false
	}
	return u.Hostname() != ""
}
