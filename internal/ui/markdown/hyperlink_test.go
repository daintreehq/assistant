package markdown

import (
	"strings"
	"testing"
)

// findOSC8 is a test-only OSC 8 scanner, written independently of the production
// scanOSC8 so a bug there cannot also blind the assertions. It recognises only
// what our renderer actually emits: ESC ] 8 ; params ; uri, terminated by BEL or
// 7-bit ST. It never crosses a terminator, so a malformed sequence is skipped
// rather than swallowing the text after it.
func findOSC8(s string) []osc8Seq {
	var out []osc8Seq
	for i := 0; ; {
		j := strings.Index(s[i:], "\x1b]8;")
		if j < 0 {
			return out
		}
		j += i
		body := s[j+4:]
		k := -1
		width := 0
		for n := 0; n < len(body); n++ {
			if body[n] == '\x07' {
				k, width = n, 1
				break
			}
			if body[n] == '\x1b' && n+1 < len(body) && body[n+1] == '\\' {
				k, width = n, 2
				break
			}
		}
		if k < 0 {
			return out // unterminated: nothing further is a real sequence
		}
		if sep := strings.IndexByte(body[:k], ';'); sep >= 0 {
			out = append(out, osc8Seq{params: body[:sep], uri: body[sep+1 : k]})
		}
		i = j + 4 + k + width
	}
}

type osc8Seq struct{ params, uri string }

func openers(s string) []osc8Seq {
	var out []osc8Seq
	for _, sq := range findOSC8(s) {
		if sq.uri != "" {
			out = append(out, sq)
		}
	}
	return out
}

const longURL = "https://github.com/daintreehq/daintree/issues/11874#issuecomment-1234567890"

// TestLongURLWrapsAsOneHyperlink is the regression pin for issue #348. A URL
// longer than the content width is HARD-wrapped (we own wrapping by design — the
// host must never autowrap our frames), so the host terminal cannot rejoin the
// rows. What makes the link survive is that every physical row carries its own
// OSC-8 span pointing at the COMPLETE URL, all sharing one id. If a glamour or
// lipgloss bump ever stops reopening the span per row, this fails.
//
// KNOWN GAP, upstream: a URI containing ';' defeats the OSC parsing in lipgloss's
// own dependency (it splits the sequence on every ';' and requires exactly three
// fields), so such a link is tracked on its first row only and the rows after it
// carry no target at all. Nothing in this package can close that, so this test
// uses a semicolon-free URL rather than asserting a behaviour we cannot deliver.
func TestLongURLWrapsAsOneHyperlink(t *testing.T) {
	r := New(darkTheme())
	out := r.Render("See "+longURL+" ok", 20, false)

	rowsWithOpener := 0
	id := ""
	for i, row := range strings.Split(out.ANSI, "\n") {
		ops := openers(row)
		if len(ops) == 0 {
			continue
		}
		rowsWithOpener++
		for _, sq := range ops {
			if sq.uri != longURL {
				t.Fatalf("row %d opener carries a TRUNCATED uri %q, want the full %q", i, sq.uri, longURL)
			}
			if !strings.HasPrefix(sq.params, "id=") || sq.params == "id=" {
				t.Fatalf("row %d opener params = %q, want a non-empty id=", i, sq.params)
			}
			if id == "" {
				id = sq.params
			} else if sq.params != id {
				t.Fatalf("row %d opener params %q differ from %q; rows must share one link id", i, sq.params, id)
			}
		}
		// Each row must also CLOSE its span, or the link bleeds into the next row.
		var closers int
		for _, sq := range findOSC8(row) {
			if sq.uri == "" {
				closers++
			}
		}
		if closers == 0 {
			t.Fatalf("row %d opens a hyperlink but never closes it: %q", i, row)
		}
	}
	if rowsWithOpener < 2 {
		t.Fatalf("expected the URL to wrap across >=2 rows each carrying an opener, got %d\nrendered:\n%s",
			rowsWithOpener, strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
	}
	if !strings.Contains(strings.ReplaceAll(out.Plain, "\n", ""), longURL) {
		t.Fatalf("wrapped rows do not reassemble the URL: %q", out.Plain)
	}
}

// TestHyperlinkSchemeAllowlist pins issue #348's second constraint: only http and
// https ship as real hyperlinks. Everything else keeps its visible text and loses
// the escape — glamour would otherwise hand Daintree's xterm linkHandler a
// javascript:/file:// target to open.
//
// Labels are distinctive so "the visible text survived" cannot be satisfied by a
// substring of the href that glamour prints beside them.
func TestHyperlinkSchemeAllowlist(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantURI string   // "" = no hyperlink at all
		visible []string // every one must survive as text
	}{
		{"https", "[LBLHTTPS](https://example.com/x)", "https://example.com/x", []string{"LBLHTTPS", "https://example.com/x"}},
		{"http", "[LBLHTTP](http://example.com/x)", "http://example.com/x", []string{"LBLHTTP", "http://example.com/x"}},
		{"mixed case scheme", "[LBLCASE](hTtPs://example.com/x)", "hTtPs://example.com/x", []string{"LBLCASE"}},
		{"semicolon in uri", "[LBLSEMI](https://example.com/x?a=1;b=2)", "https://example.com/x?a=1;b=2", []string{"LBLSEMI"}},
		{"non-ascii in uri", "[LBLRUNE](https://example.com/ÜTAIL)", "https://example.com/ÜTAIL", []string{"LBLRUNE"}},
		{"mailto", "[LBLMAIL](mailto:someone@example.com)", "", []string{"LBLMAIL"}},
		{"file", "[LBLFILE](file:///tmp/foo.log)", "", []string{"LBLFILE", "file:///tmp/foo.log"}},
		{"javascript", "[LBLJS](javascript:alert)", "", []string{"LBLJS", "javascript:alert"}},
		{"data", "[LBLDATA](data:text/plain,hello)", "", []string{"LBLDATA"}},
		{"relative path", "[LBLREL](./docs/BACKEND.md)", "", []string{"LBLREL"}},
		{"bare email autolink", "mail someone@example.com now", "", []string{"someone@example.com"}},
		{"hostless http", "[LBLHOSTLESS](http:///foo)", "", []string{"LBLHOSTLESS"}},
		{"userinfo", "[LBLUSER](https://good.example@evil.example/x)", "", []string{"LBLUSER"}},
		{"non-ascii in rejected uri", "[LBLRUNEBAD](file:///tmp/ÜTAIL)", "", []string{"LBLRUNEBAD"}},
	}
	r := New(darkTheme())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.Render(tc.src, 200, false)
			ops := openers(out.ANSI)
			if tc.wantURI == "" {
				if len(ops) != 0 {
					t.Fatalf("scheme must not be linkable, but got OSC-8 openers %+v", ops)
				}
				// A dropped opener must not orphan its closer either.
				if strings.Contains(out.ANSI, osc8Intro) {
					t.Fatalf("stray OSC-8 left behind: %q", strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
				}
			} else {
				if len(ops) == 0 {
					t.Fatalf("expected an OSC-8 opener for %q, got none", tc.wantURI)
				}
				for _, sq := range ops {
					if sq.uri != tc.wantURI {
						t.Fatalf("opener uri = %q, want %q", sq.uri, tc.wantURI)
					}
				}
			}
			for _, want := range tc.visible {
				if !strings.Contains(out.Plain, want) {
					t.Fatalf("visible text %q was eaten: %q", want, out.Plain)
				}
			}
			// Nothing the filter removes may leave a stray control behind. A BEL in
			// particular means a sequence was mis-framed and its tail leaked.
			if strings.ContainsAny(out.Plain, "\x07\x1b") {
				t.Fatalf("control character leaked into visible text: %q", out.Plain)
			}
		})
	}
}

// TestHyperlinkAllowlistAcrossBlockKinds covers the OTHER paths glamour takes to a
// hyperlink: headings, list items and blockquotes each wrap through different
// style writers, and a table renders links via its separate footer-link renderer.
func TestHyperlinkAllowlistAcrossBlockKinds(t *testing.T) {
	cases := []struct{ name, src string }{
		{"heading", "# head [LBL](file:///tmp/x)"},
		{"list item", "- item [LBL](file:///tmp/x)"},
		{"nested list", "- outer\n  - inner [LBL](file:///tmp/x)"},
		{"blockquote", "> quoted [LBL](file:///tmp/x)"},
		{"table cell", "| a | b |\n| --- | --- |\n| x | [LBL](file:///tmp/x) |"},
	}
	r := New(darkTheme())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.Render(tc.src, 80, false)
			if strings.Contains(out.ANSI, osc8Intro) {
				t.Fatalf("file:// target survived in a %s: %q", tc.name, strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
			}
			if !strings.Contains(out.Plain, "LBL") {
				t.Fatalf("label was eaten in a %s: %q", tc.name, out.Plain)
			}
		})
	}
}

// TestHyperlinkFilterIsCached pins that the FILTERED value is what the LRU stores,
// so a second render of the same content cannot serve a pre-filter result.
func TestHyperlinkFilterIsCached(t *testing.T) {
	r := New(darkTheme())
	const src = "[LBL](file:///tmp/x) and [OK](https://example.com/y)"
	first := r.Render(src, 80, false)
	second := r.Render(src, 80, false) // cache hit
	if first.ANSI != second.ANSI {
		t.Fatalf("cached render differs from the first:\n1: %q\n2: %q", first.ANSI, second.ANSI)
	}
	for _, sq := range openers(second.ANSI) {
		if sq.uri != "https://example.com/y" {
			t.Fatalf("cache served an unfiltered opener %q", sq.uri)
		}
	}
}

// TestFilterHyperlinkSchemes exercises the filter directly, including the shapes
// glamour never produces but a future dep bump or a parser change might.
func TestFilterHyperlinkSchemes(t *testing.T) {
	const (
		bel   = "\x07"
		open  = "\x1b]8;id=1;"
		shut  = "\x1b]8;;" + bel
		reset = "\x1b]8;;\x07" // written out, not ansi.ResetHyperlink(), so the
		// oracle cannot break in lockstep with the implementation
	)
	cases := []struct{ name, in, want string }{
		{
			name: "no hyperlink is returned unchanged",
			in:   "plain \x1b[31mred\x1b[0m text",
			want: "plain \x1b[31mred\x1b[0m text",
		},
		{
			name: "allowed span survives byte for byte",
			in:   "a " + open + "https://example.com" + bel + "\x1b[4mtext\x1b[m" + shut + " b",
			want: "a " + open + "https://example.com" + bel + "\x1b[4mtext\x1b[m" + shut + " b",
		},
		{
			name: "rejected span loses only the escapes",
			in:   "a \x1b]8;id=2;mailto:x@y.z" + bel + "\x1b[4mtext\x1b[m" + shut + " b",
			want: "a \x1b[4mtext\x1b[m b",
		},
		{
			name: "ST terminator is handled",
			in:   "a \x1b]8;id=2;mailto:x@y.z\x1b\\text\x1b]8;;\x1b\\ b",
			want: "a text b",
		},
		{
			// The injected reset closes the allowed target before the rejected span,
			// which leaves nothing open — so the rejected link's own trailing closer
			// is redundant and goes with it.
			name: "rejected opener superseding an allowed one emits a reset",
			in:   open + "https://example.com" + bel + "keep\x1b]8;id=2;file:///tmp/x" + bel + "drop" + shut,
			want: open + "https://example.com" + bel + "keep" + reset + "drop",
		},
		{
			name: "an allowed opener after a rejected one keeps its own closer",
			in:   "\x1b]8;id=2;file:///tmp/x" + bel + "drop" + open + "https://example.com" + bel + "keep" + shut,
			want: "drop" + open + "https://example.com" + bel + "keep" + shut,
		},
		{
			name: "consecutive allowed openers both survive",
			in:   open + "https://a.example" + bel + "one" + shut + open + "https://b.example" + bel + "two" + shut,
			want: open + "https://a.example" + bel + "one" + shut + open + "https://b.example" + bel + "two" + shut,
		},
		{
			name: "a standalone closer is preserved",
			in:   "text" + shut + " more",
			want: "text" + shut + " more",
		},
		{
			name: "an unterminated opener is left alone",
			in:   "a \x1b]8;id=2;mailto:x@y.z",
			want: "a \x1b]8;id=2;mailto:x@y.z",
		},
		{
			// CAN aborts the string; the terminal resumes at that byte and so do we,
			// which means the rejected link that follows is still caught.
			name: "a cancelled sequence does not stop the scan",
			in:   "\x1b]8;id=2;file:///a\x18tail\x1b]8;id=3;file:///b" + bel + "drop" + shut,
			want: "\x1b]8;id=2;file:///a\x18taildrop",
		},
		{
			// 0x9c is NOT a terminator here: in UTF-8 it is the continuation byte of
			// an ordinary rune, so honouring it would truncate the URI mid-rune and
			// spill its tail (and the real BEL) into visible text.
			name: "a 0x9c byte inside a uri is not a terminator",
			in:   "\x1b]8;id=2;file:///tmp/ÜTAIL" + bel + "label" + shut,
			want: "label",
		},
		{
			name: "two rejected links in a row",
			in:   "\x1b]8;id=2;mailto:a@b.c" + bel + "one" + shut + " \x1b]8;id=3;data:x" + bel + "two" + shut,
			want: "one two",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := filterHyperlinkSchemes(tc.in); got != tc.want {
				t.Fatalf("filterHyperlinkSchemes\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestHTTPHyperlinkURI pins the allowlist predicate on the malformed and
// deceptive shapes a bare strings.HasPrefix check would wave through.
func TestHTTPHyperlinkURI(t *testing.T) {
	ok := []string{
		"http://example.com",
		"https://example.com/a/b?c=1#d",
		"hTtPs://Example.com",
		"https://example.com:8443/x",
		"https://example.com/a;b=c",
		"https://[::1]/x",
		"https://127.0.0.1:8080/",
	}
	bad := []string{
		"", "http:///foo", "https://", "http:/\\evil", "http://:8080/",
		"mailto:x@y.z", "file:///tmp/x", "javascript:alert(1)", "data:text/plain,x",
		"./docs/BACKEND.md", "/abs/path", "//example.com", "ftp://example.com",
		"example.com", "http://exa\x07mple.com", "http:example.com",
		"https://user:pass@evil.example/", "https://good.example@evil.example/",
	}
	for _, u := range ok {
		if !httpHyperlinkURI(u) {
			t.Errorf("httpHyperlinkURI(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if httpHyperlinkURI(u) {
			t.Errorf("httpHyperlinkURI(%q) = true, want false", u)
		}
	}
}

// TestInjectedHyperlinkStrippedWithColor complements TestANSIInputStripped, which
// runs on the no-color theme and so cannot tell input sanitisation apart from the
// no-color output strip. Here color is ON and the injected target is https — the
// one scheme the output filter would happily keep — so its absence can only mean
// the INPUT was sanitised before parsing.
func TestInjectedHyperlinkStrippedWithColor(t *testing.T) {
	r := New(darkTheme())
	injected := "look \x1b]8;id=9;https://evil.example/steal\x07here\x1b]8;;\x07 and [real](https://good.example/x)"
	out := r.Render(injected, 200, false)

	for _, sq := range findOSC8(out.ANSI) {
		if strings.Contains(sq.uri, "evil.example") {
			t.Fatalf("model-injected OSC-8 reached the output: %q", sq.uri)
		}
	}
	if !strings.Contains(out.Plain, "here") {
		t.Fatalf("stripping ate the visible text: %q", out.Plain)
	}
	var sawGood bool
	for _, sq := range openers(out.ANSI) {
		if sq.uri == "https://good.example/x" {
			sawGood = true
		}
	}
	if !sawGood {
		t.Fatalf("markdown-authored https link was not rendered: %q", strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
	}
}

// TestC1ControlsStrippedFromInput closes the hole an ESC-gated sanitiser leaves:
// xterm decodes UTF-8 and honours the C1 controls, so U+009D ("8-bit OSC") plus
// "8;;file:///etc/passwd" U+009C is a working clickable local-file target that
// contains no ESC byte at all — and so never reached ansi.Strip.
func TestC1ControlsStrippedFromInput(t *testing.T) {
	const (
		osc8 = "\u009d" // 8-bit OSC introducer
		st   = "\u009c" // 8-bit string terminator
	)
	injected := "look " + osc8 + "8;;file:///etc/passwd" + st + "click" + osc8 + "8;;" + st + " end"
	for _, tc := range []struct {
		name string
		r    *Renderer
	}{
		{"dark", New(darkTheme())},
		{"nocolor", New(noColorTheme())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.r.Render(injected, 200, false)
			if strings.ContainsAny(out.ANSI, osc8+st) || strings.ContainsAny(out.Plain, osc8+st) {
				t.Fatalf("C1 control survived to the output: %q", out.ANSI)
			}
			if !strings.Contains(out.Plain, "click") {
				t.Fatalf("sanitising ate the visible text: %q", out.Plain)
			}
		})
	}
}

// TestNoColorDropsHyperlinks: with color off the output must carry no escapes at
// all, hyperlinks included, and ANSI must equal Plain.
func TestNoColorDropsHyperlinks(t *testing.T) {
	r := New(noColorTheme())
	out := r.Render("[LBLA](https://example.com/x) and [LBLB](mailto:x@y.z)", 80, false)
	if strings.Contains(out.ANSI, "\x1b") {
		t.Fatalf("no-color output carries an escape: %q", out.ANSI)
	}
	if out.ANSI != out.Plain {
		t.Fatalf("no-color ANSI (%q) must equal Plain (%q)", out.ANSI, out.Plain)
	}
	for _, want := range []string{"LBLA", "LBLB", "example.com"} {
		if !strings.Contains(out.Plain, want) {
			t.Fatalf("visible text %q was eaten: %q", want, out.Plain)
		}
	}
}

// TestSanitizeInputKeepsLayout: the sanitiser must not eat the characters
// markdown's block structure is made of.
func TestSanitizeInputKeepsLayout(t *testing.T) {
	in := "# head\n\n- a\tb\n- c\n\nÜnicode ok"
	if got := sanitizeInput(in); got != in {
		t.Fatalf("sanitizeInput altered ordinary markdown:\n got %q\nwant %q", got, in)
	}
	if got := sanitizeInput("a\rb\x00c\x7fd"); got != "abcd" {
		t.Fatalf("sanitizeInput(%q) = %q, want %q", "a\\rb\\x00c\\x7fd", got, "abcd")
	}
}
