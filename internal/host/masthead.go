package host

import (
	"strings"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
)

// masthead.go derives the session facts an embedding host puts in its own masthead.
//
// These are computed HERE, in the engine, rather than shipped as raw config for the
// host to interpret. Every one of them is a policy judgement that depends on constants
// this module owns — which backend URL counts as "the deployed one", what the local
// endpoint is called, which routing policy is the default, what each tier actually
// permits. A host that re-derived them would need a second copy of all of that, and the
// copy would be wrong the first time any of it changed. The wire carries the finished
// string; the host decides only how to draw it.
//
// The rules are ported verbatim from the cockpit's own masthead (internal/ui,
// render_chrome.go) so an embedded surface states exactly what the terminal UI stated.

// tierGloss is the plain-language reading of a tier, or "" for an unknown one.
func tierGloss(t domain.Tier) string {
	switch t {
	case domain.TierSupervisor:
		return "read & UI only"
	case domain.TierOperator:
		return "terminals, projects, external"
	case domain.TierSystem:
		return "full access (git, system)"
	default:
		return ""
	}
}

// mastheadBackend names a NON-DEFAULT backend endpoint, or "" for the deployed one.
//
// Only a deviation is announced: the deployed backend is what every install talks to
// and needs no statement. The masthead carries no other endpoint readout
// of which backend answered a turn, so an endpoint exported in another terminal is
// otherwise completely invisible — and a transcript that does not say it was talking to
// a local backend is a transcript that cannot be trusted.
func mastheadBackend(baseURL string) string {
	u := strings.TrimSpace(baseURL)
	if u == "" || u == backend.DefaultBaseURL {
		return ""
	}
	safe := mcp.SanitizeURL(u)
	if safe == "" {
		return ""
	}
	if safe == backend.LocalBaseURL {
		// Name the thing rather than the address. "local" is what the operator calls it,
		// and it reads at a glance in a way a host:port never does.
		return "local (" + safe + ")"
	}
	return safe
}

// mastheadRouting renders a NON-DEFAULT routing policy as one compact line, or "" when
// the policy is the server default.
func mastheadRouting(r backend.Routing) string {
	if r.IsDefault() {
		return ""
	}
	var parts []string
	if r.Privacy == backend.PrivacyZDR {
		parts = append(parts, "zero data retention")
	}
	switch r.Sort {
	case backend.SortPrice:
		parts = append(parts, "cheapest endpoint")
	case backend.SortLatency:
		parts = append(parts, "lowest latency")
	}
	if n := len(r.Only); n > 0 {
		parts = append(parts, "only "+strings.Join(r.Only, ", "))
	}
	if n := len(r.Ignore); n > 0 {
		parts = append(parts, "excluding "+strings.Join(r.Ignore, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	// "requested" is the honest word. We cannot observe the upstream request, so this
	// cannot claim a mode is IN FORCE — only that this session asked for it.
	return "requested " + strings.Join(parts, " · ") + "  (/routing)"
}
