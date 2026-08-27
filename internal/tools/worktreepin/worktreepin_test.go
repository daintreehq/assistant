package worktreepin

import "testing"

// The pin's whole reason to exist is that a worktree switch arriving MID-TURN must not
// move where that turn's agents land.
func TestFirstFreshOfferOfATurnWins(t *testing.T) {
	p := New()
	p.BeginTurn()
	p.Offer("/w/app", "/w/app", "develop", true)
	// The human switches worktrees; the runtime block keeps reporting the live one, so
	// later rounds keep offering. None of them may move the binding.
	p.Offer("/w/other", "/w/other", "feature", true)
	p.Offer("/w/third", "/w/third", "hotfix", true)

	if got := p.ID(); got != "/w/app" {
		t.Errorf("ID after a mid-turn switch = %q, want the turn's first worktree /w/app", got)
	}
	id, path, branch := p.Describe()
	if id != "/w/app" || path != "/w/app" || branch != "develop" {
		t.Errorf("Describe = (%q, %q, %q), want the first offer's full row", id, path, branch)
	}
}

// A STALE snapshot (served from the session's cross-turn cache without being re-read
// this turn) can name the worktree the user just LEFT: the cache TTL is 15s, so
// "switch worktree, then immediately ask" lands inside it. It binds provisionally so
// the turn is never left with nothing, and the fresh read kicked at turn start
// CORRECTS it when it lands.
func TestAFreshOfferUpgradesAStaleBinding(t *testing.T) {
	p := New()
	p.BeginTurn()
	p.Offer("/w/old", "/w/old", "develop", false)
	if got := p.ID(); got != "/w/old" {
		t.Fatalf("a stale offer should still bind provisionally, got %q", got)
	}

	// ID() froze it — a spawn has now used /w/old, so correcting it would split a
	// cohort. Rebind and replay without the read to test the upgrade itself.
	p.BeginTurn()
	p.Offer("/w/old", "/w/old", "develop", false)
	p.Offer("/w/new", "/w/new", "feature", true)
	if got := p.ID(); got != "/w/new" {
		t.Errorf("ID = %q, want the fresh read /w/new to correct the stale binding", got)
	}
}

// Once a spawn has READ the pin, the value is in flight and must not move — a sibling
// in the same concurrently-dispatched cohort reading a different worktree is exactly
// the bug this package exists to prevent.
func TestReadingFreezesAProvisionalBinding(t *testing.T) {
	p := New()
	p.BeginTurn()
	p.Offer("/w/old", "/w/old", "develop", false)
	_ = p.ID() // a spawn consumes it

	p.Offer("/w/new", "/w/new", "feature", true)
	if got := p.ID(); got != "/w/old" {
		t.Errorf("ID = %q, want /w/old — a fresh read arriving after a spawn must not move the target", got)
	}
}

// A stale offer never overwrites another stale one, so the binding cannot drift round
// by round while every read is coming from the same cache.
func TestStaleOffersDoNotOverwriteEachOther(t *testing.T) {
	p := New()
	p.BeginTurn()
	p.Offer("/w/a", "/w/a", "a", false)
	p.Offer("/w/b", "/w/b", "b", false)
	if got := p.ID(); got != "/w/a" {
		t.Errorf("ID = %q, want the first provisional binding /w/a", got)
	}
}

// A round that observed no worktree (a failed read, or no row selected) must leave the
// pin OPEN rather than freezing "unknown" for the rest of the turn.
func TestEmptyOfferDoesNotBind(t *testing.T) {
	p := New()
	p.BeginTurn()
	p.Offer("", "", "", true)
	if got := p.ID(); got != "" {
		t.Fatalf("ID after an empty offer = %q, want \"\"", got)
	}
	p.Offer("/w/app", "/w/app", "develop", true)
	if got := p.ID(); got != "/w/app" {
		t.Errorf("ID = %q, want the first REAL offer to bind", got)
	}
}

// An empty read must NOT freeze the pin, or one unlucky early consult would cost the
// whole turn its binding.
func TestReadingAnUnboundPinDoesNotFreezeIt(t *testing.T) {
	p := New()
	p.BeginTurn()
	if got := p.ID(); got != "" {
		t.Fatalf("unbound ID = %q, want \"\"", got)
	}
	p.Offer("/w/app", "/w/app", "develop", false)
	if got := p.ID(); got != "/w/app" {
		t.Errorf("ID = %q, want a later offer to still bind after an empty read", got)
	}
}

// Each turn rebinds. Without this a session's first worktree would outlive every later
// turn, so switching worktrees between two questions would silently be ignored.
func TestBeginTurnReleasesThePriorBinding(t *testing.T) {
	p := New()
	p.BeginTurn()
	p.Offer("/w/app", "/w/app", "develop", true)
	_ = p.ID() // frozen

	p.BeginTurn()
	if got := p.ID(); got != "" {
		t.Fatalf("ID immediately after BeginTurn = %q, want an unbound pin", got)
	}
	p.Offer("/w/other", "/w/other", "feature", false)
	if got := p.ID(); got != "/w/other" {
		t.Errorf("ID = %q, want the new turn's worktree — BeginTurn must clear the freeze too", got)
	}
}

// Every method is nil-safe so an unwired build (tests, stripped tool sets) degrades to
// the unbound branch, which fails loudly, instead of panicking.
func TestNilPinIsUsable(t *testing.T) {
	var p *Pin
	p.BeginTurn()
	p.Offer("/w/app", "/w/app", "develop", true)
	if got := p.ID(); got != "" {
		t.Errorf("nil pin ID = %q, want \"\"", got)
	}
	if id, path, branch := p.Describe(); id != "" || path != "" || branch != "" {
		t.Errorf("nil pin Describe = (%q, %q, %q), want all empty", id, path, branch)
	}
}
