package auth

import "testing"

// state_test.go pins the predicates on State against each other.
//
// The enum's danger is that several states LOOK alike to a naive reader and demand
// opposite handling, and the predicates are how a caller is supposed to tell them apart
// without switching on the closed set. A predicate that disagrees with its siblings is
// therefore worse than a missing one: it hands a caller a confident wrong answer.

// Terminal must agree with NeedsPlan. Both subscription states are settled locally —
// nothing this process does moves either — and StateSubscriptionInactive was omitted
// from Terminal for no reason anyone could name.
func TestBothSubscriptionStatesAreTerminal(t *testing.T) {
	for _, s := range []State{StateSubscriptionRequired, StateSubscriptionInactive} {
		if !s.NeedsPlan() {
			t.Fatalf("setup: %q does not need a plan", s)
		}
		if !s.Terminal() {
			t.Errorf("%q is not Terminal — a caller gating on it would retry the one account whose answer is settled", s)
		}
	}
}

// The states that must NOT be terminal, each for its own reason: a retry, a refresh or
// a plain re-ask genuinely moves them.
func TestUnsettledStatesAreNotTerminal(t *testing.T) {
	for _, s := range []State{
		StateUnknown,
		StateAuthorizing,
		StateSignedInUnverified,
		StateSignedInActive,
		StateRefreshing,
		StateTemporarilyUnavailable,
		StateStorageUnavailable,
	} {
		if s.Terminal() {
			t.Errorf("%q reported Terminal — a caller would stop asking about something that changes on its own", s)
		}
	}
}

// Entitlement has exactly one carrier. CanSpend is the question the strict callers ask,
// and the answer must be reachable only from the account projection — never from
// "a protected request worked", which is what MarkIdentityLive knows and all it knows.
func TestOnlyTheEntitledStateCanSpend(t *testing.T) {
	for _, s := range []State{
		StateUnknown, StateSignedOut, StateAuthorizing, StateSignedInUnverified,
		StateSubscriptionRequired, StateSubscriptionInactive, StateRefreshing,
		StateTemporarilyUnavailable, StateRevoked, StateStorageUnavailable,
		StateAccountsUnavailable, StateAccessRefused,
	} {
		if s.CanSpend() {
			t.Errorf("%q reported CanSpend — only a decoded access=granted may say that", s)
		}
	}
	if !StateSignedInActive.CanSpend() {
		t.Errorf("%q cannot spend, which leaves nothing that can", StateSignedInActive)
	}
	// The one mapping that produces it, pinned here so a change to StateForAccess has to
	// be a deliberate one.
	if got := StateForAccess("granted"); got != StateSignedInActive {
		t.Errorf("StateForAccess(granted) = %q, want %q", got, StateSignedInActive)
	}
}
