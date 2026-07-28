package models

import (
	"encoding/json"
	"testing"
)

// multimodal_test.go covers the CONTENT-PART vocabulary: the wire shape of a text /
// image part and how a multi-part message flattens back to plain text. The provider
// round-trip tests that used to live here went with the DeepSeek transport.

func TestImageDataPartShape(t *testing.T) {
	def := ImageDataPart("AAAA", "")
	if def.Type != "image_url" || def.ImageURL != "data:image/png;base64,AAAA" {
		t.Fatalf("default part = %+v", def)
	}
	custom := ImageDataPart("ZZ", "image/jpeg")
	if custom.ImageURL != "data:image/jpeg;base64,ZZ" {
		t.Fatalf("custom mime = %q", custom.ImageURL)
	}
	// Round-trip through the wire marshaller: the image_url object must carry only a
	// url (no detail field).
	b, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}` {
		t.Fatalf("wire = %s", b)
	}
}

func TestTextPartShape(t *testing.T) {
	p := TextPart("hi")
	if p.Type != "text" || p.Text != "hi" {
		t.Fatalf("text part = %+v", p)
	}
	b, _ := json.Marshal(p)
	if string(b) != `{"type":"text","text":"hi"}` {
		t.Fatalf("wire = %s", b)
	}
}

func TestContentToTextFlatten(t *testing.T) {
	if got := (ChatMessage{Role: "user", StringContent: ""}).ContentToText(); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := TextMessage("user", "hello").ContentToText(); got != "hello" {
		t.Fatalf("string = %q", got)
	}
	m := ChatMessage{Role: "user", Parts: []ChatContentPart{TextPart("Describe this"), ImageDataPart("bigbase64", "")}}
	if got := m.ContentToText(); got != "Describe this\n[image omitted]" {
		t.Fatalf("flatten = %q", got)
	}
}
