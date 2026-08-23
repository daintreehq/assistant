package daemon

import (
	"encoding/json"
	"strings"
)

// liveWatcherStatuses mirrors Store.ListLiveWatchers' SQL predicate. The daemon's
// Store seam exposes only ListWatchers(status) (""=all), so the live set is
// filtered here; keep it in lockstep with storage/timers_watchers.go.
var liveWatcherStatuses = map[string]bool{"active": true, "created": true, "paused": true}

// ClearAdoptedSubscriptionLatches resets every live watcher's per-terminal
// `subscribed` latch. It MUST run once at ownership boot, alongside
// Store.BeginOwnership, in whichever process has just taken the project lease.
//
// Why this exists. TerminalState.Subscribed records that a terminal's agent-state
// resource is subscribed, and it is legitimately PERSISTED: optionsJson is a
// watcher's only memory between ticks (each check reloads its state from the
// store), so without persistence ensureSubscribed would re-issue a Subscribe on
// every single tick. But the thing it records — mcp.Client.subs — is PER-PROCESS
// in-memory state. It starts empty in every new process, and resubscribe replays
// only from that map, so a subscription never survives a process boundary.
//
// The mismatch is invisible but costly. After an attached session→daemon handover (or the
// reverse) an adopted watcher reads Subscribed=true, ensureSubscribed early-returns,
// and no Subscribe is ever issued — yet the quiet-subscribed path still widens the
// poll cadence to SubscribedReconcileMS (30s) on the strength of a push channel that
// will never deliver. A background agent that finishes during that span is noticed
// up to 30s late instead of on the push or the 3s tick, for the entire span — in
// exactly the detached window the persistent supervisor exists to cover.
//
// It also corrupts sibling watchers: the adopted watcher's unsubscribeAll calls
// Unsubscribe for a URI it never subscribed to in THIS process, and if another
// watcher here really did subscribe to that terminal, the refcount decrement drops
// it to zero and issues a wire Unsubscribe — silently killing the live watcher's
// real push path.
//
// AgentID/ResourceURI are deliberately PRESERVED: they are stable identity that
// lets ensureSubscribed re-issue the subscription without paying for a fresh
// getStatus read. Only the latch is cleared.
//
// Best-effort by construction — a decode or write failure for one watcher must
// never block ownership boot, so errors are counted, not returned. The cost of a
// miss is a slow cadence for one watcher, never a lost watcher. Returns how many
// watchers were rewritten.
func ClearAdoptedSubscriptionLatches(store Store) int {
	if store == nil {
		return 0
	}
	watchers, err := store.ListWatchers("") // "" = all; filtered to the live set below
	if err != nil {
		return 0
	}
	cleared := 0
	for _, rec := range watchers {
		if !liveWatcherStatuses[rec.Status] {
			continue // an ended watcher's latch is inert — don't rewrite history
		}
		if rec.OptionsJson == nil || strings.TrimSpace(*rec.OptionsJson) == "" {
			continue
		}
		var options watcherOptions
		if err := json.Unmarshal([]byte(*rec.OptionsJson), &options); err != nil {
			continue // corrupt state is the per-tick check's problem, not ours
		}
		changed := false
		for id, st := range options.PerTerminal {
			if st.Subscribed {
				st.Subscribed = false
				options.PerTerminal[id] = st
				changed = true
			}
		}
		if !changed {
			continue
		}
		next, err := json.Marshal(options)
		if err != nil {
			continue
		}
		if err := store.UpdateWatcher(rec.ID, map[string]any{"optionsJson": string(next)}); err != nil {
			continue
		}
		cleared++
	}
	return cleared
}
