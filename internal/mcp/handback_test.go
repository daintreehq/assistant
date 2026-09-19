package mcp

import (
	"encoding/json"
	"strings"
	"testing"
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
	} {
		t.Run(name+" yields nil", func(t *testing.T) {
			if h := TerminalHandback(decode(t, body)); h != nil {
				t.Errorf("got %+v, want nil", h)
			}
		})
	}
	t.Run("an uncapped message is cut locally and flagged", func(t *testing.T) {
		long := strings.Repeat("é", maxHandbackMessageRunes+50)
		h := TerminalHandback(map[string]any{"lastHandback": map[string]any{"message": long, "observedAt": float64(9), "truncated": false}})
		if h == nil || h.Message == nil || len([]rune(*h.Message)) != maxHandbackMessageRunes || !h.Truncated {
			t.Fatalf("got %+v", h)
		}
	})
}
