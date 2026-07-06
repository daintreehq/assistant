package extractionx

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestParallelizableOptInBoundary locks the concurrency opt-in at its source. Only the
// independent per-call snapshot reads (terminal.extract / .json) are Parallelizable;
// the BARRIER read terminal.awaitAll is deliberately NOT — parallelizing an awaitAll
// with a following extract would let the extract read the tail BEFORE the cohort
// settled, the exact supervision regression that motivated an explicit opt-in over a
// blanket "read risk ⇒ concurrent" rule. Every parallelizable tool must also be
// RiskRead so the runner's double-gate (ParallelSafe) can't be defeated.
func TestParallelizableOptInBoundary(t *testing.T) {
	extract := newExtractTool(Deps{})
	extractJSON := newExtractJSONTool(Deps{})
	awaitAll := newAwaitAllTool(Deps{})

	if !extract.Parallelizable {
		t.Error("terminal.extract must be Parallelizable (independent per-call read)")
	}
	if !extractJSON.Parallelizable {
		t.Error("terminal.extract.json must be Parallelizable (independent per-call read)")
	}
	if awaitAll.Parallelizable {
		t.Error("terminal.awaitAll must NOT be Parallelizable — it is a barrier; " +
			"parallelizing it with a following read breaks cohort supervision")
	}

	// Double-gate invariant: anything opted in must be read-risk, or the runner's
	// RiskRead gate would (correctly) refuse to parallelize it.
	for _, tl := range []struct {
		name string
		risk domain.RiskClass
		par  bool
	}{
		{extract.Name, extract.Risk, extract.Parallelizable},
		{extractJSON.Name, extractJSON.Risk, extractJSON.Parallelizable},
	} {
		if tl.par && tl.risk != domain.RiskRead {
			t.Errorf("%s is Parallelizable but risk=%s, want read (double-gate would reject it)", tl.name, tl.risk)
		}
	}
}
