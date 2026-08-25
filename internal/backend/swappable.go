package backend

import (
	"context"
	"sync/atomic"
)

// Swappable is a Backend whose underlying client can be replaced at runtime, safely,
// while other goroutines are mid-call.
//
// It exists for one reason: the live client must be replaceable WITHOUT a restart. That
// client is captured in a lot of long-lived places — agent.Session's deps, the watcher
// engine, the async coordinator, the workflow layer — and several of those run on their
// own goroutines (the 3s scheduler tick, the 1s coordinator tick, autonomous wake
// turns). Reassigning a plain `App.Backend` field under all that is a data race, and
// threading a guarded accessor through four subsystems would touch every call site.
//
// WHAT SWAPS: `/backend`, which points the session at a different deployment. That is a
// new endpoint AND a new account authority — a credential is minted for one deployment
// and must never be presented to another — so the swap rebuilds the client and the
// manager together (internal/app/backendswitch.go). An ordinary token refresh does NOT
// come through here: TokenSource changes the credential for the same endpoint, one level
// below, so an hourly rotation touches neither the transport nor any consumer's handle.
//
// Instead the app hands out ONE Swappable at construction and never replaces the
// reference. Every consumer keeps the pointer it already has; a swap changes only what
// the wrapper delegates to, so the next call — from any goroutine — lands on the new
// client. Nothing downstream needs to know this happened.
//
// An in-flight call keeps running against the client it started on. That is the correct
// behaviour, not a compromise: a streaming turn cannot be moved to a different endpoint
// halfway through, and cutting it off would corrupt the transcript. The swap takes
// effect from the next call.
type Swappable struct {
	// A pointer-sized atomic load per delegated call: cheaper than a mutex on the hot
	// RespondStream path, and it cannot deadlock against a slow streaming call the way
	// a held RLock could.
	inner atomic.Pointer[Backend]
}

// NewSwappable wraps an initial client. b must not be nil.
func NewSwappable(b Backend) *Swappable {
	s := &Swappable{}
	s.inner.Store(&b)
	return s
}

// Swap replaces the delegate and returns the previous one. Calls already in flight
// finish against the old client; every call after this lands on the new one.
func (s *Swappable) Swap(b Backend) Backend {
	old := s.inner.Swap(&b)
	if old == nil {
		return nil
	}
	return *old
}

// Current returns the delegate, for callers that need the concrete client (diagnostics).
func (s *Swappable) Current() Backend { return *s.inner.Load() }

func (s *Swappable) RespondStream(ctx context.Context, req RespondRequest, cb StreamCallbacks) (RespondResult, error) {
	return s.Current().RespondStream(ctx, req, cb)
}

func (s *Swappable) RunTask(ctx context.Context, req TaskRequest) (TaskResult, error) {
	return s.Current().RunTask(ctx, req)
}

func (s *Swappable) Capabilities(ctx context.Context) (Capabilities, error) {
	return s.Current().Capabilities(ctx)
}

// Account delegates the account-status read.
func (s *Swappable) Account(ctx context.Context) (AccountStatus, error) {
	return (*s.inner.Load()).Account(ctx)
}

func (s *Swappable) VerifyKey(ctx context.Context) (KeyVerification, error) {
	return s.Current().VerifyKey(ctx)
}

func (s *Swappable) Version(ctx context.Context) (Version, error) {
	return s.Current().Version(ctx)
}

func (s *Swappable) Health(ctx context.Context) error { return s.Current().Health(ctx) }

func (s *Swappable) Ready(ctx context.Context) error { return s.Current().Ready(ctx) }

func (s *Swappable) BaseURL() string { return s.Current().BaseURL() }

// Compile-time proof the wrapper stays a drop-in for the interface it fronts.
var _ Backend = (*Swappable)(nil)
