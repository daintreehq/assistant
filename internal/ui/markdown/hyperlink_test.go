package markdown

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// osc8Re matches a complete OSC 8 sequence INDEPENDENTLY of the production
// splitter, so these tests can't be fooled by a bug in osc8URI: params is
// everything up to the first ';', the URI is everything up to the terminator.
var osc8Re = regexp.MustCompile("\x1b\\]8;([^;]*);([^\x07\x1b]*)(?:\x07|\x1b\\\\)")

type osc8Seq struct{ params, uri string }

func osc8Seqs(s string) []osc8Seq {
	var out []osc8Seq
	for _, m := range osc8Re.FindAllStringSubmatch(s, -1) {
		out = append(out, osc8Seq{params: m[1], uri: m[2]})
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
func TestLongURLWrapsAsOneHyperlink(t *testing.T) {
	r := New(darkTheme())
	out := r.Render("See "+longURL+" ok", 20, false)

	rows := strings.Split(out.ANSI, "\n")
	linked := 0
	id := ""
	for i, row := range rows {
		seqs := osc8Seqs(row)
		if len(seqs) == 0 {
			continue
		}
		linked++
		var openers int
		for _, s := range seqs {
			if s.uri == "" {
				continue // closer
			}
			openers++
			if s.uri != longURL {
				t.Fatalf("row %d opener carries a TRUNCATED uri %q, want the full %q", i, s.uri, longURL)
			}
			if s.params == "" {
				t.Fatalf("row %d opener has no params; want a shared id=", i)
			}
			if id == "" {
				id = s.params
			} else if s.params != id {
				t.Fatalf("row %d opener params %q differ from %q; rows must share one link id", i, s.params, id)
			}
		}
		if openers > 0 && !strings.Contains(row, "\x1b]8;;") {
			t.Fatalf("row %d opens a hyperlink but never closes it: %q", i, row)
		}
	}
	if linked < 2 {
		t.Fatalf("expected the URL to wrap across >=2 linked rows at width 20, got %d\nrendered:\n%s",
			linked, strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
	}
	// The visible text must still spell the whole URL out across the rows.
	if !strings.Contains(strings.ReplaceAll(out.Plain, "\n", ""), longURL) {
		t.Fatalf("wrapped rows do not reassemble the URL: %q", out.Plain)
	}
}

// TestHyperlinkSchemeAllowlist pins issue #348's second constraint: only http and
// https ship as real hyperlinks. Everything else keeps its visible text and loses
// the escape — glamour would otherwise hand Daintree's xterm linkHandler a
// javascript:/file:// target to open.
func TestHyperlinkSchemeAllowlist(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantURI string // "" = no hyperlink at all
		visible string // must survive as text either way
	}{
		{"https", "[a](https://example.com/x)", "https://example.com/x", "example.com"},
		{"http", "[a](http://example.com/x)", "http://example.com/x", "example.com"},
		{"mixed case scheme", "[a](hTtPs://example.com/x)", "hTtPs://example.com/x", "example.com"},
		{"query and semicolon", "[a](https://example.com/x?a=1;b=2)", "https://example.com/x?a=1;b=2", "example.com"},
		{"mailto", "[a](mailto:someone@example.com)", "", "a"},
		{"file", "[a](file:///tmp/foo.log)", "", "a"},
		{"javascript", "[a](javascript:alert)", "", "a"},
		{"data", "[a](data:text/plain,hello)", "", "a"},
		{"relative path", "[a](./docs/BACKEND.md)", "", "a"},
		{"bare email autolink", "mail someone@example.com now", "", "someone@example.com"},
		{"hostless http", "[a](http:///foo)", "", "a"},
	}
	r := New(darkTheme())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := r.Render(tc.src, 200, false)
			seqs := osc8Seqs(out.ANSI)
			var openers []string
			for _, s := range seqs {
				if s.uri != "" {
					openers = append(openers, s.uri)
				}
			}
			if tc.wantURI == "" {
				if len(openers) != 0 {
					t.Fatalf("scheme must not be linkable, but got OSC-8 openers %q", openers)
				}
				// A dropped opener must not orphan its closer either.
				if strings.Contains(out.ANSI, "\x1b]8;") {
					t.Fatalf("stray OSC-8 left behind: %q", strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
				}
			} else {
				if len(openers) == 0 {
					t.Fatalf("expected an OSC-8 opener for %q, got none", tc.wantURI)
				}
				for _, u := range openers {
					if u != tc.wantURI {
						t.Fatalf("opener uri = %q, want %q", u, tc.wantURI)
					}
				}
			}
			if !strings.Contains(out.Plain, tc.visible) {
				t.Fatalf("visible text %q was eaten: %q", tc.visible, out.Plain)
			}
		})
	}
}

// TestFilterHyperlinkSchemes exercises the filter directly, including the shapes
// glamour never produces but a future dep bump might.
func TestFilterHyperlinkSchemes(t *testing.T) {
	const (
		bel  = "\x07"
		open = "\x1b]8;id=1;"
		shut = "\x1b]8;;" + bel
	)
	cases := []struct {
		name, in, want string
	}{
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
			want: open + "https://example.com" + bel + "keep" + ansi.ResetHyperlink() + "drop",
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
			name: "two rejected links in a row",
			in:   "\x1b]8;id=2;mailto:a@b.c" + bel + "one" + shut + " \x1b]8;id=3;data:x" + bel + "two" + shut,
			want: "one two",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterHyperlinkSchemes(tc.in)
			if got != tc.want {
				t.Fatalf("filterHyperlinkSchemes\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestHTTPHyperlinkURI pins the allowlist predicate on the malformed shapes a
// bare strings.HasPrefix check would wave through.
func TestHTTPHyperlinkURI(t *testing.T) {
	ok := []string{
		"http://example.com",
		"https://example.com/a/b?c=1#d",
		"hTtPs://Example.com",
		"https://example.com:8443/x",
		"https://user@example.com/x",
	}
	bad := []string{
		"", "http:///foo", "https://", "http:/\\evil", "http://:8080/",
		"mailto:x@y.z", "file:///tmp/x", "javascript:alert(1)", "data:text/plain,x",
		"./docs/BACKEND.md", "/abs/path", "ftp://example.com", "example.com",
		"http://exa\x07mple.com",
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

	for _, s := range osc8Seqs(out.ANSI) {
		if strings.Contains(s.uri, "evil.example") {
			t.Fatalf("model-injected OSC-8 reached the output: %q", s.uri)
		}
	}
	if !strings.Contains(out.Plain, "here") {
		t.Fatalf("stripping ate the visible text: %q", out.Plain)
	}
	// The link WE generate from the markdown source still ships.
	var sawGood bool
	for _, s := range osc8Seqs(out.ANSI) {
		if s.uri == "https://good.example/x" {
			sawGood = true
		}
	}
	if !sawGood {
		t.Fatalf("markdown-authored https link was not rendered: %q", strings.ReplaceAll(out.ANSI, "\x1b", "<ESC>"))
	}
}

// TestNoColorDropsHyperlinks: with color off the output must carry no escapes at
// all, hyperlinks included, and ANSI must equal Plain.
func TestNoColorDropsHyperlinks(t *testing.T) {
	r := New(noColorTheme())
	out := r.Render("[a](https://example.com/x) and [b](mailto:x@y.z)", 80, false)
	if strings.Contains(out.ANSI, "\x1b") {
		t.Fatalf("no-color output carries an escape: %q", out.ANSI)
	}
	if out.ANSI != out.Plain {
		t.Fatalf("no-color ANSI (%q) must equal Plain (%q)", out.ANSI, out.Plain)
	}
	if !strings.Contains(out.Plain, "example.com") {
		t.Fatalf("visible link text was eaten: %q", out.Plain)
	}
}
