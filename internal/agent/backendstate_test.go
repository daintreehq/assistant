package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/models"
)

// fakeStateStore records the durable mirror writes so the test can prove the token was
// cleared on disk as well as in memory.
type fakeStateStore struct {
	last  string
	calls int
}

func (f *fakeStateStore) PutSessionBackendState(_ string, state string) error {
	f.last = state
	f.calls++
	return nil
}

// DropBackendState exists for the endpoint switch: the token is server-SIGNED and
// endpoint-specific, so replaying one issued by the deployed backend to a local one
// hands it a token it cannot verify. The durable mirror has to go too, or a handover to
// the supervisor daemon resurrects exactly the token this dropped.
func TestDropBackendStateClearsTokenAndMirror(t *testing.T) {
	store := &fakeStateStore{}
	s := &Session{backendState: "signed-by-the-old-backend"}
	s.deps.SessionID = "ses_test"
	s.deps.BackendStateStore = store

	if err := s.DropBackendState(); err != nil {
		t.Fatalf("DropBackendState: %v", err)
	}
	if s.backendState != "" {
		t.Errorf("in-memory token survived: %q", s.backendState)
	}
	if store.calls != 1 || store.last != "" {
		t.Errorf("durable mirror not cleared: calls=%d last=%q", store.calls, store.last)
	}
}

// The turn gate. A turn is multi-round, and swapping the endpoint between rounds would
// send the next round to a backend that cannot read the state token the previous one
// signed. Refusing must also change NOTHING — a drop that happened and then reported
// failure would leave the session worse off than either outcome alone.
func TestDropBackendStateRefusesMidTurnAndChangesNothing(t *testing.T) {
	store := &fakeStateStore{}
	s := &Session{backendState: "still-valid", inFlight: true}
	s.deps.SessionID = "ses_test"
	s.deps.BackendStateStore = store

	if err := s.DropBackendState(); !errors.Is(err, ErrTurnInProgress) {
		t.Fatalf("want ErrTurnInProgress mid-turn, got %v", err)
	}
	if s.backendState != "still-valid" {
		t.Errorf("a refused drop cleared the token anyway: %q", s.backendState)
	}
	if store.calls != 0 {
		t.Errorf("a refused drop wrote to the durable mirror %d time(s)", store.calls)
	}
}

// A session with no durable store is the offline/test shape; the drop must not panic.
func TestDropBackendStateWithoutAMirror(t *testing.T) {
	s := &Session{backendState: "x"}
	if err := s.DropBackendState(); err != nil {
		t.Fatalf("DropBackendState: %v", err)
	}
	if s.backendState != "" {
		t.Errorf("token survived: %q", s.backendState)
	}
}

// The endpoint swap and the "is a turn running?" check must be ONE act.
//
// DropBackendState alone only proved no turn had started when it was called: it released
// the session, and a turn was free to begin before the client was actually replaced —
// opening against the old endpoint and finishing its later rounds against the new one,
// which is the precise failure the guard exists to prevent, reached by passing the guard
// rather than by bypassing it.
//
// Written as a race: a Send is attempted from another goroutine while `apply` is running,
// and the swap must observe that no turn started underneath it.
func TestSwapBackendExcludesATurnStartingMidSwap(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))

	inApply := make(chan struct{})
	releaseApply := make(chan struct{})
	sendErr := make(chan error, 1)

	go func() {
		_ = s.SwapBackend(func() {
			close(inApply)
			<-releaseApply
		})
	}()

	<-inApply
	go func() {
		_, err := s.Send(context.Background(), "x", SendOptions{})
		sendErr <- err
	}()
	// The Send must still be BLOCKED on the session while apply holds it. If the lock
	// were released between the check and the swap it would already have returned.
	select {
	case err := <-sendErr:
		t.Fatalf("a turn started in the middle of the swap (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseApply)

	select {
	case err := <-sendErr:
		if err != nil {
			t.Fatalf("the turn after the swap returned %v, want it to run", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never ran after the swap released the session")
	}
}

// The other direction: a turn already running refuses the swap, and changes nothing.
func TestSwapBackendRefusesWhileATurnRuns(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))
	s.mu.Lock()
	s.inFlight = true
	s.mu.Unlock()

	applied := false
	if err := s.SwapBackend(func() { applied = true }); err != ErrTurnInProgress {
		t.Fatalf("SwapBackend = %v, want ErrTurnInProgress", err)
	}
	if applied {
		t.Error("the swap ran anyway — a refused switch must change nothing at all")
	}
}

// The picker's reservation blocks turns for as long as the sheet is open, so a choice
// the user makes is always a choice that can be applied. Without it `/backend` could ask
// during a quiet moment, have a wake turn start underneath it, and then refuse the
// answer the user had already given — a question that asked for a decision and did not
// honour it.
func TestReserveEndpointBlocksTurnsUntilReleased(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))

	tok, err := s.ReserveEndpoint()
	if err != nil {
		t.Fatalf("ReserveEndpoint on an idle session: %v", err)
	}
	if _, err := s.Send(context.Background(), "x", SendOptions{}); err != ErrTurnInProgress {
		t.Fatalf("Send during a reservation = %v, want ErrTurnInProgress", err)
	}
	// Two pickers cannot both hold it, or each would believe its answer would apply.
	if _, err := s.ReserveEndpoint(); err != ErrTurnInProgress {
		t.Fatalf("a second reservation = %v, want ErrTurnInProgress", err)
	}
	// A swap by anyone ELSE is refused while a decision is outstanding — an explicit
	// `/backend <target>` must not settle ahead of the picker it raced.
	if err := s.SwapBackend(func() { t.Error("an unreserved swap ran during a reservation") }); err != ErrTurnInProgress {
		t.Fatalf("unreserved SwapBackend = %v, want ErrTurnInProgress", err)
	}
	// …and the holder's own swap still goes through: the reservation exists to
	// guarantee it, not to be worked around.
	applied := false
	if err := s.SwapBackendReserved(tok, func() { applied = true }); err != nil {
		t.Fatalf("SwapBackendReserved under our own token: %v", err)
	}
	if !applied {
		t.Error("the swap did not run under the reservation that exists to guarantee it")
	}

	s.ReleaseEndpoint(tok)
	if _, err := s.Send(context.Background(), "x", SendOptions{}); err != nil {
		t.Fatalf("Send after release = %v, want it to run", err)
	}
}

// A stale release must not unlock somebody else's reservation.
//
// The release is deferred, and a deferred release can outlive what it was written for:
// with a bare flag, the picker that has already finished silently unlocks the NEXT one
// mid-decision. The classic ABA, and the reason the reservation is a token.
func TestReleaseEndpointIgnoresAStaleToken(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))

	first, err := s.ReserveEndpoint()
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	s.ReleaseEndpoint(first)

	second, err := s.ReserveEndpoint()
	if err != nil {
		t.Fatalf("second reservation: %v", err)
	}
	if second == first {
		t.Fatal("reservations reuse their token; a stale release would be indistinguishable")
	}

	// The stale release, arriving late.
	s.ReleaseEndpoint(first)
	if _, err := s.Send(context.Background(), "x", SendOptions{}); err != ErrTurnInProgress {
		t.Fatalf("a stale release unlocked a live reservation (Send = %v)", err)
	}
	// A zero token releases nothing either — that is what a caller with no reservation
	// holds, and it must not be a master key.
	s.ReleaseEndpoint(0)
	if _, err := s.Send(context.Background(), "x", SendOptions{}); err != ErrTurnInProgress {
		t.Fatalf("a zero token unlocked a live reservation (Send = %v)", err)
	}

	s.ReleaseEndpoint(second)
	if _, err := s.Send(context.Background(), "x", SendOptions{}); err != nil {
		t.Fatalf("Send after the real release = %v, want it to run", err)
	}
}

// A turn already running refuses the reservation, so the sheet never opens.
func TestReserveEndpointRefusedDuringATurn(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))
	s.mu.Lock()
	s.inFlight = true
	s.mu.Unlock()
	if _, err := s.ReserveEndpoint(); err != ErrTurnInProgress {
		t.Fatalf("ReserveEndpoint during a turn = %v, want ErrTurnInProgress", err)
	}
}

// A token is only good while it is the LIVE one. The weaker check let any stale token
// commit once its reservation had ended, which is the same unauthorized swap the token
// was introduced to stop.
func TestSwapBackendReservedRefusesAStaleOrWrongToken(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))

	first, err := s.ReserveEndpoint()
	if err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	s.ReleaseEndpoint(first)

	// Nothing is held, so a NON-ZERO token is a lie about holding one.
	if err := s.SwapBackendReserved(first, func() {
		t.Error("a stale token committed a swap after its reservation had ended")
	}); err != ErrTurnInProgress {
		t.Fatalf("stale-token swap = %v, want ErrTurnInProgress", err)
	}

	second, err := s.ReserveEndpoint()
	if err != nil {
		t.Fatalf("second reservation: %v", err)
	}
	// …and the WRONG token cannot commit against a live reservation either.
	if err := s.SwapBackendReserved(first, func() {
		t.Error("a wrong token committed a swap against somebody else's reservation")
	}); err != ErrTurnInProgress {
		t.Fatalf("wrong-token swap = %v, want ErrTurnInProgress", err)
	}
	// The holder's own still works.
	applied := false
	if err := s.SwapBackendReserved(second, func() { applied = true }); err != nil || !applied {
		t.Fatalf("the holder's own swap was refused: err=%v applied=%v", err, applied)
	}
	s.ReleaseEndpoint(second)
}

// Clear and Compact rewrite the history a parked picker's command will return into, and
// a host renders a cleared conversation by wiping its live state — the question sheet
// with it. Admitting either would leave the command parked on a question nobody can now
// see, holding a reservation that refuses every later turn.
func TestClearAndCompactRefuseWhileAnEndpointDecisionIsOpen(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))
	tok, err := s.ReserveEndpoint()
	if err != nil {
		t.Fatalf("ReserveEndpoint: %v", err)
	}

	if err := s.Clear(); err != ErrTurnInProgress {
		t.Errorf("Clear during a reservation = %v, want ErrTurnInProgress", err)
	}
	if err := s.Compact("summary"); err != ErrTurnInProgress {
		t.Errorf("Compact during a reservation = %v, want ErrTurnInProgress", err)
	}

	s.ReleaseEndpoint(tok)
	if err := s.Clear(); err != nil {
		t.Errorf("Clear after release = %v, want it to run", err)
	}
}

// The /compact PRE-CHECK has to mean the same thing Compact means, or it stops
// pre-checking.
//
// It archives the transcript first and lets Compact decide afterwards, so a pre-check
// that is narrower than the real one recreates the multi-megabyte orphan the ordering
// exists to prevent — here, on every /compact attempted while a `/backend` picker is
// open.
func TestCompactWithTranscriptRefusesBeforeArchivingDuringAReservation(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))
	tok, err := s.ReserveEndpoint()
	if err != nil {
		t.Fatalf("ReserveEndpoint: %v", err)
	}

	before := s.Artifacts().Len()
	if err := s.CompactWithTranscript("summary", "a very large transcript"); err != ErrTurnInProgress {
		t.Fatalf("CompactWithTranscript during a reservation = %v, want ErrTurnInProgress", err)
	}
	if got := s.Artifacts().Len(); got != before {
		t.Errorf("the transcript was archived anyway: %d artifacts, was %d", got, before)
	}

	s.ReleaseEndpoint(tok)
	if err := s.CompactWithTranscript("summary", "a very large transcript"); err != nil {
		t.Fatalf("CompactWithTranscript after release = %v, want it to run", err)
	}
}
