package docsx

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// TestDocsReadsParallelizable locks the concurrency opt-in for the documentation
// family. docs.search/getPage/getRelatedPages are independent reads over the docs MCP
// with no ordering dependency on their batch siblings, so a research burst (several
// searches, or reading several pages at once) must overlap its network round-trips
// instead of serializing — the same win as terminal.extract. Every parallelizable tool
// must also be RiskRead, or the runner's double-gate (ParallelSafe) would refuse it.
func TestDocsReadsParallelizable(t *testing.T) {
	cases := []struct {
		label string
		tool  *tools.Tool
	}{
		{"docs.search", newSearchTool(Deps{})},
		{"docs.getPage", newGetPageTool(Deps{})},
		{"docs.getRelatedPages", newGetRelatedPagesTool(Deps{})},
	}
	for _, c := range cases {
		if !c.tool.Parallelizable {
			t.Errorf("%s must be Parallelizable (independent docs read; batched research overlaps)", c.label)
		}
		if c.tool.Risk != domain.RiskRead {
			t.Errorf("%s is Parallelizable but risk=%s, want read (double-gate would reject it)", c.label, c.tool.Risk)
		}
	}
}
