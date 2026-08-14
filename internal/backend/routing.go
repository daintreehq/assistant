package backend

import (
	"fmt"
	"regexp"
	"strings"
)

// routing.go is the caller's endpoint-routing preference — which OpenRouter endpoint
// answers a request, and how strictly the pool is filtered.
//
// It exists because two of the backend's routing decisions are legitimately the
// CALLER's: the key is theirs and the bill is theirs. The default buys speed at the
// cost of price (`throughput`) and guarantees no-training but not no-retention
// (`no_training`); a cost-sensitive tester may want `price`, and someone handling
// sensitive source may want to pay the availability cost of zero data retention.
//
// It is a CLOSED SET, mirroring the backend's own contract, and validated here as well
// as there. Sending an arbitrary provider block would let a client silently drop the
// no-training floor or pin an endpoint that ignores `response_format` — and validating
// locally turns a typo into a message at boot rather than a 400 in the middle of a turn.
//
// Two properties this deliberately CANNOT express, both by the backend's design:
// privacy can only be tightened (the no-training floor is sent unconditionally and is
// not derived from anything here), and no policy guarantees a route. A strict mode plus
// a narrow allowlist can empty the pool, which fails closed as
// `upstream_no_compliant_provider` rather than quietly relaxing the filter.

// Privacy modes. The wording a user SEES comes from the backend's capabilities
// (`routing.privacy_description`), never from here — see PrivacyDescription.
const (
	// PrivacyNoTraining routes only to endpoints that do not collect or train on
	// request data. It is a no-training guarantee and NOT a no-retention one; an
	// endpoint in this pool may still hold a request transiently.
	PrivacyNoTraining = "no_training"
	// PrivacyZDR adds OpenRouter's zero-data-retention filter on top. It narrows the
	// eligible pool substantially.
	PrivacyZDR = "zdr"
)

// Endpoint sort axes.
const (
	SortThroughput = "throughput" // fastest tokens/second, explicitly at the cost of price
	SortPrice      = "price"      // cheapest wins
	SortLatency    = "latency"    // lowest time-to-first-token
)

// MaxEndpointList caps an allow/deny list, matching the backend's own bound. Generous
// (a model is served by a couple of dozen endpoints) but bounded, because these strings
// are forwarded upstream verbatim.
const MaxEndpointList = 24

// maxEndpointSlugLength and endpointSlugPattern mirror the backend's validator exactly.
const maxEndpointSlugLength = 64

// endpointSlugPattern mirrors the backend's `_ENDPOINT_SLUG` exactly, including the
// requirement that a slug START with an alphanumeric — ".deepinfra" is rejected there and
// must be rejected here, or the failure just moves to a mid-turn 400.
//
// Anchored at both ends. Go's `$` without `(?m)` means end of TEXT, so unlike Python's
// `re.match(..., "deepinfra\n")` this cannot accept a trailing newline; the anchors are
// kept explicit anyway so the pattern reads as the same rule.
var endpointSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Routing is the optional `routing` block on a respond request. The zero value means
// "no preference" and serializes to nothing — the server default applies, which is what
// almost every request wants.
type Routing struct {
	Privacy string   `json:"privacy,omitempty"`
	Sort    string   `json:"sort,omitempty"`
	Only    []string `json:"only,omitempty"`
	Ignore  []string `json:"ignore,omitempty"`
}

// IsZero reports whether this expresses no preference at all, in which case the block is
// omitted from the request rather than sent as an empty object.
func (r Routing) IsZero() bool {
	return r.Privacy == "" && r.Sort == "" && len(r.Only) == 0 && len(r.Ignore) == 0
}

// IsDefault reports whether this policy is what the caller would get by expressing no
// preference. Used to decide whether the posture is worth announcing: an explicit
// setting that happens to match the default is not news.
func (r Routing) IsDefault() bool {
	return (r.Privacy == "" || r.Privacy == PrivacyNoTraining) &&
		(r.Sort == "" || r.Sort == SortThroughput) &&
		len(r.Only) == 0 && len(r.Ignore) == 0
}

// Validate rejects anything the backend would reject, with a message naming the valid
// choices. Running the same closed set on both sides is what turns a mistyped env var
// into a startup error instead of a 400 that lands mid-turn, after the user has already
// typed a message.
func (r Routing) Validate() error {
	switch r.Privacy {
	case "", PrivacyNoTraining, PrivacyZDR:
	default:
		return fmt.Errorf("routing privacy %q is not one of: %s, %s", r.Privacy, PrivacyNoTraining, PrivacyZDR)
	}
	switch r.Sort {
	case "", SortThroughput, SortPrice, SortLatency:
	default:
		return fmt.Errorf("routing sort %q is not one of: %s, %s, %s", r.Sort, SortThroughput, SortPrice, SortLatency)
	}
	if err := validateEndpointList("only", r.Only); err != nil {
		return err
	}
	if err := validateEndpointList("ignore", r.Ignore); err != nil {
		return err
	}
	// An endpoint that is both required and forbidden guarantees an empty pool. The
	// backend rejects it as a contradiction rather than letting it surface as a routing
	// dead end the caller has to reverse-engineer; say the same thing at startup.
	var overlap []string
	inIgnore := make(map[string]bool, len(r.Ignore))
	for _, s := range r.Ignore {
		inIgnore[s] = true
	}
	for _, s := range r.Only {
		if inIgnore[s] {
			overlap = append(overlap, s)
		}
	}
	if len(overlap) > 0 {
		return fmt.Errorf("routing: %s appear in both only and ignore, which can never route",
			strings.Join(overlap, ", "))
	}
	return nil
}

func validateEndpointList(field string, list []string) error {
	if len(list) > MaxEndpointList {
		return fmt.Errorf("routing %s has %d endpoints, over the limit of %d", field, len(list), MaxEndpointList)
	}
	seen := make(map[string]bool, len(list))
	for _, slug := range list {
		if len(slug) < 1 || len(slug) > maxEndpointSlugLength || !endpointSlugPattern.MatchString(slug) {
			return fmt.Errorf("routing %s: %q is not a valid endpoint slug (start with a lowercase letter or digit, then lowercase alphanumerics, '.', '-' and '_', 1-%d characters)",
				field, slug, maxEndpointSlugLength)
		}
		// Duplicates consume the list cap for nothing and produce a distinct request
		// body for a semantically identical policy; the backend rejects them.
		if seen[slug] {
			return fmt.Errorf("routing %s: %q appears more than once", field, slug)
		}
		seen[slug] = true
	}
	return nil
}

// ParseEndpointList splits a comma-separated env value into slugs, dropping empties and
// trimming whitespace so `"deepinfra, together"` works as typed.
func ParseEndpointList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// PrivacyDescription returns the accurate one-line description of a privacy mode,
// preferring the BACKEND's own wording from capabilities.
//
// The fallback exists only for the case where capabilities could not be read; it is
// worded to the same standard the backend holds itself to. The distinction it protects
// is the whole reason the modes are named rather than boolean: no-training and
// no-retention are different promises, and saying "does not store" for `no_training`
// would be a claim about someone's data that is not true.
func PrivacyDescription(mode string, caps *Capabilities) string {
	if caps != nil && caps.Routing.PrivacyDescription != "" &&
		(mode == "" || mode == caps.Routing.PrivacyMode) {
		return caps.Routing.PrivacyDescription
	}
	if mode == PrivacyZDR {
		return "Requests route only to endpoints that enforce zero data retention and do not collect or train on request data."
	}
	return "Requests route only to endpoints that do not collect or train on request data. Zero data retention is not enforced."
}
