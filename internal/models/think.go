package models

import (
	"bytes"
	"regexp"
	"strings"
)

// thinkOpen / thinkClose are the reasoning-tag boundaries. They are ASCII, so all
// the indexing/slicing below is byte-safe even when buf holds multi-byte runes:
// the only places we slice are at a tag boundary or at a keepBack offset, neither
// of which can fall inside a multi-byte rune (the held-back tail is re-joined on
// the next push, so a transiently rune-split tail is harmless).
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// ThinkFilter incrementally separates <think>…</think> reasoning from visible
// content across streamed deltas, holding back a possibly-partial tag at the tail
// so a split token ("<thi" / "<think>" straddling two chunks) is never
// mis-emitted. Operates on bytes (the tags are ASCII).
type ThinkFilter struct {
	inThink   bool
	buf       []byte
	reasoning []byte
	visible   []byte
}

// Push appends a content delta and returns the newly visible (non-think) text.
func (f *ThinkFilter) Push(delta string) string {
	f.buf = append(f.buf, delta...)
	var out []byte
	for len(f.buf) > 0 {
		if !f.inThink {
			open := bytes.Index(f.buf, []byte(thinkOpen))
			if open == -1 {
				// Hold back a possible partial "<think>" tag at the tail.
				safe := keepBack(f.buf, thinkOpen)
				out = append(out, f.buf[:safe]...)
				f.buf = f.buf[safe:]
				break
			}
			out = append(out, f.buf[:open]...)
			f.buf = f.buf[open+len(thinkOpen):]
			f.inThink = true
		} else {
			closeIdx := bytes.Index(f.buf, []byte(thinkClose))
			if closeIdx == -1 {
				safe := keepBack(f.buf, thinkClose)
				f.reasoning = append(f.reasoning, f.buf[:safe]...)
				f.buf = f.buf[safe:]
				break
			}
			f.reasoning = append(f.reasoning, f.buf[:closeIdx]...)
			f.buf = f.buf[closeIdx+len(thinkClose):]
			f.inThink = false
		}
	}
	// Re-slice buf onto a fresh backing array so the held-back tail isn't aliased
	// by a future append that could corrupt out (out shares no storage with buf,
	// but be defensive against the append-grow reuse pattern).
	if len(f.buf) > 0 {
		f.buf = append([]byte(nil), f.buf...)
	} else {
		f.buf = f.buf[:0]
	}
	f.visible = append(f.visible, out...)
	return string(out)
}

// End flushes any held-back tail at end of stream. An unterminated think block is
// treated as all-reasoning (it never reached a close tag).
func (f *ThinkFilter) End() string {
	rest := f.buf
	f.buf = nil
	if f.inThink {
		f.reasoning = append(f.reasoning, rest...)
		return ""
	}
	f.visible = append(f.visible, rest...)
	return string(rest)
}

// Visible returns the accumulated visible text so far.
func (f *ThinkFilter) Visible() string { return string(f.visible) }

// Reasoning returns the accumulated reasoning text so far.
func (f *ThinkFilter) Reasoning() string { return string(f.reasoning) }

// keepBack returns the largest prefix length of s safe to emit without splitting a
// possible tag: it finds the longest suffix of s that is a prefix of tag and holds
// it back. Both operands are byte slices; tag is ASCII.
func keepBack(s []byte, tag string) int {
	maxOverlap := len(tag) - 1
	if len(s) < maxOverlap {
		maxOverlap = len(s)
	}
	for k := maxOverlap; k > 0; k-- {
		if bytes.Equal(s[len(s)-k:], []byte(tag[:k])) {
			return len(s) - k
		}
	}
	return len(s)
}

// thinkBlockRe matches a full <think>…</think> block, non-greedy + dot-all, for
// stripThink.
var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThink removes complete <think>…</think> blocks (used by the json path),
// then trims surrounding whitespace.
func stripThink(s string) string {
	return strings.TrimSpace(thinkBlockRe.ReplaceAllString(s, ""))
}
