package app

import (
	"context"
	"errors"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// capableSnapshot is the cached descriptor of a backend that accepts runtime.display,
// filed under the endpoint that answered — the pairing the gate checks.
func capableSnapshot(baseURL string) *backendCapsSnapshot {
	caps := backend.Capabilities{}
	caps.Respond.DisplayContext = true
	return &backendCapsSnapshot{baseURL: baseURL, caps: caps}
}

// The geometry only reaches a turn when BOTH halves hold: a front end measured a real
// terminal, and the live backend said it accepts the block. The gate is not cosmetic —
// the backend validates request.runtime with extra="forbid", so sending the field to a
// deployment that predates it 422s the whole turn before the model runs.
func TestPromptContextWithholdsDisplayFromUnawareBackend(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.SetDisplaySize(120, 97)
	if got := a.PromptContext().Display; got != nil {
		t.Fatalf("geometry sent to a backend that never advertised support: %+v", got)
	}

	// An answered handshake that does NOT advertise the field is still a no: absent and
	// false must behave identically, since an older backend simply omits the key.
	a.backendCaps.Store(&backendCapsSnapshot{baseURL: a.Backend.BaseURL()})
	if got := a.PromptContext().Display; got != nil {
		t.Fatalf("geometry sent on a false capability: %+v", got)
	}

	a.backendCaps.Store(capableSnapshot(a.Backend.BaseURL()))
	got := a.PromptContext().Display
	if got == nil {
		t.Fatal("a capable backend must receive the measured geometry")
	}
	if got.Columns != 120 || got.ContentWidth != 97 {
		t.Fatalf("geometry = %+v, want 120x97", got)
	}
}

// THE race the endpoint pin exists for: a capability fetch that completes AFTER the
// client was replaced describes the OLD deployment. Believed, it would open the gate against
// a backend that rejects the field and 422 every remaining turn of the session — a
// permanent break, not a blip. A descriptor from another endpoint is not evidence about
// this one, so the gate stays closed.
func TestPromptContextDistrustsCapabilitiesFromAnotherEndpoint(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.SetDisplaySize(120, 97)
	a.backendCaps.Store(capableSnapshot("https://some-other-backend.test"))
	if got := a.PromptContext().Display; got != nil {
		t.Fatalf("stale answer from another endpoint opened the gate: %+v", got)
	}

	// Same descriptor, correctly attributed: now it counts.
	a.backendCaps.Store(capableSnapshot(a.Backend.BaseURL()))
	if got := a.PromptContext().Display; got == nil {
		t.Fatal("an answer from the live endpoint must be believed")
	}
}

// An unmeasured surface (piped one-shot, stdio host, daemon) must report nothing rather
// than a fabricated default — "unknown" and "narrow" are different claims, and only the
// backend gets to pick the fallback.
func TestPromptContextOmitsUnmeasuredDisplay(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()
	a.backendCaps.Store(capableSnapshot(a.Backend.BaseURL()))

	if got := a.PromptContext().Display; got != nil {
		t.Fatalf("nothing was measured, yet a geometry was reported: %+v", got)
	}

	// A surface that loses its terminal (width probe fails, host detaches) CLEARS the
	// published value; it must not leave the last good geometry standing as if it were
	// still true.
	a.SetDisplaySize(100, 80)
	a.SetDisplaySize(0, 0)
	if got := a.PromptContext().Display; got != nil {
		t.Fatalf("stale geometry survived an unmeasurable surface: %+v", got)
	}
}

// A resize replaces the pair atomically: a reader can see the old geometry or the new
// one, never a new column count against a stale content width.
func TestSetDisplaySizeReplacesTheWholePair(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.SetDisplaySize(200, 100)
	a.SetDisplaySize(60, 57)
	got := a.DisplaySize()
	if got == nil || got.Columns != 60 || got.ContentWidth != 57 {
		t.Fatalf("geometry = %+v, want the latest 60x57", got)
	}
}

// The whole path, end to end: a measured terminal, a real capability fetch through the
// caching wrapper the boot handshake uses, and a turn that puts the geometry on the wire.
// The per-layer tests above can all pass while the layers fail to meet, which is exactly
// how a capability gate ends up permanently closed and nobody notices.
func TestMeasuredGeometryReachesTheWireOncePermitted(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeBackend{caps: func() (backend.Capabilities, error) {
		caps := backend.Capabilities{}
		caps.Respond.DisplayContext = true
		return caps, nil
	}}
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir, Tier: strPtr("operator"),
			WorkflowIntelligence: boolPtr(false),
		},
		BackendOverride: fake,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	a.SetDisplaySize(120, 97)

	// Before the handshake the backend's answer is unknown, so the turn goes out without
	// the field rather than gambling the whole turn on a 422.
	if _, err := a.Session.Send(context.Background(), "hello", agent.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := fake.lastRequest().Runtime.Display; got != nil {
		t.Fatalf("geometry went out before any handshake: %+v", got)
	}

	if _, err := a.BackendCapabilities(context.Background()); err != nil {
		t.Fatalf("BackendCapabilities: %v", err)
	}
	if _, err := a.Session.Send(context.Background(), "again", agent.SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := fake.lastRequest().Runtime.Display
	if got == nil {
		t.Fatal("a capable backend received no geometry")
	}
	if got.Columns != 120 || got.ContentWidth != 97 {
		t.Fatalf("wire geometry = %+v, want 120x97", got)
	}
}

// A failed capability fetch must not be mistaken for an answer. The cache keeps whatever
// it had, the caller hears about it, and the gate stays where it was — "we could not ask"
// is not evidence that anything changed.
func TestBackendCapabilitiesLeavesTheCacheAloneOnFailure(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.backendCaps.Store(capableSnapshot(a.Backend.BaseURL()))
	a.SetDisplaySize(120, 97)

	// App.Backend is ALWAYS a *backend.Swappable — that is the invariant that lets a
	// consumer capture it once and still follow a later client rebuild. Asserting it
	// here is how this test reaches the delegate, and pins the invariant besides.
	sw, ok := a.Backend.(*backend.Swappable)
	if !ok {
		t.Fatalf("App.Backend must be a *backend.Swappable, got %T", a.Backend)
	}
	swap := sw.Swap(&fakeBackend{caps: func() (backend.Capabilities, error) {
		return backend.Capabilities{}, errors.New("backend unreachable")
	}})
	defer sw.Swap(swap)

	if _, err := a.BackendCapabilities(context.Background()); err == nil {
		t.Fatal("want the fetch error surfaced to the caller")
	}
	// The cached answer is now attributed to a different endpoint than the live one, so
	// the gate closes on its own: an unreachable backend is never assumed compatible.
	if got := a.PromptContext().Display; got != nil {
		t.Fatalf("a failed fetch left the gate open on another endpoint's answer: %+v", got)
	}
}

// Text cannot render wider than the window holding it. The boundary reconciles an
// impossible pair instead of forwarding it, so a miscounted caller cannot make the model
// write for a line that does not exist.
func TestSetDisplaySizeRejectsContentWiderThanTheWindow(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.SetDisplaySize(40, 100)
	got := a.DisplaySize()
	if got == nil || got.ContentWidth != 40 {
		t.Fatalf("geometry = %+v, want the content width capped at the 40-column window", got)
	}
}
