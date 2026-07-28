package models

import (
	"strings"
	"testing"
)

// A multi-part message must flatten to text with each image replaced by a
// placeholder — never its base64 payload. This is what keeps a megabyte of image
// data out of transcripts, checkpoints, and the debug log, all of which flatten
// messages through ContentToText.
func TestContentToTextImageOmitted(t *testing.T) {
	m := ChatMessage{Role: "user", Parts: []ChatContentPart{
		TextPart("a"), ImageDataPart("ZZZ", ""), TextPart("b"),
	}}
	got := m.ContentToText()
	if got != "a\n[image omitted]\nb" {
		t.Fatalf("ContentToText = %q", got)
	}
	if strings.Contains(got, "ZZZ") {
		t.Fatalf("base64 payload leaked into flattened text: %q", got)
	}
}
