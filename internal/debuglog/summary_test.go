package debuglog

import (
	"strings"
	"testing"
)

func TestSummarizeShortString(t *testing.T) {
	s := Summarize("hello", 100)
	if s.Bytes != 5 {
		t.Errorf("Bytes = %d want 5", s.Bytes)
	}
	if s.Truncated {
		t.Error("short string must not be marked truncated")
	}
	if s.Preview != "hello" {
		t.Errorf("Preview = %q want hello", s.Preview)
	}
	if !strings.HasPrefix(s.SHA, "sha256:") {
		t.Errorf("SHA = %q want sha256: prefix", s.SHA)
	}
}

func TestSummarizeTruncatesAtRuneBoundary(t *testing.T) {
	// 10 multibyte runes, budget 4 → preview is exactly 4 runes, flagged truncated.
	in := strings.Repeat("é", 10)
	s := Summarize(in, 4)
	if !s.Truncated {
		t.Error("over-budget string must be marked truncated")
	}
	if got := len([]rune(s.Preview)); got != 4 {
		t.Errorf("preview rune count = %d want 4", got)
	}
	if s.Bytes != len(in) {
		t.Errorf("Bytes = %d want %d (full byte length, not preview)", s.Bytes, len(in))
	}
}

func TestSummarizeEmptyIsZero(t *testing.T) {
	s := Summarize("", 100)
	if s.Bytes != 0 || s.SHA != "" || s.Preview != "" || s.Truncated {
		t.Errorf("empty Summarize = %+v want zero value", s)
	}
}

func TestSummarizeIdenticalPayloadsShareSHA(t *testing.T) {
	a := Summarize("the same body", 4)
	b := Summarize("the same body", 100)
	if a.SHA != b.SHA {
		t.Errorf("identical payloads got different SHAs: %q vs %q", a.SHA, b.SHA)
	}
	if !a.Truncated || b.Truncated {
		t.Error("truncation flag should reflect each call's budget, not the shared hash")
	}
}

func TestSummarizeJSONFallsBackOnMarshalError(t *testing.T) {
	// A channel cannot be marshaled — SummarizeJSON must not panic and must produce a
	// non-empty summary from the %v rendering instead.
	s := SummarizeJSON(make(chan int), 100)
	if s.Bytes == 0 {
		t.Error("expected a non-empty fallback summary on marshal error")
	}
}

func TestPreviewEllipsis(t *testing.T) {
	if got := Preview("short", 100); got != "short" {
		t.Errorf("Preview(short) = %q want short", got)
	}
	long := strings.Repeat("x", 50)
	got := Preview(long, 10)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("over-budget Preview = %q want trailing ellipsis", got)
	}
	if got := len([]rune(got)); got != 11 { // 10 runes + the ellipsis
		t.Errorf("Preview rune count = %d want 11", got)
	}
}
