package asyncwork

import (
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// mustJSON serializes the per-terminal outcome ledger for a storage patch. The
// input is always a map of small plain structs, so a marshal error is a
// programming bug — degrade to "{}" rather than panic the tick (supervision
// must survive anything).
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// parseJSONIDs decodes a persisted terminal-id list; nil on any malformation
// (an adopted row we can't parse is handled by the caller, never a panic).
func parseJSONIDs(raw string) []string {
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	out := ids[:0]
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// parseOutcomes decodes a persisted per-terminal outcome ledger; empty map on
// absence or malformation (the poll simply re-derives the outcomes).
func parseOutcomes(raw *string) map[string]domain.AsyncTerminalOutcome {
	if raw == nil || *raw == "" {
		return nil
	}
	var out map[string]domain.AsyncTerminalOutcome
	if err := json.Unmarshal([]byte(*raw), &out); err != nil {
		return nil
	}
	return out
}
