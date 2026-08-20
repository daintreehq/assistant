package markdown

import (
	"net/url"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// osc8Intro is the OSC 8 hyperlink introducer. A full sequence is
//
//	ESC ] 8 ; <params> ; <uri> <terminator>
//
// where <params> is a ':'-separated key=value list (glamour emits a single
// "id=<fnv32>") and <terminator> is BEL, 7-bit ST (ESC \) or 8-bit ST. A
// sequence whose <uri> is empty CLOSES the current link rather than opening one.
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
// The transform is purely SUBTRACTIVE — it removes bytes and never rewrites a
// kept sequence — with exactly one exception: when a rejected opener supersedes a
// still-open allowed one, a hyperlink reset takes its place so the allowed target
// cannot bleed onto the rejected span. That keeps the http/https bytes
// byte-for-byte identical to what glamour and lipgloss produced, which is what
// makes the wrap survive: lipgloss's WrapWriter closes and REOPENS the span (same
// full URL, same id=) around every newline it inserts, so each physical row
// carries the complete URI on its own.
//
// Cell widths are unaffected: OSC-8 sequences are zero-width, so removing them
// cannot change wrapping or row boundaries.
func filterHyperlinkSchemes(s string) string {
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

		// DecodeSequence with a nil parser gives us the exact raw sequence and its
		// length without a pooled data buffer (so an over-long URI is fine), and it
		// already knows every terminator and cancel rule. A non-Normal end state
		// means the sequence never terminated.
		seq, _, n, end := ansi.DecodeSequence(s[j:], ansi.NormalState, nil)
		if n == 0 || end != ansi.NormalState {
			break // malformed / unterminated tail: leave it exactly as it is
		}
		i = j + n

		uri, ok := osc8URI(seq)
		if !ok {
			continue // not a well-formed OSC 8 after all; pass it through untouched
		}

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
		if httpHyperlinkURI(uri) {
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

// osc8URI splits a raw OSC 8 sequence into its URI part, reporting false if the
// sequence is not a well-formed OSC 8. Params are ':'-separated, so the FIRST ';'
// after the introducer ends them and everything up to the terminator is the URI —
// which matters because a URI may legitimately contain ';'.
func osc8URI(seq string) (string, bool) {
	body, ok := strings.CutPrefix(seq, osc8Intro)
	if !ok {
		return "", false
	}
	switch {
	case strings.HasSuffix(body, "\x07"): // BEL
		body = body[:len(body)-1]
	case strings.HasSuffix(body, "\x1b\\"): // 7-bit ST
		body = body[:len(body)-2]
	case strings.HasSuffix(body, "\x9c"): // 8-bit ST
		body = body[:len(body)-1]
	default:
		return "", false
	}
	k := strings.IndexByte(body, ';')
	if k < 0 {
		return "", false
	}
	return body[k+1:], true
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
	return u.Hostname() != ""
}
