package extractionx

import (
	"context"
	"testing"
)

type budgetRouter struct {
	routeRouter
	budget int
}

func (r *budgetRouter) ExtractText(_ context.Context, _ string, _ []string, _ string, budget int) (string, bool, error) {
	r.budget = budget
	return "ownership confirmed", false, nil
}

func (r *budgetRouter) ExtractJSON(_ context.Context, _ string, _ []string, _ string, _ map[string]any, budget int) (any, error) {
	r.budget = budget
	return map[string]any{"owner": true}, nil
}

func TestExtractionUsesResolvedOutputBudget(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, budget := range []int{1, 1024, 2000} {
			r := &budgetRouter{}
			base, errMsg := resolveBase(baseArgs{TerminalIDs: []string{"t1", "t2"}, MaxTokens: &budget})
			if errMsg != "" {
				t.Fatal(errMsg)
			}
			_, err := runExtract(context.Background(), Deps{Router: r}, base.core("ownership", format, "{}"), "drafts")
			if err != nil {
				t.Fatal(err)
			}
			if r.budget != budget {
				t.Fatalf("%s budget=%d, want %d", format, r.budget, budget)
			}
		}
	}
}
