package app

import (
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

// compactionCapableSnapshot is the cached descriptor of a backend advertising the exact
// server-side compaction contract, filed under the endpoint that answered it.
func compactionCapableSnapshot(baseURL string) *backendCapsSnapshot {
	caps := backend.Capabilities{
		ContextCompaction: &backend.ContextCompactionCaps{
			Enabled:             true,
			StreamEvent:         "compaction",
			Delivery:            "before_done",
			AtMostOnce:          true,
			StreamingOnly:       true,
			BestEffort:          true,
			AppendOnly:          true,
			BlockMessageName:    backend.ContextCompactionBlockName,
			Span:                backend.ContextCompactionSpanCaps{Collection: "input.messages", IndexBase: new(int), EndExclusive: true, ExcludesCurrentReply: true},
			TurnIDMatchRequired: true,
			// The backend's default (cap_compaction_block_bytes); 256 KiB is the
			// configurable ceiling, not what a deployment serves.
			MaxBlockContentBytes: 65_536,
		},
	}
	return &backendCapsSnapshot{baseURL: baseURL, caps: caps}
}

// The gate opens only on a verified contract from the endpoint about to be called, and
// hands back the descriptor the block will be measured against.
func TestBackendContextCompactionOpensOnlyOnAVerifiedContract(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	// No handshake yet: nothing is known about this deployment, so nothing is accepted.
	if _, ok := a.backendContextCompaction(); ok {
		t.Fatal("the gate must be closed before any capability answer")
	}

	// An answered handshake that omits the block entirely — an older backend.
	a.backendCaps.Store(&backendCapsSnapshot{baseURL: a.Backend.BaseURL()})
	if _, ok := a.backendContextCompaction(); ok {
		t.Fatal("a backend that never advertised compaction must not open the gate")
	}

	// Advertised but with no compactor wired — the state of every real deployment
	// today. Distinguishable from absent, and equally closed.
	disabled := compactionCapableSnapshot(a.Backend.BaseURL())
	disabled.caps.ContextCompaction.Enabled = false
	a.backendCaps.Store(disabled)
	if _, ok := a.backendContextCompaction(); ok {
		t.Fatal("enabled:false must not open the gate")
	}

	a.backendCaps.Store(compactionCapableSnapshot(a.Backend.BaseURL()))
	caps, ok := a.backendContextCompaction()
	if !ok {
		t.Fatal("a verified contract must open the gate")
	}
	if caps.MaxBlockContentBytes != 65_536 {
		t.Fatalf("the descriptor must come back with the gate, got %+v", caps)
	}
}

// The same endpoint pin the display and pinned-runbook gates carry, and here it guards
// something worse than a failed request. A descriptor that arrives after the client was
// replaced describes the OLD deployment; believed, it would authorise splicing the
// user's own conversation using a contract the LIVE backend never agreed to — and a
// wrong splice is silent and permanent, not a blip.
func TestBackendContextCompactionDistrustsAnotherEndpointsAnswer(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.backendCaps.Store(compactionCapableSnapshot("http://some-other-backend.invalid"))
	if _, ok := a.backendContextCompaction(); ok {
		t.Fatal("a descriptor from another endpoint must not open the gate")
	}
}

// A single unrecognised field closes the gate. The splice arithmetic is built on every
// one of them, so a backend that revised one ships blocks this client would apply
// wrongly — and no compaction is strictly better than a wrong splice.
func TestBackendContextCompactionClosesOnAnyContractDeviation(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	for name, mutate := range map[string]func(*backend.ContextCompactionCaps){
		"inclusive end":    func(c *backend.ContextCompactionCaps) { c.Span.EndExclusive = false },
		"one-based":        func(c *backend.ContextCompactionCaps) { one := 1; c.Span.IndexBase = &one },
		"other block name": func(c *backend.ContextCompactionCaps) { c.BlockMessageName = "summary" },
		"reply in span":    func(c *backend.ContextCompactionCaps) { c.Span.ExcludesCurrentReply = false },
		"no turn id gate":  func(c *backend.ContextCompactionCaps) { c.TurnIDMatchRequired = false },
	} {
		t.Run(name, func(t *testing.T) {
			snap := compactionCapableSnapshot(a.Backend.BaseURL())
			mutate(snap.caps.ContextCompaction)
			a.backendCaps.Store(snap)
			if _, ok := a.backendContextCompaction(); ok {
				t.Fatalf("%s must close the gate", name)
			}
		})
	}
}

// The gate reads only what an EXPLICIT handshake left behind, and a /backend switch
// therefore closes it until something negotiates with the new endpoint. That is the
// documented limit (see backendContextCompaction), and it is the SAFE direction: a block
// spliced on the strength of the old deployment's contract would rewrite this
// conversation's history on a promise nobody made.
func TestBackendSwitchClosesTheCompactionGate(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	before := a.Backend.BaseURL()
	a.backendCaps.Store(compactionCapableSnapshot(before))
	if _, ok := a.backendContextCompaction(); !ok {
		t.Fatal("precondition: the gate should be open for the current endpoint")
	}

	if _, err := a.SetBackendURL("http://127.0.0.1:8473"); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	if a.Backend.BaseURL() == before {
		t.Skip("endpoint did not change in this environment")
	}
	if _, ok := a.backendContextCompaction(); ok {
		t.Error("the previous endpoint's descriptor must not survive the switch")
	}
}
