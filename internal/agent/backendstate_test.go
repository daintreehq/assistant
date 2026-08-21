package agent

import (
	"errors"
	"testing"
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
