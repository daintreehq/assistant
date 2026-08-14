package redact

import (
	"encoding/json"
	"fmt"
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
			// keyed by its own value), so it is redacted as free text — but never dropped,
			// because a missing key changes the shape of the record.
			//
			// Redacted keys are made UNIQUE. Two different secret-bearing keys both
			// collapsing to "[redacted]" would silently overwrite each other, and the
			// record would lose entries — a redactor that deletes data is a bug, not a
			// stricter redactor.
			rk := String(k)
			if rk != k {
				if _, clash := out[rk]; clash {
					rk = fmt.Sprintf("%s-%d", rk, len(out))
				}
			}
			out[rk] = redactValue(sub, keyIsSensitive || IsSensitiveKey(k), depth+1)
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
	case bool, float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		// Scalars cannot carry a credential, and masking them would destroy the
		// durations, counts, and ids that make a record worth keeping — unless the key
		// says otherwise, where a numeric PIN is still a secret.
		if keyIsSensitive {
			return Mark
		}
		return v
	default:
		// A STRUCT, a pointer, or any other Go value a caller passed directly.
		//
		// These used to fall through untouched, which was a silent hole: the support
		// bundle marshals a *DoctorReport, so every string inside it — including nested
		// maps — bypassed redaction entirely while the code appeared to redact. Round-trip
		// through JSON to get the map/slice form, then walk that. Slower than the direct
		// cases, but this is the cold path, and "we redact except for types we did not
		// anticipate" is not a property worth having.
		raw, err := json.Marshal(v)
		if err != nil {
			// Genuinely unmarshalable (a func, a channel, a cycle). Emit a typed
			// placeholder rather than %v: the default rendering of those is a pointer
			// ADDRESS, which tells a reader nothing and quietly discloses a memory layout
			// detail. Naming the type is the useful half.
			return fmt.Sprintf("<unserializable %T>", v)
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return String(string(raw))
		}
		return redactValue(decoded, keyIsSensitive, depth+1)
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

// metadataSuffixes mark a credential-named field as describing a credential rather than
// being one.
//
// A suffix rule rather than a list of exact names — `apiKeyPresent`, `mcpTokenLength`,
// `tokenCount` and every future variant follow one shape, and enumerating them
// individually guarantees the next one is missed. `apiKeyLength` in particular must
// survive: it is what tells support a key was pasted truncated.
//
// The list is deliberately CONSERVATIVE, because every entry is a hole. Two that were
// here and had to go:
//
//	"bytes"  — `privateKeyBytes` and `secretBytes` hold the actual secret, not its size.
//	"tokens" — `accessTokens` and `refreshTokens` are a LIST OF CREDENTIALS. Only the
//	           specific token-count names below are metadata (see tokenCountKeys).
//
// The remaining entries describe a quantity, a location, or a label — never a value.
var metadataSuffixes = []string{
	"present", "length", "count", "counts", "size", "path", "name", "names",
	"algorithm", "redacted", "expiry", "expiresat", "usage", "limit", "ref",
	"preferences", "enabled", "required", "type", "kind", "source", "version",
	// A credential-shaped name ending in "id" is a REFERENCE to a credential, not the
	// credential: sessionId, apiKeyId, credentialId. The thing that authenticates is the
	// cookie or the session TOKEN, both of which are caught by their own markers.
	"id",
}

// tokenCountKeys are the exact plural-"tokens" names that mean a COUNT. Spelled out
// rather than matched by suffix, because `accessTokens` and `refreshTokens` share that
// suffix and are the credentials themselves.
var tokenCountKeys = map[string]bool{
	"tokens": true, "prompttokens": true, "completiontokens": true, "cachedtokens": true,
	"totaltokens": true, "maxtokens": true, "inputtokens": true, "outputtokens": true,
	"reasoningtokens": true, "tokencount": true, "tokencounts": true,
}

// metadataPrefixes mark a field as a PREDICATE about a credential (`hasToken`,
// `isSecret`, `numApiKeys`).
//
// The prefix must be followed by a credential marker, not merely present at the start.
// Without that, `hashedPassword` was exempted because it happens to begin with the
// letters "has" — the single most dangerous false exemption available, since a password
// hash is exactly what an attacker wants.
var metadataPrefixes = []string{"has", "is", "max", "min", "num", "total", "count"}

// IsSensitiveKey reports whether a field name marks its value as a credential.
func IsSensitiveKey(key string) bool {
	k := normalizeKey(key)
	if k == "" {
		return false
	}
	marker := ""
	for _, part := range sensitiveKeyParts {
		if p := normalizeKey(part); strings.Contains(k, p) {
			marker = p
			break
		}
	}
	if marker == "" {
		return false
	}
	if tokenCountKeys[k] {
		return false
	}
	for _, suffix := range metadataSuffixes {
		if strings.HasSuffix(k, suffix) {
			return false
		}
	}
	// A predicate prefix counts ONLY when the marker starts immediately after it, so
	// "has"+"token" exempts hasToken while "has"+"hedpassword" leaves hashedPassword
	// masked.
	for _, prefix := range metadataPrefixes {
		if rest := strings.TrimPrefix(k, prefix); rest != k && strings.HasPrefix(rest, marker) {
			return false
		}
	}
	return true
}

// normalizeKey lowercases and strips separators so apiKey, api_key and API-KEY compare
// equal.
func normalizeKey(s string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}
