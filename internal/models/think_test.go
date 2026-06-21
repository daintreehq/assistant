package models

import "testing"

// A single push with a complete think block splits visible from reasoning.
func TestThinkFilterSingleBlock(t *testing.T) {
	f := &ThinkFilter{}
	out := f.Push("hello <think>secret</think> world")
	f.End()
	if got := f.Visible(); got != "hello  world" {
		t.Fatalf("visible = %q", got)
	}
	if got := f.Reasoning(); got != "secret" {
		t.Fatalf("reasoning = %q", got)
	}
	_ = out
}

// The open tag split across two chunks must never leak a partial "<thi" into out.
func TestThinkFilterSplitOpenTag(t *testing.T) {
	f := &ThinkFilter{}
	v1 := f.Push("visible<thi")
	if v1 != "visible" {
		t.Fatalf("first push visible = %q, want %q (held-back partial tag)", v1, "visible")
	}
	v2 := f.Push("nk>reasoning</think>after")
	tail := f.End()
	if f.Visible() != "visibleafter" {
		t.Fatalf("visible = %q", f.Visible())
	}
	if f.Reasoning() != "reasoning" {
		t.Fatalf("reasoning = %q", f.Reasoning())
	}
	_ = v2
	_ = tail
}

// The close tag split across chunks must keep the partial out of reasoning until
// completed, and not emit it as visible.
func TestThinkFilterSplitCloseTag(t *testing.T) {
	f := &ThinkFilter{}
	f.Push("<think>abc</thi")
	f.Push("nk>xyz")
	f.End()
	if f.Visible() != "xyz" {
		t.Fatalf("visible = %q", f.Visible())
	}
	if f.Reasoning() != "abc" {
		t.Fatalf("reasoning = %q", f.Reasoning())
	}
}

// Tag split one byte at a time exercises keepBack at every overlap length.
func TestThinkFilterByteByByte(t *testing.T) {
	f := &ThinkFilter{}
	src := "AA<think>RR</think>BB"
	for i := 0; i < len(src); i++ {
		f.Push(src[i : i+1])
	}
	f.End()
	if f.Visible() != "AABB" {
		t.Fatalf("visible = %q", f.Visible())
	}
	if f.Reasoning() != "RR" {
		t.Fatalf("reasoning = %q", f.Reasoning())
	}
}

// An unterminated think block at end of stream is all reasoning.
func TestThinkFilterUnterminated(t *testing.T) {
	f := &ThinkFilter{}
	f.Push("vis<think>never closed")
	tail := f.End()
	if tail != "" {
		t.Fatalf("end tail = %q, want empty (in-think)", tail)
	}
	if f.Visible() != "vis" {
		t.Fatalf("visible = %q", f.Visible())
	}
	if f.Reasoning() != "never closed" {
		t.Fatalf("reasoning = %q", f.Reasoning())
	}
}

// A trailing partial tag with no completion flushes as visible at End().
func TestThinkFilterTrailingPartialFlushes(t *testing.T) {
	f := &ThinkFilter{}
	v := f.Push("done<thi")
	if v != "done" {
		t.Fatalf("push visible = %q", v)
	}
	tail := f.End()
	if tail != "<thi" {
		t.Fatalf("end tail = %q, want the held-back partial", tail)
	}
	if f.Visible() != "done<thi" {
		t.Fatalf("visible = %q", f.Visible())
	}
}

// Multi-byte runes adjacent to tags must not be split mid-rune.
func TestThinkFilterMultibyte(t *testing.T) {
	f := &ThinkFilter{}
	f.Push("héllo<think>réason</think>wörld")
	f.End()
	if f.Visible() != "héllowörld" {
		t.Fatalf("visible = %q", f.Visible())
	}
	if f.Reasoning() != "réason" {
		t.Fatalf("reasoning = %q", f.Reasoning())
	}
}

func TestStripThink(t *testing.T) {
	cases := map[string]string{
		"  <think>a</think>{\"x\":1}  ":      `{"x":1}`,
		"<think>a</think><think>b</think>hi": "hi",
		"no think here":                      "no think here",
	}
	for in, want := range cases {
		if got := stripThink(in); got != want {
			t.Errorf("stripThink(%q) = %q, want %q", in, got, want)
		}
	}
}

// The <think> filter strips a literal <think>…</think> ANYWHERE in the output —
// this is deliberate and FAITHFUL to the TS (the streaming code-unit contract that
// makes a split tag boundary-safe relies on it; constraining it would break that).
// These tests LOCK that behavior so it isn't accidentally narrowed later.

// A literal <think> tag appearing inside otherwise-visible prose is stripped, both
// incrementally (ThinkFilter) and in the whole-string path (stripThink).
func TestThinkFilterStripsLiteralTagInVisibleProse(t *testing.T) {
	f := &ThinkFilter{}
	f.Push("Use the <think>internal</think> approach.")
	f.End()
	// The literal tag and its contents are removed even though it reads like prose.
	if got := f.Visible(); got != "Use the  approach." {
		t.Fatalf("visible = %q (filter strips literal tags anywhere)", got)
	}
	if got := f.Reasoning(); got != "internal" {
		t.Fatalf("reasoning = %q", got)
	}
	if got := stripThink("Use the <think>internal</think> approach."); got != "Use the  approach." {
		t.Fatalf("stripThink = %q", got)
	}
}

// A <think>…</think> sequence sitting INSIDE a JSON string value is still stripped
// (the filter/regex are not JSON-aware). Documented so the json-path behavior is
// explicit: a model that emits the literal tag bytes inside a string loses them.
func TestThinkStripsTagInsideJSONStringValue(t *testing.T) {
	in := `{"note":"before <think>hidden</think> after"}`
	// Whole-string path (json route): the tag span is removed from the string value.
	if got := stripThink(in); got != `{"note":"before  after"}` {
		t.Fatalf("stripThink(json-string) = %q", got)
	}
	// Incremental path: same span removal, JSON quoting is not respected.
	f := &ThinkFilter{}
	f.Push(in)
	f.End()
	if got := f.Visible(); got != `{"note":"before  after"}` {
		t.Fatalf("filter(json-string) visible = %q", got)
	}
	if got := f.Reasoning(); got != "hidden" {
		t.Fatalf("reasoning = %q", got)
	}
}
