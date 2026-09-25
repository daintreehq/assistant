package backend

import (
	"fmt"
	"net/http"
	"strings"
)

// Upstream is the caller's own model host, model and API key — bring your own key.
// Daintree collects it in its assistant settings and hands it to this process through
// the environment; the client forwards it on every backend call, and the backend runs
// the turn's model calls on it instead of on its own key. The zero value means "let
// the backend fund the turn".
type Upstream struct {
	Provider string
	Model    string
	APIKey   string
	// OpenRouter only — its own endpoint-routing preferences, each empty for "no
	// preference": Sort ranks the endpoints (latency, price, throughput),
	// DataCollection "deny" skips endpoints that collect or train on prompts, and
	// ZDR "true" keeps to endpoints OpenRouter lists as zero data retention.
	Sort           string
	DataCollection string
	ZDR            string
}

// UpstreamProviders are the hosts the backend can route a caller's key to. Kept in
// step with the backend's providers/upstream.py and Daintree's provider list.
var UpstreamProviders = []string{"baseten", "openrouter", "openai"}

// The backend's refusals of the upstream headers themselves: none sent to a deployment
// that serves only caller-funded turns, or a malformed set.
const (
	CodeUpstreamRequired = "upstream_required"
	CodeInvalidUpstream  = "invalid_upstream"
)

const (
	upstreamProviderHeader = "X-Daintree-Upstream-Provider"
	upstreamModelHeader    = "X-Daintree-Upstream-Model"
	upstreamKeyHeader      = "X-Daintree-Upstream-Key"
	upstreamSortHeader     = "X-Daintree-Upstream-Sort"
	upstreamDataHeader     = "X-Daintree-Upstream-Data-Collection"
	upstreamZDRHeader      = "X-Daintree-Upstream-Zdr"
)

// IsSet reports whether any part of an upstream was configured.
func (u Upstream) IsSet() bool {
	return u.Provider != "" || u.Model != "" || u.APIKey != "" ||
		u.Sort != "" || u.DataCollection != "" || u.ZDR != ""
}

// Validate refuses a half-configured upstream at startup, naming the variable, rather
// than letting it become a 400 on the first turn. An empty Upstream is valid.
func (u Upstream) Validate() error {
	if !u.IsSet() {
		return nil
	}
	known := false
	for _, p := range UpstreamProviders {
		if u.Provider == p {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("DAINTREE_UPSTREAM_PROVIDER must be one of %s (got %q)",
			strings.Join(UpstreamProviders, ", "), u.Provider)
	}
	if err := ValidateKeyShape(u.APIKey); err != nil {
		return fmt.Errorf("DAINTREE_UPSTREAM_API_KEY: %w", err)
	}
	if strings.ContainsAny(u.Model, " \t\r\n") {
		return fmt.Errorf("DAINTREE_UPSTREAM_MODEL contains whitespace")
	}
	if u.Sort == "" && u.DataCollection == "" && u.ZDR == "" {
		return nil
	}
	if u.Provider != "openrouter" {
		return fmt.Errorf("DAINTREE_UPSTREAM_SORT / _DATA_COLLECTION / _ZDR apply to openrouter only")
	}
	if !oneOf(u.Sort, "", "latency", "price", "throughput") {
		return fmt.Errorf("DAINTREE_UPSTREAM_SORT must be latency, price or throughput (got %q)", u.Sort)
	}
	if !oneOf(u.DataCollection, "", "allow", "deny") {
		return fmt.Errorf("DAINTREE_UPSTREAM_DATA_COLLECTION must be allow or deny (got %q)", u.DataCollection)
	}
	if !oneOf(u.ZDR, "", "true", "false") {
		return fmt.Errorf("DAINTREE_UPSTREAM_ZDR must be true or false (got %q)", u.ZDR)
	}
	return nil
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func (u Upstream) apply(h http.Header) {
	if !u.IsSet() {
		return
	}
	h.Set(upstreamProviderHeader, u.Provider)
	h.Set(upstreamKeyHeader, u.APIKey)
	if u.Model != "" {
		h.Set(upstreamModelHeader, u.Model)
	}
	for header, value := range map[string]string{
		upstreamSortHeader: u.Sort,
		upstreamDataHeader: u.DataCollection,
		upstreamZDRHeader:  u.ZDR,
	} {
		if value != "" {
			h.Set(header, value)
		}
	}
}

// IsCallerUpstreamKey reports a provider-account error about the CALLER's own key
// rather than the deployment's. The backend marks those by naming the key header in
// `param`, and its message then speaks to the caller; the code is unchanged, so every
// branch that classifies by code still does.
func (e *Error) IsCallerUpstreamKey() bool {
	return e != nil && e.IsProviderAccount() && e.Param == upstreamKeyHeader
}

// IsCallerUpstreamModel reports a failure the backend attributes to the model the user
// chose — one their provider does not serve, or refused a request for. Its message names
// the model and says where to change it.
func (e *Error) IsCallerUpstreamModel() bool {
	return e != nil && e.Param == upstreamModelHeader
}
