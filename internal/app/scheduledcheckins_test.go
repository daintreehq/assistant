package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
)

// checkinApp builds an offline App over fb, the same way pinCapableApp does, but hands
// the caller the fake so it can read what reached the wire.
func checkinApp(t *testing.T, fb *fakeBackend) *App {
	t.Helper()
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 strPtr("operator"),
			WorkflowIntelligence: boolPtr(false),
		},
		BackendOverride: fb,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	return a
}

func checkinCaps() backend.Capabilities {
	caps := backend.Capabilities{}
	caps.Respond.ScheduledCheckins = true
	return caps
}

// waitNegotiated blocks until the gate's detached negotiation has settled.
func waitNegotiated(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.checkins.mu.Lock()
		done := !a.checkins.inFlight
		a.checkins.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("negotiation never settled")
}

// The gate negotiates on first use — once, detached, never blocking the round that
// asked — and opens for later rounds when the endpoint advertises the block.
func TestScheduledCheckinsGateNegotiatesOnceAndOpens(t *testing.T) {
	a, calls := pinCapableApp(t, nil, checkinCaps(), nil)
	if a.backendAcceptsScheduledCheckins() {
		t.Fatal("the first consult must fail closed while the answer is still in flight")
	}
	waitNegotiated(t, a)
	if !a.backendAcceptsScheduledCheckins() {
		t.Fatal("an endpoint advertising respond.scheduled_checkins must open the gate")
	}
	_ = a.backendAcceptsScheduledCheckins()
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("capabilities fetched %d times, want exactly 1", got)
	}
}

// The negotiated answer stays in the gate's own slot. Publishing it into the shared
// cache would quietly open the display-context and compaction gates on surfaces that
// never negotiated them.
func TestScheduledCheckinsGateDoesNotOpenOtherGates(t *testing.T) {
	caps := checkinCaps()
	caps.Respond.DisplayContext = true
	caps.Runbooks.PinnedRunbookIDs = true
	a, _ := pinCapableApp(t, nil, caps, nil)
	_ = a.backendAcceptsScheduledCheckins()
	waitNegotiated(t, a)
	if a.backendCaps.Load() != nil {
		t.Fatal("the check-in negotiation must not publish into the shared capability cache")
	}
	if a.backendAcceptsDisplayContext() || a.backendAcceptsPinnedRunbookIDs() {
		t.Fatal("the check-in negotiation opened another feature's gate")
	}
}

// A backend that does not advertise the block keeps the gate shut — it would 422 the turn.
func TestScheduledCheckinsGateStaysShutForAnUnawareBackend(t *testing.T) {
	a, calls := pinCapableApp(t, nil, backend.Capabilities{}, nil)
	_ = a.backendAcceptsScheduledCheckins()
	waitNegotiated(t, a)
	if a.backendAcceptsScheduledCheckins() {
		t.Fatal("the gate opened against a backend that advertises nothing")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("a settled 'no' must be remembered, not re-asked: %d calls", got)
	}
}

// A failed ask is not retried on every round: a backend mid-deploy would otherwise be
// asked once per round of every turn while a loop is live.
func TestScheduledCheckinsGateBacksOffAfterAFailure(t *testing.T) {
	a, calls := pinCapableApp(t, nil, backend.Capabilities{}, errors.New("warming up"))
	_ = a.backendAcceptsScheduledCheckins()
	waitNegotiated(t, a)
	for i := 0; i < 5; i++ {
		if a.backendAcceptsScheduledCheckins() {
			t.Fatal("a failed negotiation must leave the gate shut")
		}
	}
	waitNegotiated(t, a)
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("a failed ask was retried inside the backoff: %d calls", got)
	}
}

// An explicit handshake for the same endpoint already answered the question.
func TestScheduledCheckinsGateReadsAnExplicitHandshake(t *testing.T) {
	a, calls := pinCapableApp(t, nil, backend.Capabilities{}, nil)
	a.backendCaps.Store(&backendCapsSnapshot{baseURL: a.Backend.BaseURL(), caps: checkinCaps()})
	if !a.backendAcceptsScheduledCheckins() {
		t.Fatal("a handshake for the live endpoint that advertises the block must open the gate")
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("an answered question must not be asked again: %d calls", got)
	}
}

// End to end through the App wiring: a scheduled repeating message in the store rides
// the turn context once the endpoint has said it accepts the block, and an ordinary
// launch with no such timer never asks.
func TestScheduledCheckinsReachTheWire(t *testing.T) {
	var calls int32
	fb := &fakeBackend{caps: func() (backend.Capabilities, error) {
		atomic.AddInt32(&calls, 1)
		return checkinCaps(), nil
	}}
	a := checkinApp(t, fb)

	if _, err := a.Session.Send(context.Background(), "hello", agent.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("a turn with no scheduled message fetched capabilities %d time(s)", got)
	}
	if rows := fb.lastRequest().Turn.ScheduledCheckins; len(rows) != 0 {
		t.Fatalf("no timers, but rows were sent: %q", rows)
	}

	every := int64(10 * 60 * 1000)
	maxRuns := 30
	if _, err := a.Store.InsertTimer(domain.TimerRecord{
		ID: "tmr_loop", Title: "job queue check", FireAt: domain.NowMS() + every, Status: "scheduled",
		PayloadType: "message", PayloadJson: `{"type":"message","message":"check the queue"}`,
		RepeatEveryMs: &every, MaxRuns: &maxRuns, CreatedAt: domain.NowMS(),
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	// The first turn with a loop starts the negotiation; a later one carries the rows.
	if _, err := a.Session.Send(context.Background(), "status?", agent.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitNegotiated(t, a)
	if _, err := a.Session.Send(context.Background(), "status again?", agent.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	rows := fb.lastRequest().Turn.ScheduledCheckins
	if len(rows) != 1 || !strings.HasPrefix(rows[0], `tmr_loop "job queue check"`) || !strings.Contains(rows[0], "runs 0/30") {
		t.Fatalf("scheduled check-in rows = %q", rows)
	}
}

// A backend rollback must close the gate. The lazy probe filed `true` privately; a
// LATER explicit handshake for the same endpoint says `false`. The newer answer wins —
// otherwise the private `true` keeps sending a field the rolled-back backend now 422s
// on every turn. And a still-newer handshake that says `true` again reopens it.
func TestScheduledCheckinsGateFollowsTheNewestAnswerBothWays(t *testing.T) {
	var accepts atomic.Bool
	accepts.Store(true)
	fb := &fakeBackend{caps: func() (backend.Capabilities, error) {
		caps := backend.Capabilities{}
		caps.Respond.ScheduledCheckins = accepts.Load()
		return caps, nil
	}}
	a := checkinApp(t, fb)

	_ = a.backendAcceptsScheduledCheckins()
	waitNegotiated(t, a)
	if !a.backendAcceptsScheduledCheckins() {
		t.Fatal("precondition: the lazy probe should have opened the gate")
	}

	accepts.Store(false) // the deployment rolls back
	if _, err := a.BackendCapabilities(context.Background()); err != nil {
		t.Fatalf("explicit handshake: %v", err)
	}
	if a.backendAcceptsScheduledCheckins() {
		t.Fatal("a newer handshake reporting scheduled_checkins:false must close the gate over the older private true")
	}

	accepts.Store(true) // and forward again
	if _, err := a.BackendCapabilities(context.Background()); err != nil {
		t.Fatalf("explicit handshake: %v", err)
	}
	if !a.backendAcceptsScheduledCheckins() {
		t.Fatal("a still-newer handshake reporting true must reopen the gate")
	}
}

// An explicit handshake that already said `false` for the live endpoint is an answer:
// the gate must not go and negotiate around it.
func TestScheduledCheckinsGateTrustsAnExplicitNo(t *testing.T) {
	a, calls := pinCapableApp(t, nil, checkinCaps(), nil)
	a.backendCaps.Store(&backendCapsSnapshot{baseURL: a.Backend.BaseURL(), caps: backend.Capabilities{}, seq: backendCapsSeq.Add(1)})
	if a.backendAcceptsScheduledCheckins() {
		t.Fatal("an explicit false for the live endpoint must keep the gate shut")
	}
	waitNegotiated(t, a)
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("the gate negotiated around an explicit answer: %d calls", got)
	}
}
