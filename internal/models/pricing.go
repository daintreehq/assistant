package models

import "strings"

// Static, dependency-free pricing for the Fireworks models the router maps to,
// used to turn per-turn token counts into a rough running session cost. Rates are
// USD per million tokens, current for Fireworks serverless ~2026-06; they drift and
// are intentionally approximate. An unknown model returns (0,false) so the UI can
// distinguish "no rate" from a genuine $0.000.

type modelRate struct {
	inputPerM  float64
	outputPerM float64
}

// rates is keyed by model-id PREFIX so versioned ids (e.g. deepseek-v3-0324) match
// their base family. Longest matching prefix wins.
var rates = []struct {
	prefix string
	rate   modelRate
}{
	{"glm-5p2", modelRate{inputPerM: 0.55, outputPerM: 2.19}},
	{"minimax-m3", modelRate{inputPerM: 0.3, outputPerM: 1.2}},
	{"deepseek-v4", modelRate{inputPerM: 0.56, outputPerM: 1.68}},
	{"deepseek-v3", modelRate{inputPerM: 0.56, outputPerM: 1.68}},
}

// cachedInputDiscount: cached prompt tokens bill at half the input rate.
const cachedInputDiscount = 0.5

// BareModelID strips any accounts/<x>/models/<id> Fireworks path: the substring
// after the last '/', else the id as-is.
func BareModelID(model string) string {
	if i := strings.LastIndex(model, "/"); i != -1 {
		return model[i+1:]
	}
	return model
}

// rateFor returns the longest-matching-prefix rate for a model (lowercased, bare),
// or (zero,false) when none matches.
func rateFor(model string) (modelRate, bool) {
	bare := BareModelID(strings.ToLower(model))
	var best modelRate
	bestLen := -1
	for _, r := range rates {
		if strings.HasPrefix(bare, r.prefix) && len(r.prefix) > bestLen {
			best = r.rate
			bestLen = len(r.prefix)
		}
	}
	if bestLen == -1 {
		return modelRate{}, false
	}
	return best, true
}

// EstimateCostUsd estimates the USD cost of one model call. cachedTokens (a subset
// of promptTokens) is billed at the cache discount. Returns (0,false) for a model
// with no known rate so callers can show "$?" rather than a misleading $0.000.
func EstimateCostUsd(model string, promptTokens, completionTokens, cachedTokens int) (float64, bool) {
	rate, ok := rateFor(model)
	if !ok {
		return 0, false
	}
	cached := cachedTokens
	if cached < 0 {
		cached = 0
	}
	if cached > promptTokens {
		cached = promptTokens
	}
	freshInput := promptTokens - cached
	inputCost := (float64(freshInput)*rate.inputPerM +
		float64(cached)*rate.inputPerM*cachedInputDiscount) / 1_000_000
	outputCost := float64(completionTokens) * rate.outputPerM / 1_000_000
	return inputCost + outputCost, true
}
