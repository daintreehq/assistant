package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// pinCapableApp builds an offline App whose backend answers a scripted capability
// descriptor, with `--runbook` pins already named. The counter it returns is what proves
// the preflight's cost: an ordinary launch must not pay a capability GET.
func pinCapableApp(t *testing.T, pins []string, caps backend.Capabilities, capsErr error) (*App, *int32) {
	t.Helper()
	dir := t.TempDir()
	var calls int32
	fb := &fakeBackend{caps: func() (backend.Capabilities, error) {
		atomic.AddInt32(&calls, 1)
		return caps, capsErr
	}}
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 strPtr("operator"),
			WorkflowIntelligence: boolPtr(false),
		},
		BackendOverride: fb,
		PinnedRunbookIDs:  pins,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	return a, &calls
}

// capsWithCatalog is a backend that accepts pins and advertises a catalog.
func capsWithCatalog(refs ...backend.RunbookRef) backend.Capabilities {
	caps := backend.Capabilities{}
	caps.Runbooks.PinnedRunbookIDs = true
	caps.Runbooks.CatalogRevision = "sha256:test"
	// Non-nil even when empty: an advertised empty catalog is a real answer.
	caps.Runbooks.Catalog = append([]backend.RunbookRef{}, refs...)
	return caps
}

// The cost contract. The capability fetch lives in the preflight rather than a boot
// handshake precisely so an ordinary launch keeps its current network profile — if this
// ever regresses, every scripted run in the fleet grows a round trip nobody asked for.
func TestPreparePinnedRunbooksMakesNoCallWithoutPins(t *testing.T) {
	a, calls := pinCapableApp(t, nil, capsWithCatalog(), nil)
	notice, err := a.PreparePinnedRunbooks(context.Background())
	if err != nil || notice != "" {
		t.Fatalf("PreparePinnedRunbooks with no pins = (%q, %v), want a silent no-op", notice, err)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("an unpinned launch fetched capabilities %d time(s); it must fetch none", got)
	}
	if a.PinnedRunbookIDs() != nil {
		t.Fatalf("PinnedRunbookIDs = %v, want nil", a.PinnedRunbookIDs())
	}
}

// A backend that never advertises the capability must not be sent the field (it is
// extra="forbid" server-side, so the turn would 422) AND must not be run against
// silently — an unpinned run that looks pinned is the failure --runbook exists to prevent,
// so refusing to launch is the only honest answer.
func TestPreparePinnedRunbooksRefusesAnUnawareBackend(t *testing.T) {
	a, _ := pinCapableApp(t, []string{"daintree.foundation"}, backend.Capabilities{}, nil)
	_, err := a.PreparePinnedRunbooks(context.Background())
	if err == nil {
		t.Fatal("a backend that does not advertise runbooks.pinned_runbook_ids must fail the launch, not run unpinned")
	}
	if !strings.Contains(err.Error(), "does not accept pinned runbooks") {
		t.Fatalf("error does not name the cause: %v", err)
	}
	// The gate must stay shut regardless — nothing may reach the wire.
	if a.backendAcceptsPinnedRunbookIDs() {
		t.Fatal("gate opened against a backend that advertises nothing")
	}
}

// "We could not ask" is not evidence that pinning works. Proceeding would send the field
// blind (422) or drop it silently; both are worse than a clear failure.
func TestPreparePinnedRunbooksFailsWhenCapabilitiesCannotBeRead(t *testing.T) {
	a, _ := pinCapableApp(t, []string{"x"}, backend.Capabilities{}, errors.New("dial tcp: refused"))
	_, err := a.PreparePinnedRunbooks(context.Background())
	if err == nil {
		t.Fatal("an unreadable capability descriptor must fail a pinned launch")
	}
	if !strings.Contains(err.Error(), "dial tcp: refused") {
		t.Fatalf("error drops the underlying cause: %v", err)
	}
}

// nil catalog (a backend that predates the listing) is NOT an empty catalog. It cannot
// answer, so the id rides on the backend's own warning backstop — advisory, not fatal.
func TestPreparePinnedRunbooksWarnsWhenNoCatalogIsAdvertised(t *testing.T) {
	caps := backend.Capabilities{}
	caps.Runbooks.PinnedRunbookIDs = true // catalog stays nil
	a, _ := pinCapableApp(t, []string{"beta", "alpha"}, caps, nil)

	notice, err := a.PreparePinnedRunbooks(context.Background())
	if err != nil {
		t.Fatalf("a missing catalog is the SERVER's gap and must not fail the launch: %v", err)
	}
	if notice == "" {
		t.Fatal("a pin that could not be checked must say so")
	}
	// Sorted so the advisory is stable regardless of pin order.
	if !strings.Contains(notice, `"alpha", "beta"`) {
		t.Fatalf("advisory does not name the unchecked ids in stable order: %q", notice)
	}
	if !a.backendAcceptsPinnedRunbookIDs() {
		t.Fatal("pinning is supported here; the gate must be open")
	}
}

// An advertised empty catalog IS an answer: this backend loads nothing, so every id is
// wrong. Collapsing it with nil would let a typo through on exactly the deployment that
// could have caught it.
func TestPreparePinnedRunbooksRejectsAgainstAnEmptyCatalog(t *testing.T) {
	a, _ := pinCapableApp(t, []string{"anything"}, capsWithCatalog(), nil)
	if _, err := a.PreparePinnedRunbooks(context.Background()); err == nil {
		t.Fatal("an advertised empty catalog knows every id is unknown; the launch must fail")
	}
}

// The common case the whole feature is for: a typo, caught before a turn is spent, and
// named as a near miss rather than as a run that looked fine.
func TestPreparePinnedRunbooksReportsEveryUnknownIDWithNearMisses(t *testing.T) {
	caps := capsWithCatalog(
		backend.RunbookRef{ID: "daintree.foundation", Title: "Foundation"},
		backend.RunbookRef{ID: "daintree.orchestration.multi-agent", Title: "Multi-agent"},
	)
	a, _ := pinCapableApp(t, []string{"daintree.foundatoin", "totally.unrelated.thing"}, caps, nil)

	_, err := a.PreparePinnedRunbooks(context.Background())
	if err == nil {
		t.Fatal("unknown ids must fail before a turn is spent")
	}
	msg := err.Error()
	// BOTH failures in one message: a caller who mistyped two ids should not have to
	// re-run to discover the second.
	if !strings.Contains(msg, "daintree.foundatoin") || !strings.Contains(msg, "totally.unrelated.thing") {
		t.Fatalf("not every unknown id was reported: %v", msg)
	}
	if !strings.Contains(msg, `did you mean "daintree.foundation"?`) {
		t.Fatalf("a one-transposition typo must produce a near miss: %v", msg)
	}
	// A far-off id must NOT be guessed at — a wrong suggestion sends the reader to fix
	// something that was never the problem.
	if strings.Contains(msg, `"totally.unrelated.thing" (did you mean`) {
		t.Fatalf("a far-off id should not be guessed at: %v", msg)
	}
	if !strings.Contains(msg, "--list-runbooks") {
		t.Fatalf("the error must point at the way to discover the real ids: %v", msg)
	}
}

func TestPreparePinnedRunbooksAcceptsKnownIDs(t *testing.T) {
	caps := capsWithCatalog(
		backend.RunbookRef{ID: "a.one", Title: "One"},
		backend.RunbookRef{ID: "b.two", Title: "Two"},
	)
	a, _ := pinCapableApp(t, []string{"b.two", "a.one"}, caps, nil)
	notice, err := a.PreparePinnedRunbooks(context.Background())
	if err != nil || notice != "" {
		t.Fatalf("known ids = (%q, %v), want a clean pass", notice, err)
	}
	// Order is preserved through Create: the backend admits pins in order and budgets
	// them against its cap, so a reordered list is a different request.
	if got := a.PinnedRunbookIDs(); len(got) != 2 || got[0] != "b.two" || got[1] != "a.one" {
		t.Fatalf("PinnedRunbookIDs = %v, want the launch order [b.two a.one]", got)
	}
}

// Case-sensitive matching, case-insensitive suggestion: the id is the backend's key, so
// a wrong case is a real miss — but it is also exactly the typo a suggestion should catch.
func TestPinnedRunbookIDMatchingIsCaseSensitiveButSuggestsAcrossCase(t *testing.T) {
	caps := capsWithCatalog(backend.RunbookRef{ID: "daintree.foundation", Title: "Foundation"})
	a, _ := pinCapableApp(t, []string{"Daintree.Foundation"}, caps, nil)
	_, err := a.PreparePinnedRunbooks(context.Background())
	if err == nil {
		t.Fatal("a wrong-case id is not the catalog's id and must not silently match")
	}
	if !strings.Contains(err.Error(), `did you mean "daintree.foundation"?`) {
		t.Fatalf("a case-only difference must be suggested: %v", err)
	}
}

// THE race the endpoint pin exists for, in the pin gate this time: a descriptor from
// another endpoint is not evidence about this one. Believed, it would attach a field the
// live backend forbids and 422 every remaining turn.
func TestPinGateDistrustsCapabilitiesFromAnotherEndpoint(t *testing.T) {
	a, _ := pinCapableApp(t, []string{"x"}, capsWithCatalog(backend.RunbookRef{ID: "x"}), nil)
	if _, err := a.PreparePinnedRunbooks(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !a.backendAcceptsPinnedRunbookIDs() {
		t.Fatal("gate should be open after a successful preflight")
	}

	caps := backend.Capabilities{}
	caps.Runbooks.PinnedRunbookIDs = true
	a.backendCaps.Store(&backendCapsSnapshot{baseURL: "http://somewhere-else.test", caps: caps})
	if a.backendAcceptsPinnedRunbookIDs() {
		t.Fatal("an answer from a DIFFERENT endpoint must never open the gate")
	}
}

func TestNormalizePinnedRunbookIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"nil stays nil", nil, nil},
		{"blank-only collapses to nil", []string{"", "  "}, nil},
		{"trims", []string{"  a.one  "}, []string{"a.one"}},
		{"drops exact repeats, keeps first-seen order", []string{"b", "a", "b"}, []string{"b", "a"}},
		{"case is preserved — the id is the backend's key", []string{"A.One"}, []string{"A.One"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizePinnedRunbookIDs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// The App's list must not be mutable through what it hands out — a caller that sorts its
// copy would otherwise silently reorder what every later turn pins.
func TestPinnedRunbookIDsReturnsACopy(t *testing.T) {
	a, _ := pinCapableApp(t, []string{"a", "b"}, capsWithCatalog(), nil)
	got := a.PinnedRunbookIDs()
	got[0] = "mutated"
	if again := a.PinnedRunbookIDs(); again[0] != "a" {
		t.Fatalf("caller mutated the App's pins through the returned slice: %v", again)
	}
}

// The suggestion has to be deterministic — catalog order must not decide which of two
// equally-close ids gets named, or the same typo produces different advice on different
// deployments and the reader cannot tell whether the answer means anything.
func TestNearestRunbookIDIsDeterministicAndBounded(t *testing.T) {
	catalog := []backend.RunbookRef{
		{ID: "zeta.one"},
		{ID: "beta.one"}, // same distance from "xeta.one" as zeta.one; smaller id wins
		{ID: "completely.different.runbook"},
	}
	for _, tc := range []struct {
		name string
		want string
		got  string
	}{
		{"tie breaks lexicographically, not on catalog order", "xeta.one", "beta.one"},
		{"exact-ish match still wins over a tie", "beta.on", "beta.one"},
		// Three edits is past the bound: naming an unrelated runbook sends the reader to
		// fix something that was never the problem.
		{"far-off id is not guessed at", "qqqqqqqqqq", ""},
		{"empty want is not guessed at", "  ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nearestRunbookID(tc.want, catalog); got != tc.got {
				t.Fatalf("nearestRunbookID(%q) = %q, want %q", tc.want, got, tc.got)
			}
		})
	}

	// Reversing the catalog must not change the answer.
	reversed := []backend.RunbookRef{catalog[2], catalog[1], catalog[0]}
	if a, b := nearestRunbookID("xeta.one", catalog), nearestRunbookID("xeta.one", reversed); a != b {
		t.Fatalf("catalog order changed the suggestion: %q vs %q", a, b)
	}

	// A blank catalog entry must be skipped rather than returned as a suggestion — an
	// empty "did you mean" is worse than no suggestion at all.
	if got := nearestRunbookID("x", []backend.RunbookRef{{ID: "   "}}); got != "" {
		t.Fatalf("nearestRunbookID suggested a blank id: %q", got)
	}
}

// `/backend` (App.SetBackendURL) swaps the client in place and deliberately does no
// network work, so the cached capability answer stays pinned to the endpoint that is no
// longer being called. The pin gate must CLOSE on that — believing the old deployment's
// answer would attach a field the new endpoint may forbid and 422 every remaining turn.
//
// This is the one production path that reaches the Session's warn-and-omit branch, so it
// is worth pinning here rather than leaving it to look like defensive dead code.
func TestPinGateClosesAfterAnEndpointSwitch(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 strPtr("operator"),
			WorkflowIntelligence: boolPtr(false),
			BackendURL:           strPtr(backend.LocalBaseURL),
		},
		PinnedRunbookIDs: []string{"a.one"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	// Stand in for a successful preflight against the CURRENT endpoint.
	caps := backend.Capabilities{}
	caps.Runbooks.PinnedRunbookIDs = true
	a.backendCaps.Store(&backendCapsSnapshot{baseURL: a.Backend.BaseURL(), caps: caps})
	if !a.backendAcceptsPinnedRunbookIDs() {
		t.Fatal("gate should be open against the endpoint that answered")
	}

	if _, err := a.SetBackendURL(backend.DefaultBaseURL); err != nil {
		t.Fatalf("SetBackendURL: %v", err)
	}
	if a.Backend.BaseURL() != backend.DefaultBaseURL {
		t.Fatalf("the swap did not take: BaseURL = %q", a.Backend.BaseURL())
	}
	if a.backendAcceptsPinnedRunbookIDs() {
		t.Fatal("the gate stayed open after an endpoint switch; the old deployment's answer is not evidence about the new one")
	}
	// The pins themselves are untouched — they are what the launch asked for, and the
	// Session is what decides (and reports) that they cannot ride this turn.
	if got := a.PinnedRunbookIDs(); len(got) != 1 || got[0] != "a.one" {
		t.Fatalf("PinnedRunbookIDs = %v, want the launch's list preserved across the switch", got)
	}
}
