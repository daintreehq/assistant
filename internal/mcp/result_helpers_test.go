package mcp

import (
	"context"
	"testing"
)

// Port of the mcpResultHelpers.test.ts contract as it manifests in internal/mcp.
// The dedicated parseMcpArray/parseMcpString matrix lives (unexported) in
// internal/daemon in the Go rewrite; the mcp package's surface for the same
// "structuredContent-first, then JSON-text fallback, ignore garbage" rule is
// ReadProjectName (the actions.getContext extractor) and the CallResult
// normalization. This table exercises that source-precedence + ignore matrix.

func TestReadProjectNameSourcePrecedence(t *testing.T) {
	cases := []struct {
		name string
		res  CallResult
		want string
	}{
		// structuredContent top-level, preferred and trimmed.
		{"structured top-level trimmed",
			CallResult{StructuredContent: map[string]any{"projectName": "  Acme  "}, Text: `{"projectName":"FromText"}`},
			"Acme"},
		// nested project.name from structuredContent.
		{"structured nested",
			CallResult{StructuredContent: map[string]any{"project": map[string]any{"name": " Nested "}}},
			"Nested"},
		// structuredContent absent → JSON-text fallback (Daintree's real shape).
		{"text fallback top-level",
			CallResult{Text: `{"projectName":"FromText"}`},
			"FromText"},
		{"text fallback nested",
			CallResult{Text: `{"project":{"name":"NestedText"}}`},
			"NestedText"},
		// structured present but field is the wrong type → fall through to text.
		{"structured wrong type falls to text",
			CallResult{StructuredContent: map[string]any{"projectName": 123}, Text: `{"projectName":"FromText"}`},
			"FromText"},
		// ignore: non-JSON text body.
		{"non-json text", CallResult{Text: "not json at all"}, ""},
		// ignore: non-object JSON (array / number / null).
		{"json array", CallResult{Text: "[1,2,3]"}, ""},
		{"json number", CallResult{Text: "42"}, ""},
		{"json null", CallResult{Text: "null"}, ""},
		// ignore: object whose projectName is blank/whitespace.
		{"blank projectName", CallResult{Text: `{"projectName":"   "}`}, ""},
		// ignore: object whose projectName is non-string.
		{"non-string projectName", CallResult{Text: `{"projectName":7}`}, ""},
		// ignore: nested project is not an object.
		{"nested project not object", CallResult{Text: `{"project":"nope"}`}, ""},
		// neither source provides anything.
		{"empty", CallResult{}, ""},
		{"empty structured object", CallResult{StructuredContent: map[string]any{}}, ""},
		// empty-string text body is valid-but-empty → "".
		{"empty text", CallResult{Text: ""}, ""},
		// whitespace-only text body → "" (TrimSpace guard, never parsed).
		{"whitespace text", CallResult{Text: "   "}, ""},
	}
	for _, tc := range cases {
		if got := ReadProjectName(tc.res); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

// TestCallResultNormalizeStructuredAndText confirms the McpCallResult normalization
// keeps structuredContent and the flattened text verbatim and side-by-side (the
// merge-both source availability that parseMcp* relies on), with Content defaulting
// to a non-nil empty slice.
func TestCallResultNormalizeStructuredAndText(t *testing.T) {
	low := &fakeLow{callResult: rawResult{
		Text:              `{"terminals":[{"id":"a"}]}`,
		Content:           nil,
		StructuredContent: map[string]any{"terminals": []any{map[string]any{"id": "b"}}},
		IsError:           false,
	}}
	c := newInjected(low)
	c.Connect(context.Background())
	res, err := c.CallTool(context.Background(), "terminal.list", nil, CallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == nil {
		t.Error("content must default to non-nil []")
	}
	if res.Text != `{"terminals":[{"id":"a"}]}` {
		t.Errorf("text body must pass through verbatim, got %q", res.Text)
	}
	if res.StructuredContent == nil {
		t.Error("structuredContent must pass through so callers can read either source")
	}
	if res.IsError {
		t.Error("isError must reflect the source (false here)")
	}
}
