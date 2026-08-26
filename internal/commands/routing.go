package commands

import (
	"context"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
)

// routing.go renders `/routing`: which OpenRouter endpoints may serve this session, and
// how they are ranked.
//
// Two of these decisions are legitimately the caller's, for different reasons. The
// privacy filter is theirs because it governs where their own source code may go.
// Ranking is theirs because it trades the latency they sit through against the cost the
// deployment bears upstream, and only they know which one this session is worth. This
// panel exists because a policy set in another terminal, possibly days ago, is otherwise
// invisible, and the privacy posture is part of the trust decision someone makes before
// sending their source code anywhere.
//
// The privacy wording is SERVED, never composed here. OpenRouter models "does not
// collect or train on" and "does not retain" as two separate filters, and the default
// mode gives only the first. A client that invented its own summary would eventually
// write "does not store", which is a claim about someone's data that is not true.

// routingProbeTimeout bounds the capabilities read. The panel is useful without it — the
// configured policy is local — so a slow backend degrades to "we could not confirm what
// the server is doing" rather than making the user wait.
const routingProbeTimeout = 3 * time.Second

func routingText(ctx context.Context, a *app.App) string {
	cfg := a.SnapshotConfig()
	local := cfg.Routing

	// One-shot: a diagnostic panel must answer now, not spend the patient turn budget
	// replaying a refused socket to reach the same verdict.
	cctx, cancel := context.WithTimeout(backend.WithoutRetry(ctx), routingProbeTimeout)
	// Through the App, so a successful probe also refreshes the cached descriptor the
	// per-turn capability gates read (see App.BackendCapabilities).
	caps, capsErr := a.BackendCapabilities(cctx)
	cancel()

	var b strings.Builder
	b.WriteString("Which OpenRouter endpoints may serve this session, and how they rank.\n\n")

	// CONFIRMED vs REQUESTED is the distinction this whole panel turns on, and it is
	// stated before any number. A privacy mode we asked for is not a privacy mode that
	// is in force: we cannot observe the upstream request, so the strongest honest claim
	// about a non-default policy is that the backend accepts the field at all. Presenting
	// a request as a guarantee would be the one failure that matters here — someone
	// sending sensitive source under a promise nobody made.
	supported := capsErr == nil && caps.Routing.ClientSelectable != nil
	switch {
	case capsErr != nil:
		b.WriteString("! Could not reach the backend, so nothing below is confirmed — it is this\n" +
			"  CLI's configuration, not a verified server state.\n\n")
	case !supported:
		b.WriteString("! This backend does not accept a routing preference. Anything configured here\n" +
			"  is NOT in force; the server's own default applies. See /doctor.\n\n")
	}

	// The ACTIVE posture, as far as anything can be known. The server's advertised mode
	// is the authority when it is reachable and not accepting a preference.
	effectivePrivacy := local.Privacy
	effectiveSort := local.Sort
	if !supported {
		effectivePrivacy = capsPrivacy(caps, capsErr)
		effectiveSort = capsSort(caps, capsErr)
	}
	effectivePrivacy = firstNonEmpty(effectivePrivacy, capsPrivacy(caps, capsErr), backend.PrivacyNoTraining)
	effectiveSort = firstNonEmpty(effectiveSort, capsSort(caps, capsErr), backend.SortThroughput)

	b.WriteString(padRight("privacy", 10) + ": " + effectivePrivacy + routingSource(local.Privacy, supported) + "\n")
	// The served description, verbatim — and only when the server is describing the mode
	// actually shown. Composing our own for a mode the server has not described is how a
	// client ends up writing "does not store" for a filter that only prevents training.
	var capsPtr *backend.Capabilities
	if capsErr == nil {
		capsPtr = &caps
	}
	if desc := servedPrivacyDescription(effectivePrivacy, capsPtr); desc != "" {
		b.WriteString("            " + desc + "\n")
	} else {
		b.WriteString("            " + backend.PrivacyDescription(effectivePrivacy, nil) +
			"\n            (this CLI's wording — the backend did not describe this mode)\n")
	}
	b.WriteString(padRight("ranking", 10) + ": " + effectiveSort + " " + sortGloss(effectiveSort) + routingSource(local.Sort, supported) + "\n")
	if len(local.Only) > 0 {
		b.WriteString(padRight("only", 10) + ": " + strings.Join(local.Only, ", ") + routingSource("set", supported) + "\n")
	}
	if len(local.Ignore) > 0 {
		b.WriteString(padRight("excluding", 10) + ": " + strings.Join(local.Ignore, ", ") + routingSource("set", supported) + "\n")
	}

	b.WriteString("\n" + routingHowTo)
	if !local.IsDefault() {
		// A stricter policy can match NO endpoint. The backend fails closed rather than
		// quietly relaxing the filter, so the user needs to recognise that failure as
		// theirs to fix — see backend.CodeUpstreamNoCompliantProvider.
		b.WriteString("\n\nA stricter policy can match no endpoint at all. That fails closed — the turn\n" +
			"reports that nothing matched, and the privacy filter is never relaxed to find\n" +
			"a route. Widen the policy if you hit it.")
	}
	return b.String()
}

// servedPrivacyDescription returns the backend's own wording for `mode`, or "" when the
// backend is describing a DIFFERENT mode. Reusing a no_training sentence to describe zdr
// would attach the wrong guarantee to the stricter choice — quietly understating it — and
// the reverse would overstate the default, which is worse.
func servedPrivacyDescription(mode string, caps *backend.Capabilities) string {
	if caps == nil || caps.Routing.PrivacyDescription == "" || caps.Routing.PrivacyMode != mode {
		return ""
	}
	return caps.Routing.PrivacyDescription
}

// routingSource marks where a value came from and whether it is actually in effect. A
// user debugging "why did my setting do nothing" needs those to be different words.
func routingSource(configured string, supported bool) string {
	if strings.TrimSpace(configured) == "" {
		return "  (server default)"
	}
	if !supported {
		return "  (configured here — NOT in force)"
	}
	return "  (requested by this CLI)"
}

// routingHowTo documents the settings. Trusted env only, and the panel says so: a user
// who tries to set these in their project's .env deserves to learn why it did nothing
// rather than debug a policy that never took effect.
const routingHowTo = "Set with environment variables (your own shell only — a project .env cannot\n" +
	"set these, since they decide where your source is sent):\n" +
	"  DAINTREE_ROUTING_PRIVACY   no_training (default) | zdr\n" +
	"  DAINTREE_ROUTING_SORT      throughput (default) | price | latency\n" +
	"  DAINTREE_ROUTING_ONLY      comma-separated endpoint slugs to allow\n" +
	"  DAINTREE_ROUTING_IGNORE    comma-separated endpoint slugs to exclude"

// sortGloss says what a ranking axis costs, since each one is a trade rather than a
// strict improvement.
func sortGloss(sort string) string {
	switch sort {
	case backend.SortPrice:
		return "(cheapest endpoint wins, at the cost of speed)"
	case backend.SortLatency:
		return "(lowest time-to-first-token wins)"
	default:
		return "(fastest endpoint wins, at the cost of price)"
	}
}

func capsPrivacy(caps backend.Capabilities, err error) string {
	if err != nil {
		return ""
	}
	return caps.Routing.PrivacyMode
}

func capsSort(caps backend.Capabilities, err error) string {
	if err != nil {
		return ""
	}
	return caps.Routing.Sort
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
