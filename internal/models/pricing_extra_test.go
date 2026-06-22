package models

import (
	"math"
	"testing"
)

func closeEnough(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// estimateCostUsd prices the large model at its input/output rates, matches
// versioned ids by prefix, strips a Fireworks account path, half-discounts cached
// prompt tokens (clamped to the prompt total), and returns ok=false for unknowns.
func TestEstimateCostUsdMatrix(t *testing.T) {
	cases := []struct {
		name                       string
		model                      string
		prompt, completion, cached int
		want                       float64
		tol                        float64
		wantOK                     bool
	}{
		{
			name:  "minimax-m3 input+output rates",
			model: "minimax-m3", prompt: 1_000_000, completion: 1_000_000,
			want: 1.5, tol: 1e-6, wantOK: true, // 1M*0.30 + 1M*1.20
		},
		{
			name:  "versioned id matches by prefix",
			model: "deepseek-v3-0324", prompt: 1_000_000, completion: 0,
			want: 0.56, tol: 1e-6, wantOK: true,
		},
		{
			name:  "strips fireworks account path",
			model: "accounts/fireworks/models/minimax-m3", prompt: 1_000_000, completion: 0,
			want: 0.3, tol: 1e-6, wantOK: true,
		},
		{
			name:  "cached prompt tokens half-discounted",
			model: "minimax-m3", prompt: 1_000_000, completion: 0, cached: 1_000_000,
			want: 0.15, tol: 1e-6, wantOK: true, // 1M*0.30*0.5
		},
		{
			name:  "cached clamped to prompt total (no negative fresh cost)",
			model: "minimax-m3", prompt: 100, completion: 0, cached: 1_000,
			want: (100 * 0.3 * 0.5) / 1_000_000, tol: 1e-9, wantOK: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := EstimateCostUsd(c.model, c.prompt, c.completion, c.cached)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !closeEnough(got, c.want, c.tol) {
				t.Fatalf("cost = %v, want %v (±%v)", got, c.want, c.tol)
			}
		})
	}

	// An unknown model returns ok=false so the UI can show "$?" rather than $0.000.
	if _, ok := EstimateCostUsd("some-unknown-model", 1000, 1000, 0); ok {
		t.Fatal("unknown model must return ok=false")
	}
}

// BareModelID strips a Fireworks account path and leaves a bare id unchanged.
func TestBareModelIDExtra(t *testing.T) {
	if got := BareModelID("accounts/fireworks/models/minimax-m3"); got != "minimax-m3" {
		t.Fatalf("bare = %q", got)
	}
	if got := BareModelID("deepseek-v4-flash"); got != "deepseek-v4-flash" {
		t.Fatalf("bare = %q", got)
	}
}

// TestEstimateCostUsdClampsNegative locks the clamp: malformed/negative token counts
// must never produce a negative cost (which would make the running session total drop).
func TestEstimateCostUsdClampsNegative(t *testing.T) {
	cost, ok := EstimateCostUsd("minimax-m3", -100, -50, -10)
	if !ok {
		t.Skip("minimax-m3 not rated in this build")
	}
	if cost < 0 {
		t.Errorf("negative inputs produced a negative cost: %v", cost)
	}
}
