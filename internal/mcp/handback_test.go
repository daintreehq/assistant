package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

func TestTerminalHandback(t *testing.T) {
	// Decoded from real JSON so numbers arrive as float64, exactly as on the wire.
	decode := func(t *testing.T, body string) map[string]any {
		t.Helper()
		var e map[string]any
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			t.Fatal(err)
		}
		return e
	}

	t.Run("full object", func(t *testing.T) {
		h := TerminalHandback(decode(t, `{"terminalId":"t","lastHandback":{"message":"did the thing","observedAt":1789800000123,"submissionToken":"tok-1","truncated":true}}`))
		if h == nil || h.Message == nil || *h.Message != "did the thing" || h.ObservedAt != 1789800000123 || h.SubmissionToken != "tok-1" || !h.Truncated {
			t.Fatalf("got %+v", h)
		}
	})
	t.Run("bare marker keeps a nil message", func(t *testing.T) {
		h := TerminalHandback(decode(t, `{"lastHandback":{"message":null,"observedAt":5,"truncated":false}}`))
		if h == nil || h.Message != nil || h.ObservedAt != 5 || h.SubmissionToken != "" || h.Truncated {
			t.Fatalf("got %+v", h)
		}
	})
	for name, body := range map[string]string{
		"absent":                `{"terminalId":"t"}`,
		"null":                  `{"lastHandback":null}`,
		"not an object":         `{"lastHandback":"done"}`,
		"missing observedAt":    `{"lastHandback":{"message":"x","truncated":false}}`,
		"string observedAt":     `{"lastHandback":{"message":"x","observedAt":"5"}}`,
		"fractional observedAt": `{"lastHandback":{"message":"x","observedAt":5.5}}`,
		"zero observedAt":       `{"lastHandback":{"message":"x","observedAt":0}}`,
		"negative observedAt":   `{"lastHandback":{"message":"x","observedAt":-5}}`,
		"absurd observedAt":     `{"lastHandback":{"message":"x","observedAt":1e30}}`,
		"non-string message":    `{"lastHandback":{"message":42,"observedAt":5}}`,
		"object message":        `{"lastHandback":{"message":{"a":1},"observedAt":5}}`,
		"non-bool truncated":    `{"lastHandback":{"message":"x","observedAt":5,"truncated":"yes"}}`,
		"non-string token":      `{"lastHandback":{"message":"x","observedAt":5,"submissionToken":7}}`,
		"oversized token":       `{"lastHandback":{"message":"x","observedAt":5,"submissionToken":"` + strings.Repeat("t", maxHandbackTokenBytes+1) + `"}}`,
	} {
		t.Run(name+" yields nil", func(t *testing.T) {
			if h := TerminalHandback(decode(t, body)); h != nil {
				t.Errorf("got %+v, want nil", h)
			}
		})
	}
	t.Run("a token at the bound is kept whole", func(t *testing.T) {
		tok := strings.Repeat("t", maxHandbackTokenBytes)
		h := TerminalHandback(map[string]any{"lastHandback": map[string]any{"observedAt": float64(9), "submissionToken": tok}})
		if h == nil || h.SubmissionToken != tok || h.Message != nil {
			t.Fatalf("got %+v", h)
		}
	})
	// Redaction happens on the RAW text: once HandbackReport has escaped the quotes
	// (`\"password\":\"…\"`) the write-boundary patterns no longer match.
	t.Run("a credential in the summary is masked before it is ever quoted", func(t *testing.T) {
		secret := "example-canary-value-0123456789"
		h := TerminalHandback(map[string]any{"lastHandback": map[string]any{
			"observedAt": float64(9), "message": `wrote config {"password":"` + secret + `"} and Authorization: Bearer ` + secret,
		}})
		if h == nil || h.Message == nil {
			t.Fatalf("got %+v", h)
		}
		if strings.Contains(*h.Message, secret) || strings.Contains(domain.HandbackReport(h), secret) {
			t.Errorf("the credential survived ingestion: %q", *h.Message)
		}
	})
	t.Run("an uncapped message is cut locally and flagged", func(t *testing.T) {
		long := strings.Repeat("é", maxHandbackMessageRunes+50)
		h := TerminalHandback(map[string]any{"lastHandback": map[string]any{"message": long, "observedAt": float64(9), "truncated": false}})
		if h == nil || h.Message == nil || len([]rune(*h.Message)) != maxHandbackMessageRunes || !h.Truncated {
			t.Fatalf("got %+v", h)
		}
	})
}
