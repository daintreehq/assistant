package redact

import (
	"encoding/json"
	"strings"
)

// value.go redacts STRUCTURED data structurally, instead of running regexes over its
// serialized form.
//
// Regexing serialized JSON is how a redactor corrupts the thing it is protecting. Two
// real failures, both from the first cut of this package:
//
//	{"command":"export API_KEY=abc","path":"keep"}
//	  → the env-assignment pattern's value group is "non-whitespace", and minified JSON
//	    has no whitespace, so it swallowed the rest of the document. Output:
//	    {"command":"export API_KEY=[redacted]   — truncated, unparseable.
//
//	{"password":"abc\"def","other":"keep"}
//	  → the keyed-field pattern is not escape-aware, so it treated the ESCAPED quote as
//	    the closing one and produced {"password":"[redacted]"def",...} — also unparseable.
//
// A caller that stores audit rows or exports them to the model cannot use output like
// that. Walking the decoded value instead means the structure is decided by the JSON
// decoder, string values are redacted one at a time with no way to run past their own
// boundary, and re-marshaling always yields valid JSON.
//
// Free text still needs String: a shell command, a terminal excerpt, an error message.
// The rule is simply "use the one that matches the shape you have".

// maxDepth bounds the walk. Tool args are shallow; a pathological or cyclic-looking
// structure must not be able to turn an audit write into a stack overflow, and the
// audit path is a side-channel that must never break a tool call.
const maxDepth = 32

// Value returns v with every string redacted, walking maps and slices.
//
// Keys are examined too: a value under a credential-named key is masked whole, even when
// it matches no shape. That is where structural redaction beats the regex — `{"api_key":
// "correct horse battery staple"}` has no recognisable token shape, and the KEY is the
// only evidence available.
//
// Non-string scalars pass through untouched, EXCEPT under a sensitive key, where a
// number or bool is replaced by the mark as well: a PIN or an account id under
// `"secret"` is still a secret, and preserving its type is worth less than not leaking it.
func Value(v any) any { return redactValue(v, false, 0) }

func redactValue(v any, keyIsSensitive bool, depth int) any {
	if depth > maxDepth {
		return Mark
	}
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if keyIsSensitive {
			return Mark
		}
		return String(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, sub := range t {
			// The KEY itself can carry a secret in a pathological payload (an env dump
			// keyed by its own value), so it is redacted as free text — but never
			// dropped, because a missing key changes the shape of the record.
			out[String(k)] = redactValue(sub, keyIsSensitive || IsSensitiveKey(k), depth+1)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, sub := range t {
			out[i] = redactValue(sub, keyIsSensitive, depth+1)
		}
		return out
	case json.RawMessage:
		// Decode, walk, re-encode. A RawMessage that does not parse is treated as free
		// text — it is not JSON in any useful sense, so the regex path is the honest one.
		var decoded any
		if err := json.Unmarshal(t, &decoded); err != nil {
			return String(string(t))
		}
		encoded, err := json.Marshal(redactValue(decoded, keyIsSensitive, depth+1))
		if err != nil {
			return Mark
		}
		return json.RawMessage(encoded)
	default:
		// Numbers, bools, and anything else the decoder produced. Masked only when the
		// key says so; otherwise they cannot carry a credential and masking them would
		// destroy the durations, counts, and ids that make a record worth keeping.
		if keyIsSensitive {
			return Mark
		}
		return v
	}
}

// JSONBytes redacts a JSON document and returns valid JSON.
//
// Falls back to free-text redaction when the input does not parse, so a caller never has
// to choose between "might corrupt it" and "might not redact it".
func JSONBytes(raw []byte) []byte {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return []byte(String(string(raw)))
	}
	out, err := json.Marshal(Value(decoded))
	if err != nil {
		return []byte(`"` + Mark + `"`)
	}
	return out
}

// sensitiveKeyParts are the substrings that make a field name credential-bearing.
//
// Substring matching is deliberate — "client_secret", "x-api-key" and "sessionToken" all
// have to match — but it is why `tokenCount` and `signatureAlgorithm` were being masked:
// they contain a marker and are ordinary data. IsSensitiveKey therefore excludes the
// known-benign compounds explicitly rather than weakening the markers, because losing
// "sessionToken" to protect "tokenCount" would be the wrong trade.
var sensitiveKeyParts = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key", "api-key",
	"access_key", "access-key", "accesskey", "private_key", "private-key", "privatekey",
	"authorization", "bearer", "credential", "client_secret", "client-secret",
	"cookie", "session_id", "sessionid", "sessiontoken", "signature",
}

// benignKeyCompounds are field names that contain a sensitive marker but describe
// METADATA about a credential rather than the credential itself. Masking these hides
// exactly the numbers a reader opened the record to see.
var benignKeyCompounds = []string{
	"tokencount", "tokens", "tokenlimit", "maxtokens", "prompttokens", "completiontokens",
	"cachedtokens", "tokenusage", "signaturealgorithm", "secretpath", "secretname",
	"secretref", "credentialpath", "cookiepreferences", "hastoken", "tokenpresent",
	"tokenlength", "keyredacted",
}

// IsSensitiveKey reports whether a field name marks its value as a credential.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.NewReplacer("-", "", "_", "", " ", "").Replace(k)
	for _, benign := range benignKeyCompounds {
		if k == strings.NewReplacer("-", "", "_", "", " ", "").Replace(benign) {
			return false
		}
	}
	for _, part := range sensitiveKeyParts {
		if strings.Contains(k, strings.NewReplacer("-", "", "_", "", " ", "").Replace(part)) {
			return true
		}
	}
	return false
}
