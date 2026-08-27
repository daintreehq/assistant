// Package worktreepin holds the ONE worktree a turn is bound to, so every agent
// this turn spawns lands where the turn started rather than wherever the human
// happens to be looking when each launch reaches Daintree.
//
// Daintree resolves an omitted `worktreeId` on `agent.launch` against its LIVE
// active-worktree selection, read at the instant the call lands. That is right
// for a palette pick (the person can see which row is highlighted) and wrong for
// a turn: a fan-out of five explore spawns dispatches as a CONCURRENT cohort, so
// a human switching worktrees mid-batch splits the cohort across two of them,
// and a long turn that spawns after a switch lands somewhere the human never
// asked for. Neither is recoverable by re-reading, because by then the terminals
// exist in the wrong place.
//
// Binding has three states, and the middle one is the whole design:
//
//   - UNBOUND. Nothing has been offered yet.
//   - PROVISIONAL. A STALE snapshot has been offered — one the session served from
//     its TTL'd cross-turn cache without re-reading it this turn. It is a usable
//     answer and usually the right one, but it can predate a worktree switch the
//     user made just before sending, so it must not be the last word.
//   - FROZEN. Either a FRESH snapshot arrived (one actually fetched during this
//     turn, which is authoritative), or something READ the pin — meaning a spawn
//     has already used this value, so changing it now would split a cohort across
//     two worktrees, which is the exact bug this package exists to prevent.
//
// A fresh offer upgrades a provisional binding; a stale one never overwrites
// anything. That is what closes the "switch worktree, then immediately ask" window:
// the session's cache can be up to one TTL old at the top of a turn, so the first
// round's snapshot may still name the worktree the user just LEFT. The detached
// refresh kicked at turn start then lands, the next round offers it as fresh, and
// the binding corrects itself — provided nothing has spawned yet, which is the only
// case where correcting it is still safe.
//
// Freezing on read is not an optimisation. Two members of one concurrently
// dispatched spawn cohort reading different values would put sibling agents in
// different worktrees, which is indistinguishable from the original bug.
//
// Deliberately in-memory and process-scoped, like terminalobs.Memory: a pin that
// outlived the process would be asserting a UI selection nobody has observed since.
//
// An unbound pin reports "". That is NOT a silent fallback any more: Daintree now
// refuses an agent-dispatched launch that names no worktree, so a spawn reaching it
// unbound would be rejected by the host. Consumers must therefore treat "" as "I do
// not know where this turn is" and say so themselves, rather than forwarding an
// omitted id into a guaranteed refusal. An unwired build (tests, stripped tool sets)
// lands in that same branch, which fails loudly instead of silently mis-targeting.
package worktreepin

import "sync"

// Pin is the threadsafe turn-scoped worktree binding. The zero value is usable
// and every method is nil-receiver-safe, so an unwired consumer degrades to the
// pre-pin behaviour instead of panicking.
type Pin struct {
	mu     sync.Mutex
	bound  bool
	frozen bool
	id     string
	path   string
	branch string
}

// New returns an unbound pin.
func New() *Pin { return &Pin{} }

// BeginTurn releases the current binding so the next Offer takes effect. Called
// once at the top of a turn; a turn that offers nothing simply stays unbound.
func (p *Pin) BeginTurn() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bound, p.frozen, p.id, p.path, p.branch = false, false, "", "", ""
}

// Offer proposes the worktree observed this round. `fresh` says whether the
// snapshot was actually fetched during THIS turn, as opposed to served from the
// session's cross-turn cache.
//
// A fresh offer binds and freezes. A stale offer binds only if nothing is bound
// yet, and stays provisional so a fresh one can still correct it. Nothing can
// change a frozen binding — not a later switch, and not a fresh read that arrives
// after a spawn has already consumed the value.
//
// An empty id never binds, so a round that observed no worktree (a failed read, or
// no row selected) leaves the pin open for a later round rather than freezing
// "unknown" for the whole turn.
func (p *Pin) Offer(id, path, branch string, fresh bool) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.frozen || (p.bound && !fresh) {
		return
	}
	p.bound, p.id, p.path, p.branch = true, id, path, branch
	p.frozen = fresh
}

// ID returns the pinned worktree id, or "" when this turn never bound one. A
// non-empty read FREEZES the binding: the caller is about to launch into it, and a
// sibling in the same cohort must not get a different answer.
func (p *Pin) ID() string {
	id, _, _ := p.Describe()
	return id
}

// Describe returns the pinned worktree's id, path and branch, freezing a non-empty
// binding (see ID). Callers that only need the id use ID; this exists so a result
// can NAME where a spawn landed ("branch (id)") without a second MCP read.
func (p *Pin) Describe() (id, path, branch string) {
	if p == nil {
		return "", "", ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bound {
		p.frozen = true
	}
	return p.id, p.path, p.branch
}
