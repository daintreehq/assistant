package markdown

import (
	"net/url"
	"strings"
	"unicode/utf8"

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
// model's prose by sanitizeInput and, decisively, by sanitizeOutput — which drops
// any C1 that reaches the rendered string however it got there. By the time the
// output ships, the only OSC 8 left is the 7-bit form we generated.
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
// markdown needs) and C1 wholesale closes the literal form.
//
// It cannot be the whole defence, though: glamour runs html.UnescapeString over
// text nodes AFTER we hand it the source, so "&#x9d;" and "&#27;" come back as
// live controls downstream of here (and pre-decoding cannot win — the next round
// of entities just decodes one layer later). sanitizeOutput is the boundary that
// actually holds; this pass keeps the markdown the parser sees clean.
func sanitizeInput(s string) string {
	if strings.IndexByte(s, '\x1b') >= 0 {
		s = ansi.Strip(s)
	}
	if strings.IndexByte(s, '\r') >= 0 {
		// CommonMark treats a lone CR as a line ending, so it has to become one
		// rather than vanish — dropping it would weld two lines (and their block
		// markers) into one.
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
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
// The transform never rewrites a kept sequence. It only ever ADDS bytes in two
// places: a hyperlink reset in place of a rejected opener that supersedes a
// still-open allowed one (so the allowed target cannot bleed onto the rejected
// span), and a reset/reopen pair around a row break inside an allowed link (see
// reopenAcrossRows). Keeping the allowed bytes
// identical is what preserves the wrap survival issue #348 asked for: lipgloss's
// WrapWriter closes and REOPENS the span (same full URL, same id=) around every
// newline it inserts, so each physical row carries the complete URI on its own.
//
// Cell widths are unaffected: a complete OSC-8 sequence occupies no cells, so
// removing whole sequences cannot change wrapping or row boundaries.
func filterHyperlinkSchemes(s string) string { return filterOSC8(s, httpHyperlinkURI) }

// stripHyperlinks removes the OSC-8 OPENERS, for deriving the plain-text fallback.
// (A standalone closer is deliberately preserved by the shared state machine; the
// ansi.Strip that follows removes it, and it carries no target either way.)
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

	// openRaw is the exact bytes of the currently-open ALLOWED opener, kept so a
	// row break inside that link can be repaired with the identical sequence
	// rather than a re-serialized one. See reopenAcrossRows.
	openRaw := ""
	prev := 0
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], osc8Intro)
		if j < 0 {
			break
		}
		j += i
		if state == linkKept {
			reopenAcrossRows(s, prev, j, openRaw, drop)
		}
		prev = j

		n, uri, ok := scanOSC8(s[j:])
		if !ok {
			if n == 0 {
				break // unterminated tail: nothing after it to filter, leave it be
			}
			// Unusable sequence: an abort (a bare ESC, CAN or SUB — the terminal
			// resumes normal processing at that byte, so we resume there too), or a
			// terminated one with no params/URI separator, in which case n is past its
			// terminator. Either way it stays in the output untouched.
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
			openRaw = s[j:i]
			prev = i
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
		prev = i
	}
	if state == linkKept {
		reopenAcrossRows(s, prev, len(s), openRaw, drop)
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
// further to scan) or when it is unusable: aborted by a bare ESC, CAN or SUB, or
// terminated but carrying no params/URI separator. In every unusable case n > 0 —
// the offset of the aborting byte, or the length of the whole sequence — so the
// caller always makes progress and simply leaves those bytes alone.
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
	// A URI that is not valid UTF-8 did not survive the pipeline intact: x/ansi's
	// wrapper frames OSC payloads bytewise and cuts a rune whose continuation byte
	// is 0x9C ('Ü' is C3 9C), so lipgloss can hand us per-row openers carrying a
	// TRUNCATED target. Shipping one would be exactly the mis-directed link issue
	// #348 is about, so such a link simply does not become clickable.
	if !utf8.ValidString(uri) {
		return false
	}
	return u.Hostname() != ""
}

// sanitizeOutput is the trust boundary that actually holds: it keeps only the
// escapes we generate — SGR, and the OSC 8 hyperlinks filterHyperlinkSchemes has
// already validated — and drops every other control from the rendered string.
//
// WHY it must live here and not (only) at the input: glamour calls
// html.UnescapeString on text nodes, so a model writing "&#27;[2J" or "&#x9d;"
// gets a live clear-screen or an 8-bit OSC introducer back AFTER sanitizeInput
// has run. Pre-decoding cannot win that race — each extra layer of entities
// decodes one round later — but checking the bytes we are about to hand the
// terminal is decisive, whatever produced them.
//
// A dropped ESC leaves its (now inert) payload as ordinary text: "&#27;[2J"
// renders as the visible characters "[2J". That is deliberate — the attempt stays
// legible to the human instead of silently disappearing, and nothing about it can
// still drive the terminal.
func sanitizeOutput(s string) string {
	var (
		b      strings.Builder
		edited bool
		last   int
	)
	drop := func(from, to int) {
		if !edited {
			b.Grow(len(s))
			edited = true
		}
		b.WriteString(s[last:from])
		last = to
	}

	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\x1b':
			if strings.HasPrefix(s[i:], osc8Intro) {
				if n, _, ok := scanOSC8(s[i:]); ok {
					i += n // ours, already validated
					continue
				}
			}
			if n := sgrLen(s[i:]); n > 0 {
				i += n // ours: a colour/attribute change, which cannot do anything else
				continue
			}
			drop(i, i+1) // a lone ESC: strip the introducer, leave the payload visible
			i++
		case c == '\n' || c == '\t':
			i++
		case c < 0x20 || c == 0x7f:
			drop(i, i+1)
			i++
		case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
			drop(i, i+2) // a C1 control, which xterm honours as readily as the 7-bit form
			i += 2
		default:
			i++
		}
	}

	if !edited {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// sgrLen returns the byte length of the SGR sequence at the start of s, or 0 if
// s does not begin with one. SGR is CSI (ESC [) + parameter bytes + intermediate
// bytes + the final byte 'm'; any other final byte is some OTHER CSI function
// (cursor movement, erase, scroll) that our renderer never emits.
func sgrLen(s string) int {
	if !strings.HasPrefix(s, "\x1b[") {
		return 0
	}
	i := 2
	for ; i < len(s) && s[i] >= 0x30 && s[i] <= 0x3f; i++ {
	}
	for ; i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f; i++ {
	}
	if i < len(s) && s[i] == 'm' {
		return i + 1
	}
	return 0
}

// reopenAcrossRows closes and reopens an allowed hyperlink around every newline in
// s[from:to], so each physical row carries the target on its own.
//
// This is the guarantee issue #348 actually asks for, and lipgloss very nearly
// provides it: its WrapWriter resets before a newline it inserts and reopens
// after. But its own OSC parser splits a sequence on EVERY ';' and then requires
// exactly three fields, so a URI legitimately containing one — "…?a=1;b=2" — is
// never tracked, and the rows after the first carry no target at all. Rather than
// depend on that, we make the guarantee ourselves.
//
// It is self-limiting: when WrapWriter did reopen correctly, its reset arrives
// BEFORE the newline, so the link is already closed by the time we get here and
// this is never called for that row. It therefore only fires where the row would
// otherwise have been left without a target.
func reopenAcrossRows(s string, from, to int, openRaw string, drop func(from, to int, repl string)) {
	if openRaw == "" {
		return
	}
	for p := from; p < to; p++ {
		if s[p] != '\n' {
			continue
		}
		drop(p, p, ansi.ResetHyperlink()) // close before the break…
		drop(p+1, p+1, openRaw)           // …and reopen with the identical bytes after it
	}
}
