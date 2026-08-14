package tools

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/redact"
)

// maxAuditJSON caps the audited JSON length; preview keeps the stored row near
// the cap rather than ballooning on large file/scrollback results.
const (
	maxAuditJSON    = 4000
	previewHeadroom = 200 // wrapper+escaping headroom → preview = first 3800 chars
)

// safeJSON marshals v to a string; on a marshal error returns the sentinel
// "<unserializable>" so the audit path never throws.
//
// The value is REDACTED STRUCTURALLY — walked before marshaling, never regexed after.
//
// Why redact at all: audit rows are the most durable copy of a tool call in the system.
// They outlive the conversation, they are queryable, and `audit.export` hands them to the
// model as JSON or CSV — so a credential in an argument gets re-read into a later prompt.
// Args and results carry whatever the model and the terminal put there: an
// `export TOKEN=…`, a git remote with an inline credential, an agent echoing its
// environment. This one helper covers both the args and the result column.
//
// Why structurally: running the patterns over serialized JSON CORRUPTED it. The
// env-assignment pattern's value ran to the next whitespace, and minified JSON has none,
// so `{"command":"export API_KEY=abc","path":"keep"}` lost everything after the key and
// stopped being parseable. `redact.Value` walks the decoded value instead, so a string is
// redacted within its own boundary and re-marshaling always yields valid JSON — which
// matters here more than anywhere, because `audit.export` promises exactly that.
func safeJSON(v any) string {
	data, err := json.Marshal(redact.Value(v))
	if err != nil {
		return `"<unserializable>"`
	}
	return string(data)
}

// capJSON truncates an oversized JSON blob to a redacted preview + byte count.
// `bytes` is the UTF-8 byte length of the FULL blob; the preview is the first
// (maxAuditJSON-previewHeadroom) chars (runes).
func capJSON(s string) string {
	// Threshold on rune count (TS uses UTF-16 .length; rune-count is the faithful
	// single-unit choice and keeps multibyte slicing valid).
	runes := []rune(s)
	if len(runes) <= maxAuditJSON {
		return s
	}
	preview := string(runes[:maxAuditJSON-previewHeadroom])
	wrapped, err := json.Marshal(map[string]any{
		"truncated": true,
		"bytes":     len([]byte(s)), // UTF-8 byte length of the full blob
		"preview":   preview,
	})
	if err != nil {
		// Should never happen, but never break the audit path.
		return preview
	}
	return string(wrapped)
}

// rawAsAny unmarshals a json.RawMessage into a generic value for the debug log
// (so it logs structured data, not an escaped string). Falls back to the raw
// string on a decode error. nil/empty → nil.
func rawAsAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

// decodeDetails normalizes a Decode error into the ToolResult.Error.Details
// payload. A *DecodeError carries structured field issues; any other error
// degrades to its message string.
func decodeDetails(err error) any {
	if de, ok := err.(*DecodeError); ok {
		return de.Issues
	}
	return err.Error()
}

// --- Strict decoding helper (replaces Zod safeParse + reject-unknown-fields) ---

// DecodeError is the structured failure of a strict Decode. Its Error() is the
// one-line message; Issues is the machine-readable detail (mirrors Zod issues).
type DecodeError struct {
	Message string
	Issues  []string
}

func (e *DecodeError) Error() string { return e.Message }

// DecodeStrict decodes raw into *out, REJECTING unknown fields (the Go analogue
// of a Zod .strict() schema). Use it to build a Tool.Decode that both validates
// shape and rejects extra keys. The returned canonical args (re-marshaled from
// *out) carry defaults/coercion so the handler sees the parsed form.
//
// Usage in a tool family:
//
//	Decode: tools.StrictDecoder(func() any { return &MyArgs{} })
func DecodeStrict(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return &DecodeError{
			Message: err.Error(),
			Issues:  []string{err.Error()},
		}
	}
	// Guard against trailing junk after the first JSON value.
	if dec.More() {
		return &DecodeError{
			Message: "unexpected trailing content after JSON value",
			Issues:  []string{"trailing content"},
		}
	}
	return nil
}

// Validator is implemented by args structs that need post-decode bound/semantic
// checks (the Go analogue of Zod's .min()/.max()/.positive() refinements that
// strict JSON decoding alone can't express). StrictDecoder calls Validate after a
// successful decode; a returned error becomes the INVALID_ARGS detail. Returning a
// *DecodeError carries structured issues; any other error degrades to its message.
type Validator interface {
	Validate() error
}

// StrictDecoder builds a DecodeFunc that decodes into a fresh value from newArgs
// (rejecting unknown fields) and returns the canonical re-marshaled args. newArgs
// MUST return a pointer to a zero value of the args struct. This is the standard
// way tool families wire validation without a JSON-schema engine. If the decoded
// value implements Validator, its Validate() runs after decode so numeric/semantic
// bounds (Zod refinements) are enforced — never a panic or an unbounded value.
func StrictDecoder(newArgs func() any) DecodeFunc {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		v := newArgs()
		if err := DecodeStrict(raw, v); err != nil {
			return nil, err
		}
		if val, ok := v.(Validator); ok {
			if err := val.Validate(); err != nil {
				if _, isDecodeErr := err.(*DecodeError); isDecodeErr {
					return nil, err
				}
				return nil, &DecodeError{Message: err.Error(), Issues: []string{err.Error()}}
			}
		}
		canonical, err := json.Marshal(v)
		if err != nil {
			return nil, &DecodeError{Message: fmt.Sprintf("re-encode args: %v", err)}
		}
		return canonical, nil
	}
}
