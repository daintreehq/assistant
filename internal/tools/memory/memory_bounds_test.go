package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

// Finding 6: recall query / saved content / category / id were unbounded, letting
// pathological input bloat the FTS index or drive an expensive MATCH. Validate()
// must reject oversized input at decode time as INVALID_ARGS while leaving normal
// input untouched.
func TestMemoryRejectsOversizedInput(t *testing.T) {
	tools := Tools(Deps{Store: &memStore{}})
	recall := find(tools, "memory.recall")
	save := find(tools, "memory.save")
	list := find(tools, "memory.list")
	forget := find(tools, "memory.forget")

	bigQuery := strings.Repeat("a", maxQueryRunes+1)
	bigContent := strings.Repeat("b", maxContentRunes+1)
	bigCategory := strings.Repeat("c", maxCategoryRunes+1)
	bigID := strings.Repeat("d", maxIDRunes+1)

	cases := []struct {
		name string
		tool string
		args string
	}{
		{"oversized recall query", "recall", `{"query":"` + bigQuery + `"}`},
		{"oversized recall category", "recall", `{"query":"q","category":"` + bigCategory + `"}`},
		{"oversized save content", "save", `{"content":"` + bigContent + `"}`},
		{"oversized save category", "save", `{"content":"ok","category":"` + bigCategory + `"}`},
		{"oversized list category", "list", `{"category":"` + bigCategory + `"}`},
		{"oversized forget id", "forget", `{"id":"` + bigID + `"}`},
	}
	for _, c := range cases {
		var tool = recall
		switch c.tool {
		case "save":
			tool = save
		case "list":
			tool = list
		case "forget":
			tool = forget
		}
		if _, err := tool.Decode(json.RawMessage(c.args)); err == nil {
			t.Errorf("%s should be rejected", c.name)
		}
	}

	// Normal-sized input still decodes.
	if _, err := recall.Decode(json.RawMessage(`{"query":"recent fixes","category":"fix"}`)); err != nil {
		t.Errorf("normal recall should decode: %v", err)
	}
	if _, err := save.Decode(json.RawMessage(`{"content":"remember this","category":"decision"}`)); err != nil {
		t.Errorf("normal save should decode: %v", err)
	}
}
