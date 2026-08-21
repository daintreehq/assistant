package markdown

import (
	"strings"
	"testing"
)

// c1OSC / c1ST are the C1 (8-bit) forms of the OSC introducer and the string
// terminator. xterm decodes UTF-8 to code points and honours these exactly as it
// honours the 7-bit ESC-introduced forms.
const (
	c1OSC = "\u009d"
	c1ST  = "\u009c"
)

// TestEntityEncodedControlsNeutralised: glamour un-escapes HTML entities in text
// nodes AFTER sanitizeInput has run, so "&#27;[2J" and "&#x9d;" come back as live
// controls no matter how clean the source was. sanitizeOutput is what stops them.
func TestEntityEncodedControlsNeutralised(t *testing.T) {
	cases := []struct{ name, src, banned, mustContain string }{
		{"csi clear screen", "before &#27;[2J after", "\x1b[2J", "[2J"},
		{"c1 osc introducer", "look &#x9d;8;;file:///etc/passwd&#7;click", c1OSC, "file:///etc/passwd"},
		{"bare bel", "ring &#7; here", "\x07", "ring"},
		{"c1 string terminator", "x &#x9c; y", c1ST, "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, th := range []struct {
				name string
				r    *Renderer
			}{{"dark", New(darkTheme())}, {"nocolor", New(noColorTheme())}} {
				out := th.r.Render(tc.src, 200, false)
				for _, got := range []string{out.ANSI, out.Plain} {
					if strings.Contains(got, tc.banned) {
						t.Fatalf("%s: live control survived: %q", th.name, got)
					}
				}
				// The payload stays visible - inert, but legible to the human.
				if !strings.Contains(out.Plain, tc.mustContain) {
					t.Fatalf("%s: text was eaten: %q", th.name, out.Plain)
				}
			}
		})
	}
}

// TestSanitizeOutputKeepsOurEscapes: the validator must be a scalpel - our own SGR
// and validated OSC-8 survive untouched, everything else loses its introducer.
func TestSanitizeOutputKeepsOurEscapes(t *testing.T) {
	keep := "a \x1b[38;2;1;2;3;4mstyled\x1b[m b \x1b]8;id=1;https://example.com\x07link\x1b]8;;\x07 c"
	if got := sanitizeOutput(keep); got != keep {
		t.Fatalf("sanitizeOutput altered our own escapes:\n got %q\nwant %q", got, keep)
	}
	cases := []struct{ name, in, want string }{
		{"non-SGR CSI loses its introducer", "a \x1b[2J b", "a [2J b"},
		{"cursor CSI too", "a \x1b[10;5H b", "a [10;5H b"},
		{"other OSC loses its introducer", "a \x1b]0;title\x07 b", "a ]0;title b"},
		{"bare BEL is dropped", "a \x07 b", "a  b"},
		{"C1 OSC is dropped", "a " + c1OSC + "8;;x" + c1ST + " b", "a 8;;x b"},
		{"DEL is dropped", "a \x7f b", "a  b"},
		{"newline and tab survive", "a\n\tb", "a\n\tb"},
		{"clean text is returned unchanged", "plain ünicode ok", "plain ünicode ok"},
		{"an unterminated OSC-8 loses its introducer", "a \x1b]8;id=1;https://x b", "a ]8;id=1;https://x b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeOutput(tc.in); got != tc.want {
				t.Fatalf("sanitizeOutput(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSemicolonURLWrapsAsOneHyperlink is the other half of issue #348's contract.
// lipgloss's own OSC parser splits on every ';' and demands exactly three fields,
// so a URI containing one is never tracked and the rows after the first would
// carry no target - reopenAcrossRows is what makes the guarantee ours.
func TestSemicolonURLWrapsAsOneHyperlink(t *testing.T) {
	const u = "https://example.com/aaaaaaaaaaaaaaaaaaaaaaaa;a=bbbbbbbbbbbbbbbbbbbbbbbb"
	out := New(darkTheme()).Render("See "+u+" ok", 20, false)

	linkedRows := 0
	for i, row := range strings.Split(out.ANSI, "\n") {
		ops := openers(row)
		if len(ops) == 0 {
			continue
		}
		linkedRows++
		for _, sq := range ops {
			if sq.uri != u {
				t.Fatalf("row %d carries a truncated uri %q, want %q", i, sq.uri, u)
			}
		}
	}
	if linkedRows < 2 {
		t.Fatalf("a semicolon-bearing URL must carry its target on every wrapped row, got %d\n%s",
			linkedRows, strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
	}
}

// TestNonASCIIURLNotShippedTruncated: x/ansi's wrapper cuts an OSC payload at the
// 0x9C continuation byte of a rune like U+00DC, so lipgloss can hand us per-row
// openers carrying a TRUNCATED target. Shipping one would be the very
// mis-directed link issue #348 is about, so the link is dropped instead.
func TestNonASCIIURLNotShippedTruncated(t *testing.T) {
	u := "https://example.com/Ü" + strings.Repeat("T", 40)
	out := New(darkTheme()).Render("[LBL]("+u+")", 20, false)
	for _, sq := range openers(out.ANSI) {
		if sq.uri != u {
			t.Fatalf("shipped a truncated target %q (want the full %q, or no link at all)", sq.uri, u)
		}
	}
	if strings.ContainsAny(out.Plain, "\x07\x1b") {
		t.Fatalf("control leaked into visible text: %q", out.Plain)
	}
}

// TestSanitizeInputNormalisesCR: CommonMark treats a lone CR as a line ending, so
// deleting it would weld two blocks together.
func TestSanitizeInputNormalisesCR(t *testing.T) {
	if got := sanitizeInput("# one\r# two"); got != "# one\n# two" {
		t.Fatalf("lone CR: got %q, want %q", got, "# one\n# two")
	}
	if got := sanitizeInput("a\r\nb"); got != "a\nb" {
		t.Fatalf("CRLF: got %q, want %q", got, "a\nb")
	}
	out := New(darkTheme()).Render("# one\r# two", 40, false)
	if strings.Contains(out.Plain, "one# two") {
		t.Fatalf("lone CR welded two headings into one: %q", out.Plain)
	}
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(out.Plain, want) {
			t.Fatalf("heading %q was eaten: %q", want, out.Plain)
		}
	}
}
