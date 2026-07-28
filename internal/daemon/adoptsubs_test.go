package daemon

import (
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// optionsWith builds a watcher record carrying persisted per-terminal state.
func optionsWith(t *testing.T, id, status string, per map[string]TerminalState) domain.WatcherRecord {
	t.Helper()
	raw, err := json.Marshal(watcherOptions{PerTerminal: per})
	if err != nil {
		t.Fatalf("marshal options: %v", err)
	}
	rec := watcherWith(id, []string{"term-a"})
	rec.Status = status
	s := string(raw)
	rec.OptionsJson = &s
	return rec
}

// patchedOptions decodes the optionsJson the clear wrote back for a watcher.
func patchedOptions(t *testing.T, store *fakeStore, id string) watcherOptions {
	t.Helper()
	patch, ok := store.watchPatches[id]
	if !ok {
		t.Fatalf("no patch recorded for %s", id)
	}
	raw, _ := patch["optionsJson"].(string)
	var opts watcherOptions
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatalf("optionsJson did not decode: %v (%q)", err, raw)
	}
	return opts
}

// The subscribed latch records PER-PROCESS state (mcp.Client.subs), so ownership
// boot must clear it. Left set, an adopted watcher early-returns from
// ensureSubscribed — never issuing a Subscribe in THIS process — while still
// widening its poll cadence to 30s on the strength of a push channel that will
// never deliver, delaying notice of a finished agent for the whole detached span.
//
// AgentID/ResourceURI must SURVIVE: they are stable identity that lets
// ensureSubscribed re-subscribe without paying for a fresh getStatus read.
func TestClearAdoptedSubscriptionLatches(t *testing.T) {
	store := newFakeStore()
	store.watchers = []domain.WatcherRecord{
		optionsWith(t, "wch_live", "active", map[string]TerminalState{
			"term-a": {
				Subscribed:  true,
				AgentID:     "agt-1",
				ResourceURI: "daintree://agent/agt-1/state",
				SeenWorking: true,
				OutHash:     "abc",
			},
		}),
	}

	if n := ClearAdoptedSubscriptionLatches(store); n != 1 {
		t.Fatalf("expected 1 watcher rewritten, got %d", n)
	}

	st := patchedOptions(t, store, "wch_live").PerTerminal["term-a"]
	if st.Subscribed {
		t.Error("subscribed latch must be cleared at the ownership boundary")
	}
	if st.AgentID != "agt-1" || st.ResourceURI != "daintree://agent/agt-1/state" {
		t.Errorf("subscription IDENTITY must survive so re-subscribe needs no getStatus: %+v", st)
	}
	// The rest of the per-terminal memory is load-bearing too: SeenWorking is the
	// working→waiting completion gate, and dropping it would make an adopted
	// watcher misread a finished agent's "waiting" as a pre-start prompt.
	if !st.SeenWorking || st.OutHash != "abc" {
		t.Errorf("unrelated per-terminal memory must survive: %+v", st)
	}
}

// A watcher with nothing latched must not be rewritten — ownership boot should not
// churn rows (or bump their optionsJson) for no reason.
func TestClearAdoptedSubscriptionLatchesSkipsUnlatched(t *testing.T) {
	store := newFakeStore()
	store.watchers = []domain.WatcherRecord{
		optionsWith(t, "wch_clean", "active", map[string]TerminalState{
			"term-a": {AgentID: "agt-1", SeenWorking: true},
		}),
	}
	if n := ClearAdoptedSubscriptionLatches(store); n != 0 {
		t.Fatalf("expected no rewrite for an unlatched watcher, got %d", n)
	}
	if _, ok := store.watchPatches["wch_clean"]; ok {
		t.Error("an unlatched watcher must not be written at all")
	}
}

// Ended watchers are inert; rewriting them would churn history for no benefit.
func TestClearAdoptedSubscriptionLatchesSkipsEndedWatchers(t *testing.T) {
	store := newFakeStore()
	store.watchers = []domain.WatcherRecord{
		optionsWith(t, "wch_done", "condition_met", map[string]TerminalState{
			"term-a": {Subscribed: true, AgentID: "agt-1"},
		}),
	}
	if n := ClearAdoptedSubscriptionLatches(store); n != 0 {
		t.Fatalf("expected ended watchers to be skipped, got %d", n)
	}
	if _, ok := store.watchPatches["wch_done"]; ok {
		t.Error("an ended watcher must not be rewritten")
	}
}

// Corrupt or absent options must never break ownership boot — the cost of a miss is
// one watcher polling at its normal cadence, which is strictly better than failing
// to take the project lease.
func TestClearAdoptedSubscriptionLatchesTolerateBadRows(t *testing.T) {
	bad := watcherWith("wch_bad", []string{"term-a"})
	corrupt := "{not json"
	bad.OptionsJson = &corrupt

	empty := watcherWith("wch_empty", []string{"term-a"})

	store := newFakeStore()
	store.watchers = []domain.WatcherRecord{bad, empty}

	if n := ClearAdoptedSubscriptionLatches(store); n != 0 {
		t.Fatalf("expected no rewrites, got %d", n)
	}
	if ClearAdoptedSubscriptionLatches(nil) != 0 {
		t.Error("a nil store must be a no-op, not a panic")
	}
}

// Every latched terminal in a multi-target watcher must be cleared — a partial
// clear would leave exactly the phantom-subscription cadence bug for the terminals
// it missed.
func TestClearAdoptedSubscriptionLatchesClearsEveryTerminal(t *testing.T) {
	store := newFakeStore()
	store.watchers = []domain.WatcherRecord{
		optionsWith(t, "wch_multi", "active", map[string]TerminalState{
			"term-a": {Subscribed: true, AgentID: "agt-1"},
			"term-b": {Subscribed: true, AgentID: "agt-2"},
			"term-c": {Subscribed: false, AgentID: "agt-3"},
		}),
	}
	if n := ClearAdoptedSubscriptionLatches(store); n != 1 {
		t.Fatalf("expected the watcher rewritten once, got %d", n)
	}
	per := patchedOptions(t, store, "wch_multi").PerTerminal
	for id, st := range per {
		if st.Subscribed {
			t.Errorf("%s still latched", id)
		}
	}
	if len(per) != 3 {
		t.Errorf("expected all 3 terminals preserved, got %d", len(per))
	}
}
