package asyncwork

import "encoding/json"

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
