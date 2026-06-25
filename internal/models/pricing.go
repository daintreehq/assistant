package models

import "strings"

// Static, dependency-free pricing for the DeepSeek models the router maps to, used to
// turn per-turn token counts into a rough running session cost. Rates are USD per
// million tokens from DeepSeek's published rate card (~2026-06): inputPerM is the
// cache-MISS input price, cachedPerM the cache-HIT input price, outputPerM the output
// price. They drift, so treat them as approximate. An unknown model returns (0,false)
// so the UI can distinguish "no rate" from a genuine $0.000.

type modelRate struct {
	inputPerM  float64
	cachedPerM float64
	outputPerM float64
}

// rates is keyed by model-id PREFIX so versioned ids match their base family.
// Longest matching prefix wins — put more-specific prefixes before shorter ones.
var rates = []struct {
	prefix string
	rate   modelRate
}{
	{"glm-5p2", modelRate{inputPerM: 1.40, cachedPerM: 0.26, outputPerM: 4.40}},
	{"deepseek-v4-flash", modelRate{inputPerM: 0.14, cachedPerM: 0.0028, outputPerM: 0.28}},
	{"deepseek-v4-pro", modelRate{inputPerM: 0.435, cachedPerM: 0.003625, outputPerM: 0.87}},
	{"deepseek-v4", modelRate{inputPerM: 0.14, cachedPerM: 0.0028, outputPerM: 0.28}}, // Flash fallback
	{"deepseek-v3", modelRate{inputPerM: 0.56, cachedPerM: 0.11, outputPerM: 1.68}},
	{"minimax-m3", modelRate{inputPerM: 0.30, cachedPerM: 0.06, outputPerM: 1.20}},
	{"minimax-m2", modelRate{inputPerM: 0.30, cachedPerM: 0.06, outputPerM: 1.20}},
}

// BareModelID strips any accounts/<x>/models/<id> DeepSeek path: the substring
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
// of promptTokens) bill at the model's cached rate. Returns (0,false) for a model
// with no known rate so callers can show "$?" rather than a misleading $0.000.
func EstimateCostUsd(model string, promptTokens, completionTokens, cachedTokens int) (float64, bool) {
	rate, ok := rateFor(model)
	if !ok {
		return 0, false
	}
	// Clamp all counts to >= 0: a malformed/negative usage report must never produce a
	// negative cost that makes the running session total go DOWN.
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
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
		float64(cached)*rate.cachedPerM) / 1_000_000
	outputCost := float64(completionTokens) * rate.outputPerM / 1_000_000
	return inputCost + outputCost, true
}
